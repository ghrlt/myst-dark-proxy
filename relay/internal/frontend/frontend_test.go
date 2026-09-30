package frontend

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func listenerAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func dialUp(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

// TestAccept_RecoversPanicInHandler proves that a handler panicking on one
// connection does not take the accept loop, or the process, down with it: the
// next connection is still served.
func TestAccept_RecoversPanicInHandler(t *testing.T) {
	addr := listenerAddr(t)
	b := &Base{Listen: addr, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var handled atomic.Int32
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- b.Accept(ctx, "test", func(_ context.Context, c net.Conn) {
			defer c.Close()
			if handled.Add(1) == 1 {
				panic("boom")
			}
		})
	}()
	dialUp(t, addr)

	// First connection: the handler panics.
	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c1.Close()

	// Second connection: the accept loop must still be alive to take it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c2, err := net.Dial("tcp", addr)
		if err == nil {
			c2.Close()
		}
		if handled.Load() >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("accept loop did not survive a panicking handler: handled=%d", handled.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return after ctx was cancelled")
	}
}

// TestAccept_MaxConnsBoundsConcurrency proves a connection beyond MaxConns
// waits rather than being handled immediately, so concurrency stays bounded no
// matter how many clients arrive at once.
func TestAccept_MaxConnsBoundsConcurrency(t *testing.T) {
	const maxConns = 2
	const clients = 6

	addr := listenerAddr(t)
	b := &Base{Listen: addr, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxConns: maxConns}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	release := make(chan struct{})
	var mu sync.Mutex
	var current, peak int32

	go func() {
		_ = b.Accept(ctx, "test", func(_ context.Context, c net.Conn) {
			defer c.Close()
			mu.Lock()
			current++
			if current > peak {
				peak = current
			}
			mu.Unlock()
			<-release
			mu.Lock()
			current--
			mu.Unlock()
		})
	}()
	dialUp(t, addr)

	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				return
			}
			defer c.Close()
			// Hold the connection open; the server side closes it once handled.
			buf := make([]byte, 1)
			_, _ = c.Read(buf)
		}()
	}

	// Give every dial time to land and queue up against the limit.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	if gotPeak > maxConns {
		t.Fatalf("peak concurrency %d exceeded MaxConns %d", gotPeak, maxConns)
	}

	close(release)
	wg.Wait()
}
