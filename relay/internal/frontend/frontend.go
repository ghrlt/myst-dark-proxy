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

	"github.com/ghrlt/myst-dark-proxy/relay/internal/demand"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/guard"
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

	// Demand, when set, makes sessions follow clients instead of the clock: a
	// client's arrival is what opens a Mysterium session, and the last stream
	// ending is what starts the idle timer that closes it again. Nil keeps the
	// tunnel up permanently, which is simpler and costs by the hour.
	Demand *demand.Tracker
	// ReadyWait bounds how long a client is made to wait while a session is
	// built for it. It applies only while the kill switch is armed - shut with
	// a session coming - so a failed tunnel still refuses immediately.
	ReadyWait time.Duration

	// MaxConns bounds how many connections this frontend serves at once. Each
	// one holds two goroutines and two sockets for as long as it is open, so
	// with nothing bounding it a client (or a lot of ordinary ones under heavy
	// use) can grow that without limit. 0 means unbounded.
	MaxConns int
}

// Accept runs the accept loop on Listen, invoking handle for each client. It
// returns when ctx is cancelled.
//
// Each client is handled on its own goroutine, recovered: one connection
// hitting a bug is that connection failing, not every session this relay is
// carrying. The listener is Listen's: MaxConns bounds it, and an accept error
// is retried rather than ending the frontend - and with it the relay.
func (b *Base) Accept(ctx context.Context, name string, handle func(context.Context, net.Conn)) error {
	ln, err := Listen(ctx, b.Listen, b.MaxConns, b.Log, name)
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
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		guard.Go(b.Log, name+" connection", func() {
			defer wg.Done()
			defer conn.Close()
			handle(ctx, conn)
		})
	}
}

// Splice copies bytes in both directions until either side finishes or the
// session context is cancelled. Cancellation is what makes the strict kill
// switch bite: when the tunnel drops, every in-flight stream is closed here.
// A panic copying one direction is recovered and logged against log rather
// than taking down whatever else this process is carrying.
func Splice(log *slog.Logger, ctx context.Context, a, b net.Conn, idle time.Duration) {
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
	guard.Go(log, "splice", func() { cp(a, b) })
	guard.Go(log, "splice", func() { cp(b, a) })
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
// caller owns the returned session and must close it, and must close the
// returned connection: that is what releases the client's claim on the session
// and lets the idle timer start.
func (b *Base) Enter(ctx context.Context, target string) (*gate.Session, net.Conn, error) {
	release := func() {}
	if b.Demand != nil {
		// Asking is what starts a session when there is none, so this comes
		// before the gate rather than after it.
		b.Demand.Request()
		if b.ReadyWait > 0 {
			waitCtx, cancel := context.WithTimeout(ctx, b.ReadyWait)
			err := b.Gate.WaitOpen(waitCtx)
			cancel()
			if err != nil {
				return nil, nil, err
			}
		}
		release = b.Demand.Hold()
	}

	sess, err := b.Gate.Enter()
	if err != nil {
		release()
		return nil, nil, err
	}
	dialCtx, cancel := context.WithTimeout(sess.Context, b.DialTimeout)
	defer cancel()
	conn, err := b.Dialer.DialContext(dialCtx, "tcp", target)
	if err != nil {
		release()
		sess.Close()
		return nil, nil, err
	}
	if b.Demand == nil {
		return sess, conn, nil
	}
	return sess, &heldConn{Conn: conn, release: release}, nil
}

// heldConn keeps a claim on the tunnel for as long as the stream it carries is
// open. Every frontend closes its upstream connection when the stream ends -
// including when the kill switch tears it down - so this is the one place that
// sees the end of every stream, whichever way it ends.
type heldConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *heldConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
