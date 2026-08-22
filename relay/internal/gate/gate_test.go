package gate

import (
	"context"
	"testing"
	"time"
)

func TestGateStartsClosed(t *testing.T) {
	g := New(context.Background(), true)
	if _, err := g.Enter(); !IsClosed(err) {
		t.Fatalf("a fresh gate must refuse traffic, got %v", err)
	}
	if s := g.Stats(); s.Refused != 1 || s.Admitted != 0 {
		t.Fatalf("unexpected stats: %+v", s)
	}
}

func TestGateOpenAdmitsAndCloseRefuses(t *testing.T) {
	g := New(context.Background(), true)
	g.Open()

	sess, err := g.Enter()
	if err != nil {
		t.Fatalf("open gate refused traffic: %v", err)
	}
	if g.Stats().Active != 1 {
		t.Fatalf("active stream not counted")
	}

	g.Close("tunnel down")
	if _, err := g.Enter(); !IsClosed(err) {
		t.Fatalf("closed gate admitted traffic: %v", err)
	}

	// Strict mode must tear the established session down, not merely refuse
	// new ones: that is the difference between a kill switch and a lock.
	select {
	case <-sess.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("strict gate did not cancel the established session")
	}

	sess.Close()
	if g.Stats().Active != 0 {
		t.Fatalf("closing a session did not decrement the active count")
	}
}

func TestPermissiveGateLetsEstablishedStreamsDrain(t *testing.T) {
	g := New(context.Background(), false)
	g.Open()

	sess, err := g.Enter()
	if err != nil {
		t.Fatalf("open gate refused traffic: %v", err)
	}
	g.Close("tunnel down")

	select {
	case <-sess.Context.Done():
		t.Fatal("permissive gate cancelled an established session")
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := g.Enter(); !IsClosed(err) {
		t.Fatalf("permissive gate must still refuse new streams, got %v", err)
	}

	// Shutdown is the backstop: nothing survives it.
	g.Shutdown()
	select {
	case <-sess.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel outstanding sessions")
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	g := New(context.Background(), true)
	g.Open()
	sess, err := g.Enter()
	if err != nil {
		t.Fatal(err)
	}
	sess.Close()
	sess.Close()
	if got := g.Stats().Active; got != 0 {
		t.Fatalf("active count corrupted by double close: %d", got)
	}
}
