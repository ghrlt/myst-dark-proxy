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
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
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
	Tunnel      tunnel.Tunnel
	Gate        *gate.Gate
	Demand      *demand.Tracker // nil when the session is not tied to clients
	OnDemand    bool
	SessionIdle time.Duration
	Endpoints   []Endpoint
	Log         *slog.Logger
	Started     time.Time
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
	Uptime     string        `json:"uptime"`
	Tunnel     tunnel.Status `json:"tunnel"`
	KillSwitch gate.Stats    `json:"kill_switch"`
	Sessions   Sessions      `json:"sessions"`
	Endpoints  []Endpoint    `json:"endpoints"`
	Healthy    bool          `json:"healthy"`
	Ready      bool          `json:"ready"`
}

func (s *Server) report() Report {
	ts := s.Tunnel.Status()
	gs := s.Gate.Stats()
	healthy := ts.State.Usable() && gs.Open
	return Report{
		Service:    "myst-relay",
		Uptime:     time.Since(s.Started).Truncate(time.Second).String(),
		Tunnel:     ts,
		KillSwitch: gs,
		Sessions:   s.sessions(),
		Endpoints:  s.Endpoints,
		Healthy:    healthy,
		// Armed is the gate shut on purpose with a session coming. Nothing is
		// being carried, and nothing is wrong.
		Ready: healthy || gs.Armed,
	}
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
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
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
