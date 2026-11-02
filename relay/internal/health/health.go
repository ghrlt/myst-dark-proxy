// Package health exposes the relay's state over HTTP so the kill switch is
// observable rather than merely asserted.
//
// Two questions are answered separately, because with sessions opened on demand
// they stop having the same answer. "Healthy" is the strict one - a session is
// up and the gate is open, so traffic is being carried this instant. "Ready" is
// the operational one - that, or the relay is deliberately holding no session
// and waiting for a client. /healthz reports readiness, because a supervisor
// restarting a relay for resting would be restarting it for working correctly;
// /status and /metrics report both.
//
// Everything under /control is the other direction: asking the relay to do
// something rather than asking it how it is. Those endpoints exist for the
// Windows tray icon and the matching CLI commands, and they are held to a
// tighter rule than the read-only ones - a shared secret from a file only this
// account can read, and loopback callers only - because "change my exit" and
// "stop carrying traffic" are not things a web page the operator happens to be
// visiting should be able to ask for.
package health

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/demand"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
)

// Endpoint describes one exposed frontend for the status document.
type Endpoint struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// Server publishes relay status.
type Server struct {
	Listen      string
	Version     string
	Tunnel      tunnel.Tunnel
	Gate        *gate.Gate
	Demand      *demand.Tracker // nil when the session is not tied to clients
	OnDemand    bool
	SessionIdle time.Duration
	Endpoints   []Endpoint
	Log         *slog.Logger
	Started     time.Time

	// Token is the secret /control callers must present. An empty one leaves
	// the control endpoints off entirely, which is what a deployment with
	// nowhere to keep a secret - the read-only container - gets.
	Token string
	// Stop shuts the relay down. Nil leaves that endpoint off, so a build that
	// has no way to stop itself cleanly does not pretend otherwise.
	Stop func()
}

// controlHeader carries the shared secret. It is a header rather than a query
// parameter so it stays out of anything that logs URLs.
const controlHeader = "X-Relay-Control"

// rotator is implemented by tunnels that can be asked to move to another exit.
// Like sessionCounter it is deliberately optional: a tunnel with one fixed
// endpoint is still a tunnel, it just has nowhere to rotate to.
type rotator interface{ Rotate(reason string) bool }

// meterer is implemented by tunnels that count what they have carried and what
// it cost.
type meterer interface{ Usage() tunnel.Usage }

// exitTyper is implemented by tunnels whose choice of exit can be narrowed to
// certain kinds of address while they run. Like rotator it is optional: a
// tunnel with one fixed endpoint has no choice to narrow.
type exitTyper interface {
	ExitTypes() []string
	SetExitTypes(ctx context.Context, types []string) ([]string, int, error)
}

// countrier is implemented by tunnels whose choice of exit can be narrowed to
// certain countries while they run.
type countrier interface {
	ExitCountries() []string
	SetExitCountries(ctx context.Context, countries []string) ([]string, int, error)
}

// balancer is implemented by tunnels that can say what the identity paying for
// them has left.
type balancer interface {
	Balance(ctx context.Context) (tunnel.Balance, error)
}

// sessionCounter is implemented by tunnels that can say how many sessions they
// have opened. It is deliberately not part of tunnel.Tunnel: a tunnel that
// cannot count still works, it just reports nothing here.
type sessionCounter interface{ Sessions() uint64 }

// Sessions describes the session policy and what it is doing, which is what
// makes the running cost of the relay legible: how many sessions have been
// paid for, and how long the current lull has lasted.
type Sessions struct {
	Mode        string `json:"mode"`
	IdleTimeout string `json:"idle_timeout,omitempty"`
	IdleFor     string `json:"idle_for,omitempty"`
	Opened      uint64 `json:"opened_total"`
}

// Report is the /status document.
type Report struct {
	Service    string        `json:"service"`
	Version    string        `json:"version,omitempty"`
	Uptime     string        `json:"uptime"`
	Tunnel     tunnel.Status `json:"tunnel"`
	KillSwitch gate.Stats    `json:"kill_switch"`
	Sessions   Sessions      `json:"sessions"`
	Usage      *tunnel.Usage `json:"usage,omitempty"`
	// Exit is the part of the exit policy that can be changed while the relay
	// runs, so a client can show what is in force and offer to move it.
	Exit      *ExitPolicy `json:"exit,omitempty"`
	Endpoints []Endpoint  `json:"endpoints"`
	Healthy   bool        `json:"healthy"`
	Ready     bool        `json:"ready"`
	// Control says whether this relay accepts instructions as well as
	// questions, so a client can offer what will work rather than discovering
	// it from a refusal.
	Control bool `json:"control"`
}

