// Package raw serves the raw frontend: a fixed local port spliced to a fixed
// remote endpoint through the tunnel.
//
// This is the "anything" case — no proxy protocol, no handshake, no client
// configuration. Whatever bytes arrive are carried, so clients that cannot be
// pointed at a proxy (appliances, game clients, legacy binaries) still egress
// through the exit node, and still cannot bypass the kill switch.
package raw

import (
	"context"
	"errors"
	"net"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
)

// Server forwards one fixed port to one fixed target.
type Server struct {
	frontend.Base
	Target string
}

// New builds a raw forwarder.
func New(base frontend.Base, target string) *Server {
	return &Server{Base: base, Target: target}
}

// Name identifies the frontend.
func (s *Server) Name() string { return "raw " + s.Listen + " -> " + s.Target }

// Addr is the configured listen address.
func (s *Server) Addr() string { return s.Listen }

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	return s.Accept(ctx, s.Name(), s.handle)
}

func (s *Server) handle(ctx context.Context, client net.Conn) {
	log := s.Log.With("frontend", "raw", "client", client.RemoteAddr().String(), "target", s.Target)

	sess, upstream, err := s.Enter(ctx, s.Target)
	if err != nil {
		switch {
		case gate.IsClosed(err):
			log.Warn("refused by kill switch", "err", err)
		case errors.Is(err, tunnel.ErrTunnelDown):
			log.Warn("refused, tunnel down")
		default:
			log.Info("upstream dial failed", "err", err)
		}
		// A raw client has no protocol in which to be told why; closing the
		// connection is the refusal.
		return
	}
	defer sess.Close()
	defer upstream.Close()

	log.Debug("stream open")
	frontend.Splice(sess.Context, client, upstream, s.IdleTimeout)
}
