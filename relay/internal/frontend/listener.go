package frontend

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Accept-error backoff bounds. The same shape net/http uses for its own
// temporary errors: start small so a blip costs nothing, cap it so a machine
// that stays out of sockets is asked again every second rather than spun on.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

// Listen opens a frontend's port. The listener it returns is the one every
// frontend serves from, and it differs from a plain one in two ways that only
// matter under load.
//
// An accept error does not end it. Running out of descriptors (EMFILE), of
// socket buffers (WSAENOBUFS on Windows), or having a client reset before the
// accept completed are conditions of the moment: the listening socket is fine
// and the next accept usually works. Returning the error would end the
// frontend, and a frontend ending stops the whole relay - every session it is
// carrying, the node, the lot - over a condition a few milliseconds would have
// cleared. So the error is logged and retried with a backoff, and Accept only
// fails once the listener has actually been closed.
//
// With maxConns above zero, no more than that many accepted connections are
// open at once. A connection beyond the limit is not accepted until one closes,
// so the excess waits in the kernel's backlog instead of in this process.
func Listen(ctx context.Context, addr string, maxConns int, log *slog.Logger, name string) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return newLimitedListener(ln, maxConns, log, name), nil
}

type limitedListener struct {
	net.Listener
	log  *slog.Logger
	name string

	sem       chan struct{} // nil when unbounded
	closed    chan struct{}
	closeOnce sync.Once
}

func newLimitedListener(ln net.Listener, maxConns int, log *slog.Logger, name string) *limitedListener {
	l := &limitedListener{Listener: ln, log: log, name: name, closed: make(chan struct{})}
	if maxConns > 0 {
		l.sem = make(chan struct{}, maxConns)
	}
	return l
}

func (l *limitedListener) Accept() (net.Conn, error) {
	if l.sem != nil {
		select {
		case l.sem <- struct{}{}:
		case <-l.closed:
			return nil, net.ErrClosed
		}
	}
	backoff := time.Duration(0)
	for {
		conn, err := l.Listener.Accept()
		if err == nil {
			if l.sem == nil {
				return conn, nil
			}
			return &slotConn{Conn: conn, release: l.release}, nil
		}
		if l.isClosed() || errors.Is(err, net.ErrClosed) {
			l.release()
			return nil, net.ErrClosed
		}
		if backoff == 0 {
			backoff = acceptBackoffMin
		} else {
			backoff = min(backoff*2, acceptBackoffMax)
		}
		l.log.Warn("accept failed, retrying", "frontend", l.name, "err", err, "retry_in", backoff)
		select {
		case <-time.After(backoff):
		case <-l.closed:
			l.release()
			return nil, net.ErrClosed
		}
	}
}

func (l *limitedListener) release() {
	if l.sem != nil {
		<-l.sem
	}
}

func (l *limitedListener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

func (l *limitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// slotConn gives its connection-limit slot back when it is closed, however
// many times that happens and whoever does it - the handler, net/http, or the
// kill switch tearing the stream down.
type slotConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *slotConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// CloseWrite passes a half-close through to the socket underneath, which
// Splice relies on to tell the peer one direction has finished.
func (c *slotConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