func (s *Server) report() Report {
	ts := s.Tunnel.Status()
	gs := s.Gate.Stats()
	healthy := ts.State.Usable() && gs.Open
	rep := Report{
		Service:    "myst-relay",
		Version:    s.Version,
		Uptime:     time.Since(s.Started).Truncate(time.Second).String(),
		Tunnel:     ts,
		KillSwitch: gs,
		Sessions:   s.sessions(),
		Usage:      s.usage(),
		Exit:       s.exit(),
		Endpoints:  s.Endpoints,
		Healthy:    healthy,
		// Armed is the gate shut on purpose with a session coming. Nothing is
		// being carried, and nothing is wrong.
		Ready:   healthy || gs.Armed,
		Control: s.Token != "",
	}
	return rep
}

// ExitPolicy is the part of what the relay will accept as an exit that can be
// moved while it runs.
//
// An empty IPTypes means every kind is acceptable, which is a setting in its
// own right rather than an absent answer, so it is always present. Whether it
// can be changed from here is what Control says.
type ExitPolicy struct {
	IPTypes []string `json:"ip_types"`
	// Countries is empty for every country, and is present only when the
	// tunnel can be narrowed by country.
	Countries *[]string `json:"countries,omitempty"`
}

// exit is the exit policy in force, when the tunnel has one that can be moved.
func (s *Server) exit() *ExitPolicy {
	t, ok := s.Tunnel.(exitTyper)
	if !ok {
		return nil
	}
	types := t.ExitTypes()
	if types == nil {
		types = []string{}
	}
	p := &ExitPolicy{IPTypes: types}
	if c, ok := s.Tunnel.(countrier); ok {
		countries := c.ExitCountries()
		if countries == nil {
			countries = []string{}
		}
		p.Countries = &countries
	}
	return p
}

// usage is what the tunnel has carried and spent, when it keeps count.
func (s *Server) usage() *tunnel.Usage {
	m, ok := s.Tunnel.(meterer)
	if !ok {
		return nil
	}
	u := m.Usage()
	return &u
}

func (s *Server) sessions() Sessions {
	out := Sessions{Mode: "always on"}
	if s.OnDemand {
		out.Mode = "on demand"
	}
	if s.SessionIdle > 0 {
		out.IdleTimeout = s.SessionIdle.String()
	}
	if c, ok := s.Tunnel.(sessionCounter); ok {
		out.Opened = c.Sessions()
	}
	if s.Demand != nil {
		if idle, _ := s.Demand.IdleFor(); idle > 0 {
			out.IdleFor = idle.Truncate(time.Second).String()
		}
	}
	return out
}

// idleSeconds is the metrics view of the same lull: 0 while a stream is being
// carried, which is exactly when a rule like "idle for an hour" should not fire.
func (s *Server) idleSeconds() float64 {
	if s.Demand == nil {
		return 0
	}
	idle, _ := s.Demand.IdleFor()
	return idle.Seconds()
}

