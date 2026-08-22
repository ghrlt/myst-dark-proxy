// Package frontend holds the listener side of the relay: the fixed ports
// clients connect to. Every frontend is handed the same two collaborators - a
// Dialer (the tunnel) and a Gate (the kill switch) - and is forbidden from
// opening a connection any other way.
package frontend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
)

// Dialer is the egress capability a frontend needs. tunnel.Tunnel satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Frontend is a fixed-port listener.
type Frontend interface {
	Name() string
	Addr() string
	Serve(ctx context.Context) error
}

// Base carries what every frontend needs. Embed it.
type Base struct {
	Listen      string
	Dialer      Dialer
	Gate        *gate.Gate
	Log         *slog.Logger
	DialTimeout time.Duration
	IdleTimeout time.Duration
}

// Accept runs the accept loop on Listen, invoking handle for each client. It
// returns when ctx is cancelled.
func (b *Base) Accept(ctx context.Context, name string, handle func(context.Context, net.Conn)) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", b.Listen)
	if err != nil {
		return err
	}
	b.Log.Info("listening", "frontend", name, "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			handle(ctx, conn)
		}()
	}
}

// Splice copies bytes in both directions until either side finishes or the
// session context is cancelled. Cancellation is what makes the strict kill
// switch bite: when the tunnel drops, every in-flight stream is closed here.
func Splice(ctx context.Context, a, b net.Conn, idle time.Duration) {
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			_ = a.SetDeadline(time.Now())
			_ = b.SetDeadline(time.Now())
			_ = a.Close()
			_ = b.Close()
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		copyWithIdle(dst, src, idle)
		// Half-close so the peer learns the direction is finished instead of
		// waiting for the whole stream to time out.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.SetReadDeadline(time.Now())
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

func copyWithIdle(dst, src net.Conn, idle time.Duration) {
	if idle <= 0 {
		_, _ = io.Copy(dst, src)
		return
	}
	buf := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(idle))
		n, err := src.Read(buf)
		if n > 0 {
			_ = dst.SetWriteDeadline(time.Now().Add(idle))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Enter takes a kill-switch permit and dials target through the tunnel. The
// caller owns the returned session and must close it.
func (b *Base) Enter(ctx context.Context, target string) (*gate.Session, net.Conn, error) {
	sess, err := b.Gate.Enter()
	if err != nil {
		return nil, nil, err
	}
	dialCtx, cancel := context.WithTimeout(sess.Context, b.DialTimeout)
	defer cancel()
	conn, err := b.Dialer.DialContext(dialCtx, "tcp", target)
	if err != nil {
		sess.Close()
		return nil, nil, err
	}
	return sess, conn, nil
}
