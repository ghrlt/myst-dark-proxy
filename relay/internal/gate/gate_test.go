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

func TestArmedGateMakesCallersWaitUntilItOpens(t *testing.T) {
	g := New(context.Background(), true)
	g.Arm("no session: waiting for a client")

	go func() {
		time.Sleep(30 * time.Millisecond)
		g.Open()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := g.WaitOpen(ctx); err != nil {
		t.Fatalf("armed gate refused a caller instead of waiting for the session: %v", err)
	}
	if _, err := g.Enter(); err != nil {
		t.Fatalf("gate opened but refused the caller it kept waiting: %v", err)
	}
}

// A tunnel that has failed is not a tunnel that is coming: the honest answer is
// an immediate refusal, not a minute of hope. This is what keeps the kill
// switch's advertised reaction time true.
func TestClosedGateRefusesWaitersImmediately(t *testing.T) {
	g := New(context.Background(), true)
	g.Close("tunnel down")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := g.WaitOpen(ctx)
	if !IsClosed(err) {
		t.Fatalf("closed gate did not refuse: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("closed gate made a caller wait %s before refusing", time.Since(start))
	}
	if s := g.Stats(); s.Refused == 0 {
		t.Fatal("a refused waiter was not counted")
	}
}

// The first provider tried is often not the one that works. A waiter released
// by that first failure would be refused moments before the retry that
// succeeds - and it is the only client actually waiting for this session.
func TestWaitingSurvivesAFailedAttempt(t *testing.T) {
	g := New(context.Background(), true)
	g.Arm("opening a session")

	go func() {
		time.Sleep(20 * time.Millisecond)
		g.Close("connect failed")
		time.Sleep(20 * time.Millisecond)
		g.Arm("retrying")
		time.Sleep(20 * time.Millisecond)
		g.Open()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := g.WaitOpen(ctx); err != nil {
		t.Fatalf("waiter was dropped by a failed attempt instead of waiting for the retry: %v", err)
	}
}

func TestWaitOpenGivesUpWithoutAdmitting(t *testing.T) {
	g := New(context.Background(), true)
	g.Arm("no session")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.WaitOpen(ctx); !IsClosed(err) {
		t.Fatalf("a waiter that ran out of patience must be refused, got %v", err)
	}
	if s := g.Stats(); s.Admitted != 0 {
		t.Fatalf("waiting admitted %d stream(s) on its own", s.Admitted)
	}
}