// Serve runs the health server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Otherwise a panic net/http recovers from a handler - it recovers
		// every one by default - logs to the standard "log" package's default
		// writer, which is stderr: gone, on a relay with no console to show it.
		ErrorLog: slog.NewLogLogger(s.Log.Handler(), slog.LevelError),
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.Listen)
	if err != nil {
		return err
	}
	s.Log.Info("listening", "frontend", "health", "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handler builds the routing table, kept apart from Serve so the surface can
// be exercised without a socket.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		rep := s.report()
		code := http.StatusServiceUnavailable
		if rep.Ready {
			code = http.StatusOK
		}
		writeJSON(w, code, rep)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.report())
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		rep := s.report()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fprintMetric(w, "myst_relay_tunnel_up", "Whether the egress tunnel is usable.", boolVal(rep.Tunnel.State.Usable()))
		fprintMetric(w, "myst_relay_ready", "Whether the relay is working as configured: carrying traffic, or armed and waiting for a client.", boolVal(rep.Ready))
		fprintMetric(w, "myst_relay_killswitch_open", "Whether the kill switch admits traffic.", boolVal(rep.KillSwitch.Open))
		fprintMetric(w, "myst_relay_killswitch_armed", "Whether the kill switch is shut with a session on its way.", boolVal(rep.KillSwitch.Armed))
		fprintMetric(w, "myst_relay_active_streams", "Streams currently carried.", float64(rep.KillSwitch.Active))
		fprintMetric(w, "myst_relay_session_idle_seconds", "How long nothing has been carried; 0 while a stream is open.", s.idleSeconds())
		fprintCounter(w, "myst_relay_sessions_total", "Mysterium sessions opened since start.", float64(rep.Sessions.Opened))
		fprintCounter(w, "myst_relay_admitted_total", "Streams admitted since start.", float64(rep.KillSwitch.Admitted))
		fprintCounter(w, "myst_relay_refused_total", "Streams refused by the kill switch since start.", float64(rep.KillSwitch.Refused))
		if u := rep.Usage; u != nil {
			fprintCounter(w, "myst_relay_bytes_sent_total", "Bytes sent through the exit since start.", float64(u.Total.BytesSent))
			fprintCounter(w, "myst_relay_bytes_received_total", "Bytes received through the exit since start.", float64(u.Total.BytesReceived))
			fprintCounter(w, "myst_relay_myst_spent_total", "MYST the exits have charged since start, to four decimals.", parseFloat(u.Total.MYST))
		}
	})

	// Instructions rather than questions. All three are behind the same guard,
	// which is also what decides whether they exist at all: a relay with no
	// secret to check callers against answers 404 on every one of them. A
	// tunnel that cannot carry out a particular one - no exit to change, no
	// identity to price - says so with 501 rather than failing vaguely.
	mux.HandleFunc("/control/rotate", s.guard(http.MethodPost, s.rotate))
	mux.HandleFunc("/control/exit-type", s.guard(http.MethodPost, s.exitType))
	mux.HandleFunc("/control/exit-country", s.guard(http.MethodPost, s.exitCountry))
	mux.HandleFunc("/control/balance", s.guard(http.MethodGet, s.balance))
	mux.HandleFunc("/control/stop", s.guard(http.MethodPost, s.stop))

	return mux
}

// guard is the whole of the control surface's access rule.
//
// Three things must hold: the relay was given a secret to check against, the
// caller is on this machine, and the caller knows the secret. The loopback test
// is not the security - a token settles that - but it costs nothing and it
// keeps a relay whose status port was deliberately published (the container
// publishes it to the host) from also publishing its controls.
//
// A request carrying an Origin header is refused whatever it presents: only a
// browser sends one, and a browser asking this relay to change its exit is a
// page the operator is visiting, not the operator.
func (s *Server) guard(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" {
			writeError(w, http.StatusNotFound, "this relay has no control secret, so it takes no instructions")
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed, "use "+method)
			return
		}
		if r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "controls are not reachable from a browser")
			return
		}
		if !loopback(r.RemoteAddr) {
			writeError(w, http.StatusForbidden, "controls are reachable from this machine only")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(controlHeader)), []byte(s.Token)) != 1 {
			writeError(w, http.StatusForbidden, "wrong or missing control secret")
			return
		}
		h(w, r)
	}
}

// rotate moves the relay to a different exit.
func (s *Server) rotate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Tunnel.(rotator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this tunnel has no exit to change")
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "asked for"
	}
	changed := t.Rotate(reason)
	// Not having moved is a state, not a failure: an on-demand relay that is
	// resting has no exit to leave, and the next client picks a fresh one.
	msg := "no session is open; the next one picks a new exit"
	if changed {
		msg = "leaving this exit; a new one is being picked"
	}
	s.Log.Info("exit change requested", "reason", reason, "had_session", changed)
	writeJSON(w, http.StatusAccepted, map[string]any{"changing": changed, "detail": msg})
}

