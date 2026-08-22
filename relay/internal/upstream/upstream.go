// Package upstream dials through the proxy a myst node publishes in proxy mode.
//
// Despite the flag being called --proxymode and the node's own logs calling it
// a "proxy server", the listener speaks HTTP CONNECT, not SOCKS5. Verified
// against node 1.39.5: a SOCKS5 greeting gets no reply, while CONNECT to
// arbitrary host:port pairs (443, 22, 80, 465, 53) is answered 200 and the
// stream egresses at the provider. So any TCP protocol travels, which is what
// lets the relay promise "anything to anything" above it.
package upstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ErrUnreachable means the node's proxy listener itself could not be reached,
// as opposed to the target refusing. It is the difference between "the tunnel
// is gone" and "that host is down", and the supervisor reacts to only the
// former.
var ErrUnreachable = errors.New("upstream: node proxy unreachable")

// HTTPConnect opens tunnelled TCP streams through an HTTP CONNECT proxy.
type HTTPConnect struct {
	// Address of the proxy, host:port.
	Address string
	// Credentials, optional.
	Username string
	Password string
	// Dial reaches the proxy itself. It is never a route to the wider internet:
	// the relay reaches the node over a container-internal network.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DialContext opens a stream to addr (host:port) through the proxy.
func (h *HTTPConnect) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("upstream: %s is not carried, only TCP", network)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("upstream: target %q is not host:port: %w", addr, err)
	}

	dial := h.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	conn, err := dial(ctx, "tcp", h.Address)
	if err != nil {
		return nil, fmt.Errorf("%w (%s): %w", ErrUnreachable, h.Address, err)
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	br, err := h.handshake(conn, addr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})

	// The proxy should not speak before we do, but if it buffered anything
	// while parsing the response, hand it to the caller rather than drop it.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

func (h *HTTPConnect) handshake(conn net.Conn, target string) (*bufio.Reader, error) {
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	if h.Username != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(h.Username + ":" + h.Password))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("upstream: send CONNECT %s: %w", target, err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("upstream: read CONNECT reply for %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream: node refused CONNECT %s: %s", target, resp.Status)
	}
	return br, nil
}

// Probe reports whether the proxy is listening and willing to open streams. It
// completes the CONNECT handshake to target and closes it immediately, which
// costs a few hundred bytes and catches a listener that accepts TCP but can no
// longer carry traffic.
func (h *HTTPConnect) Probe(ctx context.Context, target string) error {
	conn, err := h.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	return conn.Close()
}

// bufferedConn drains bytes the CONNECT response parser read ahead.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
