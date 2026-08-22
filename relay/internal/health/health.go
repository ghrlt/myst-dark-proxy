// Package health exposes the relay's state over HTTP so the kill switch is
// observable rather than merely asserted: /healthz is 200 only while traffic is
// actually being carried through the tunnel.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

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
	Listen    string
	Tunnel    tunnel.Tunnel
	Gate      *gate.Gate
	Endpoints []Endpoint
	Log       *slog.Logger
	Started   time.Time
}

// Report is the /status document.
type Report struct {
	Service    string        `json:"service"`
	Uptime     string        `json:"uptime"`
	Tunnel     tunnel.Status `json:"tunnel"`
	KillSwitch gate.Stats    `json:"kill_switch"`
	Endpoints  []Endpoint    `json:"endpoints"`
	Healthy    bool          `json:"healthy"`
}

func (s *Server) report() Report {
	ts := s.Tunnel.Status()
	gs := s.Gate.Stats()
	return Report{
		Service:    "myst-relay",
		Uptime:     time.Since(s.Started).Truncate(time.Second).String(),
		Tunnel:     ts,
		KillSwitch: gs,
		Endpoints:  s.Endpoints,
		Healthy:    ts.State.Usable() && gs.Open,
	}
}

// Serve runs the health server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		rep := s.report()
		code := http.StatusServiceUnavailable
		if rep.Healthy {
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
		fprintMetric(w, "myst_relay_killswitch_open", "Whether the kill switch admits traffic.", boolVal(rep.KillSwitch.Open))
		fprintMetric(w, "myst_relay_active_streams", "Streams currently carried.", float64(rep.KillSwitch.Active))
		fprintMetric(w, "myst_relay_admitted_total", "Streams admitted since start.", float64(rep.KillSwitch.Admitted))
		fprintMetric(w, "myst_relay_refused_total", "Streams refused by the kill switch since start.", float64(rep.KillSwitch.Refused))
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
	_, _ = w.Write([]byte("# HELP " + name + " " + help + "\n# TYPE " + name + " gauge\n"))
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