// exitType narrows the exit policy to a kind of exit address, and moves off the
// current exit when it no longer satisfies it.
//
// The move is the reason this is not merely a setting. A relay told to use
// residential exits while it sits on a hosting one has been asked a question
// about the traffic it is carrying now, not about the traffic it will carry
// after whatever session it happens to hold expires.
func (s *Server) exitType(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Tunnel.(exitTyper)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this tunnel has no choice of exit to narrow")
		return
	}
	var types []string
	if raw := r.URL.Query().Get("type"); raw != "" {
		types = strings.Split(raw, ",")
	}

	// Counting what the policy leaves means asking the node for a proposal
	// list, which is the one slow thing behind any of these endpoints.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	applied, matches, err := t.SetExitTypes(ctx, types)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if applied == nil {
		applied = []string{}
	}

	// Only leave the exit when it no longer fits. Rotating off one that does is
	// a session bought for nothing, and the answer says which happened.
	moved := false
	if r0, ok := s.Tunnel.(rotator); ok && !s.exitFits(applied) {
		moved = r0.Rotate("exit type changed")
	}
	s.Log.Info("exit type changed", "ip_types", applied, "providers", matches, "left_exit", moved)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ip_types":  applied,
		"providers": matches,
		"changing":  moved,
	})
}

// exitCountry narrows the exit policy to certain countries, and moves off the
// current exit when it is in none of them. It is exitType for the other half of
// the policy, and held to the same rules.
func (s *Server) exitCountry(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Tunnel.(countrier)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this tunnel has no choice of exit to narrow")
		return
	}
	var countries []string
	if raw := r.URL.Query().Get("country"); raw != "" {
		countries = strings.Split(raw, ",")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	applied, matches, err := t.SetExitCountries(ctx, countries)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if applied == nil {
		applied = []string{}
	}

	moved := false
	if r0, ok := s.Tunnel.(rotator); ok && !s.exitInCountry(applied) {
		moved = r0.Rotate("exit country changed")
	}
	s.Log.Info("exit country changed", "countries", applied, "providers", matches, "left_exit", moved)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"countries": applied,
		"providers": matches,
		"changing":  moved,
	})
}

// exitInCountry reports whether the exit in use is already in one of these
// countries. A relay with no session has nothing to move, so it fits.
func (s *Server) exitInCountry(countries []string) bool {
	if len(countries) == 0 {
		return true
	}
	st := s.Tunnel.Status()
	if !st.State.Usable() {
		return true
	}
	for _, c := range countries {
		if strings.EqualFold(c, st.Country) {
			return true
		}
	}
	return false
}

// exitFits reports whether the exit in use already satisfies the policy. A
// relay with no session has nothing to move and nothing to keep, so it fits.
func (s *Server) exitFits(types []string) bool {
	if len(types) == 0 {
		return true
	}
	st := s.Tunnel.Status()
	if !st.State.Usable() {
		return true
	}
	for _, t := range types {
		if strings.EqualFold(t, st.IPType) {
			return true
		}
	}
	return false
}

// balance answers what the identity has left to spend.
func (s *Server) balance(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Tunnel.(balancer)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this tunnel has no identity to price")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	b, err := t.Balance(ctx)
	if err != nil {
		// The node is the only thing that knows, so it is down or still
		// starting. Say that rather than reporting a balance of nothing.
		writeError(w, http.StatusServiceUnavailable, "the node cannot be asked right now: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// stop shuts the relay down.
//
// The answer is written before anything is torn down, and the shutdown itself
// is left to a moment later: whoever asked deserves to be told it was heard,
// and this handler's own listener is one of the things about to close.
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if s.Stop == nil {
		writeError(w, http.StatusNotImplemented, "this relay cannot stop itself")
		return
	}
	s.Log.Info("stop requested")
	writeJSON(w, http.StatusAccepted, map[string]any{"stopping": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		s.Stop()
	}()
}

// loopback reports whether an address belongs to this machine's own stack.
func loopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// parseFloat renders a decimal string for the metrics page, where a value that
// cannot be parsed is better reported as zero than as a broken exposition.
func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fprintMetric(w http.ResponseWriter, name, help string, value float64) {
	writeMetric(w, name, help, "gauge", value)
}

func fprintCounter(w http.ResponseWriter, name, help string, value float64) {
	writeMetric(w, name, help, "counter", value)
}

func writeMetric(w http.ResponseWriter, name, help, kind string, value float64) {
	_, _ = w.Write([]byte("# HELP " + name + " " + help + "\n# TYPE " + name + " " + kind + "\n"))
	_, _ = w.Write([]byte(name + " " + formatFloat(value) + "\n"))
}

func boolVal(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
