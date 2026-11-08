package frontend

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

// flakyListener fails its first few accepts the way a machine short of
// sockets does, then hands out real connections.
type flakyListener struct {
	net.Listener
	mu    sync.Mutex
	fails int
}

func (f *flakyListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	f.mu.Unlock()
	return f.Listener.Accept()
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// An accept that fails for want of descriptors used to end the frontend, and
// a frontend ending stops the relay. It has to be retried instead.
func TestAnAcceptErrorIsRetriedRatherThanEndingTheFrontend(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitedListener(&flakyListener{Listener: raw, fails: 5}, 0, quiet(), "test")
	defer ln.Close()

	go func() {
		if c, err := net.Dial("tcp", raw.Addr().String()); err == nil {
			defer c.Close()
			time.Sleep(time.Second)
		}
	}()

	got := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if c != nil {
			c.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Accept gave up on a transient error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Accept never returned the connection behind the errors")
	}
}

// Closing the listener is the one way Accept is allowed to fail, and it must
// say so in a way the serve loops recognise, even while backing off.
func TestAcceptFailsOnlyOnceClosed(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitedListener(&flakyListener{Listener: raw, fails: 1 << 30}, 0, quiet(), "test")

	got := make(chan error, 1)
	go func() { _, err := ln.Accept(); got <- err }()
	time.Sleep(50 * time.Millisecond)
	_ = ln.Close()
	select {
	case err := <-got:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close returned %v, want net.ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Accept kept retrying after the listener was closed")
	}
}

// With a limit, a connection past it is not accepted until one closes - and
// closing it twice gives back one slot, not two.
func TestTheConnectionLimitHoldsUntilAConnectionCloses(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitedListener(raw, 1, quiet(), "test")
	defer ln.Close()
	addr := raw.Addr().String()

	for range 2 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}

	first, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			second <- c
		}
	}()
	select {
	case <-second:
		t.Fatal("a second connection was accepted past a limit of one")
	case <-time.After(200 * time.Millisecond):
	}

	_ = first.Close()
	_ = first.Close()
	select {
	case c := <-second:
		defer c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("closing the first connection did not free its slot")
	}
	if n := len(ln.sem); n != 1 {
		t.Fatalf("%d slots taken with one connection open", n)
	}
}
