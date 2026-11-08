// Package httpproxy serves the HTTP frontend on a fixed port: CONNECT for
// tunnelled TLS, and absolute-URI requests for plain HTTP.
package httpproxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
)

// hopByHop headers are consumed by the proxy and never forwarded.
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Server is the HTTP proxy frontend.
type Server struct {
	frontend.Base
	Username string
	Password string

	transport *http.Transport
	once      sync.Once
}

// New builds an HTTP proxy frontend.
func New(base frontend.Base, username, password string) *Server {
	return &Server{Base: base, Username: username, Password: password}
}

// Name identifies the frontend.
func (s *Server) Name() string { return "http" }

// Addr is the configured listen address.
func (s *Server) Addr() string { return s.Listen }

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	s.init()

	srv := &http.Server{
		Addr:              s.Listen,
		Handler:           http.HandlerFunc(s.route),
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// Otherwise a panic net/http recovers from a handler - it recovers
		// every one by default - logs to the standard "log" package's default
		// writer, which is stderr: gone, on a relay with no console to show it.
		ErrorLog: slog.NewLogLogger(s.Log.Handler(), slog.LevelError),
	}

	// net/http retries an accept error only when the error calls itself
	// temporary, and on Windows WSAENOBUFS - the one heavy connection churn
	// produces - does not, so a plain listener would let it end the frontend
	// and the relay with it. Listen's retries every error, and also applies
	// MaxConns, which this port otherwise had no limit on at all.
	ln, err := frontend.Listen(ctx, s.Listen, s.MaxConns, s.Log, "http")
	if err != nil {
		return err
	}
	s.Log.Info("listening", "frontend", "http", "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) init() {
	s.once.Do(func() {
		s.transport = &http.Transport{
			DialContext:           s.dial,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          64,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   20 * time.Second,
			ExpectContinueTimeout: time.Second,
			// Every hop leaves through the tunnel; there is no proxy-of-a-proxy
			// and no environment-driven fallback path.
			Proxy: nil,
		}
	})
}

// dial is the only way this frontend reaches the network: kill switch first,
// tunnel second, no third option.
func (s *Server) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	sess, conn, err := s.Enter(ctx, addr)
	if err != nil {
		return nil, err
	}
	return newGatedConn(conn, sess), nil
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="myst-relay"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		s.connect(w, r)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "this port is an HTTP proxy: send absolute-URI requests or CONNECT", http.StatusBadRequest)
		return
	}
	s.forward(w, r)
}

func (s *Server) authorised(r *http.Request) bool {
	if s.Username == "" {
		return true
	}
	const prefix = "Basic "
	h := r.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, prefix))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(user), []byte(s.Username)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(s.Password)) == 1
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	log := s.Log.With("frontend", "http", "client", r.RemoteAddr)
	target := r.URL.Host
	if target == "" {
		target = r.Host
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	sess, upstream, err := s.Enter(r.Context(), target)
	if err != nil {
		writeDialError(w, err, log, target)
		return
	}
	defer sess.Close()
	defer upstream.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hijacker.Hijack()
	if err != nil {
		log.Warn("hijack failed", "err", err)
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	// Anything the client pipelined before the tunnel came up must be replayed.
	if n := buf.Reader.Buffered(); n > 0 {
		if _, err := io.CopyN(upstream, buf, int64(n)); err != nil {
			return
		}
	}

	log.Debug("connect open", "target", target)
	frontend.Splice(s.Log, sess.Context, client, upstream, s.IdleTimeout)
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	log := s.Log.With("frontend", "http", "client", r.RemoteAddr)

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	for _, h := range hopByHop {
		outReq.Header.Del(h)
	}
	if _, ok := outReq.Header["User-Agent"]; !ok {
		// Do not let Go advertise itself on the client's behalf.
		outReq.Header.Set("User-Agent", "")
	}

	resp, err := s.transport.RoundTrip(outReq)
	if err != nil {
		writeDialError(w, err, log, outReq.URL.Host)
		return
	}
	defer resp.Body.Close()

	dst := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	for _, h := range hopByHop {
		dst.Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeDialError(w http.ResponseWriter, err error, log interface {
	Warn(string, ...any)
	Info(string, ...any)
}, target string) {
	switch {
	case gate.IsClosed(err):
		log.Warn("refused by kill switch", "target", target, "err", err)
		http.Error(w, fmt.Sprintf("myst-relay kill switch: %v", err), http.StatusServiceUnavailable)
	case errors.Is(err, tunnel.ErrTunnelDown):
		log.Warn("refused, tunnel down", "target", target)
		http.Error(w, "myst-relay: tunnel is down", http.StatusServiceUnavailable)
	default:
		log.Info("upstream dial failed", "target", target, "err", err)
		http.Error(w, "myst-relay: "+err.Error(), http.StatusBadGateway)
	}
}

// gatedConn ties a pooled connection's lifetime to its kill-switch permit, so
// a strict gate closing tears down keep-alive connections too.
type gatedConn struct {
	net.Conn
	sess *gate.Session
	once sync.Once
	done chan struct{}
}

func newGatedConn(c net.Conn, sess *gate.Session) *gatedConn {
	g := &gatedConn{Conn: c, sess: sess, done: make(chan struct{})}
	go func() {
		select {
		case <-sess.Context.Done():
			_ = g.Conn.Close()
		case <-g.done:
		}
	}()
	return g
}

func (g *gatedConn) Close() error {
	err := g.Conn.Close()
	g.once.Do(func() {
		close(g.done)
		g.sess.Close()
	})
	return err
}
