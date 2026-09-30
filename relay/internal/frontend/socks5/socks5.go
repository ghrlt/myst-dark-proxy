// Package socks5 serves the SOCKS5 frontend on a fixed port.
//
// Target host names are forwarded to the exit node verbatim (ATYP=DOMAIN) and
// are never resolved locally, so the relay cannot leak DNS even when the tunnel
// is up and healthy.
package socks5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/socks"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
)

// Server is the SOCKS5 frontend.
type Server struct {
	frontend.Base
	Username string
	Password string
	// HandshakeTimeout bounds how long a client may take to complete the SOCKS5
	// negotiation before it is dropped.
	HandshakeTimeout time.Duration
}

// New builds a SOCKS5 frontend.
func New(base frontend.Base, username, password string) *Server {
	return &Server{Base: base, Username: username, Password: password, HandshakeTimeout: 30 * time.Second}
}

// Name identifies the frontend.
func (s *Server) Name() string { return "socks5" }

// Addr is the configured listen address.
func (s *Server) Addr() string { return s.Listen }

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	return s.Accept(ctx, s.Name(), s.handle)
}

func (s *Server) handle(ctx context.Context, client net.Conn) {
	log := s.Log.With("frontend", "socks5", "client", client.RemoteAddr().String())

	_ = client.SetDeadline(time.Now().Add(s.HandshakeTimeout))
	if err := s.negotiate(client); err != nil {
		log.Debug("handshake failed", "err", err)
		return
	}

	target, err := s.readRequest(client)
	if err != nil {
		var re *requestError
		if errors.As(err, &re) {
			_ = reply(client, re.code, socks.Addr{IP: net.IPv4zero})
		}
		log.Debug("request failed", "err", err)
		return
	}
	_ = client.SetDeadline(time.Time{})

	sess, upstream, err := s.Enter(ctx, target.String())
	if err != nil {
		code := byte(socks.RepGeneralFailure)
		switch {
		case gate.IsClosed(err):
			// Kill switch: refuse loudly rather than dial around the tunnel.
			code = socks.RepNotAllowed
			log.Warn("refused by kill switch", "target", target.String(), "err", err)
		case errors.Is(err, tunnel.ErrTunnelDown):
			code = socks.RepNetworkUnreach
			log.Warn("refused, tunnel down", "target", target.String())
		default:
			code = socks.RepHostUnreach
			log.Info("upstream dial failed", "target", target.String(), "err", err)
		}
		_ = reply(client, code, socks.Addr{IP: net.IPv4zero})
		return
	}
	defer sess.Close()
	defer upstream.Close()

	// BND.ADDR is reported as 0.0.0.0: the real bound address belongs to the
	// exit node and telling the client about it would leak topology.
	if err := reply(client, socks.RepSuccess, socks.Addr{IP: net.IPv4zero}); err != nil {
		return
	}

	log.Debug("stream open", "target", target.String())
	frontend.Splice(s.Log, sess.Context, client, upstream, s.IdleTimeout)
}

func (s *Server) negotiate(client net.Conn) error {
	var head [2]byte
	if _, err := io.ReadFull(client, head[:]); err != nil {
		return err
	}
	if head[0] != socks.Version5 {
		return fmt.Errorf("client spoke SOCKS version %d", head[0])
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(client, methods); err != nil {
		return err
	}

	want := byte(socks.MethodNoAuth)
	if s.Username != "" {
		want = socks.MethodUserPass
	}
	offered := false
	for _, m := range methods {
		if m == want {
			offered = true
			break
		}
	}
	if !offered {
		_, _ = client.Write([]byte{socks.Version5, socks.MethodNoAcceptable})
		return fmt.Errorf("client does not offer method 0x%02x", want)
	}
	if _, err := client.Write([]byte{socks.Version5, want}); err != nil {
		return err
	}
	if want == socks.MethodNoAuth {
		return nil
	}
	return s.checkCredentials(client)
}

func (s *Server) checkCredentials(client net.Conn) error {
	var ver [1]byte
	if _, err := io.ReadFull(client, ver[:]); err != nil {
		return err
	}
	if ver[0] != socks.AuthVersion {
		return fmt.Errorf("bad auth sub-negotiation version %d", ver[0])
	}
	readField := func() (string, error) {
		var l [1]byte
		if _, err := io.ReadFull(client, l[:]); err != nil {
			return "", err
		}
		buf := make([]byte, l[0])
		if _, err := io.ReadFull(client, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	}
	user, err := readField()
	if err != nil {
		return err
	}
	pass, err := readField()
	if err != nil {
		return err
	}
	if user != s.Username || pass != s.Password {
		_, _ = client.Write([]byte{socks.AuthVersion, socks.AuthFailure})
		return errors.New("bad credentials")
	}
	_, err = client.Write([]byte{socks.AuthVersion, socks.AuthSuccess})
	return err
}

type requestError struct {
	code byte
	err  error
}

func (e *requestError) Error() string { return e.err.Error() }

func (s *Server) readRequest(client net.Conn) (socks.Addr, error) {
	var head [3]byte
	if _, err := io.ReadFull(client, head[:]); err != nil {
		return socks.Addr{}, err
	}
	if head[0] != socks.Version5 {
		return socks.Addr{}, &requestError{socks.RepGeneralFailure, fmt.Errorf("bad request version %d", head[0])}
	}
	addr, err := socks.ReadAddr(client)
	if err != nil {
		return socks.Addr{}, &requestError{socks.RepAddrNotSupport, err}
	}
	switch head[1] {
	case socks.CmdConnect:
		return addr, nil
	case socks.CmdUDPAssociate:
		// The myst node's proxy listener carries TCP streams only. Refusing is
		// the safe answer: a client that silently fell back to direct UDP would
		// be exactly the leak this service exists to prevent.
		return socks.Addr{}, &requestError{socks.RepCommandNotSupport, errors.New("UDP ASSOCIATE is not carried by the myst proxy")}
	default:
		return socks.Addr{}, &requestError{socks.RepCommandNotSupport, fmt.Errorf("command 0x%02x", head[1])}
	}
}

func reply(w io.Writer, code byte, bind socks.Addr) error {
	buf := bind.Marshal([]byte{socks.Version5, code, 0x00})
	_, err := w.Write(buf)
	return err
}
