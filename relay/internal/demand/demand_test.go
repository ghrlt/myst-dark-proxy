package demand

import (
	"context"
	"testing"
	"time"
)

func TestIdleOnlyRunsWhileNothingIsCarried(t *testing.T) {
	d := New()

	release := d.Hold()
	time.Sleep(20 * time.Millisecond)
	if idle, _ := d.IdleFor(); idle != 0 {
		t.Fatalf("a stream is open, so the tunnel is not idle; got %s", idle)
	}
	if d.Active() != 1 {
		t.Fatalf("held stream not counted: %d", d.Active())
	}

	release()
	if d.Active() != 0 {
		t.Fatalf("released stream still counted: %d", d.Active())
	}
	time.Sleep(20 * time.Millisecond)
	if idle, _ := d.IdleFor(); idle <= 0 {
		t.Fatal("idle timer did not start when the last stream ended")
	}

	// A leaked double release would make the count go negative and keep the
	// session alive for ever.
	release()
	if d.Active() != 0 {
		t.Fatalf("double release corrupted the count: %d", d.Active())
	}
}

func TestWaitWakesOnTheNextRequest(t *testing.T) {
	d := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, seq := d.IdleFor()
	go func() {
		time.Sleep(20 * time.Millisecond)
		d.Request()
	}()

	got, err := d.Wait(ctx, seq)
	if err != nil {
		t.Fatalf("waiting for a client: %v", err)
	}
	if got == seq {
		t.Fatal("Wait returned the sequence it was told to wait past")
	}
}

// The supervisor reads IdleFor, decides to close the session, tears it down,
// and only then waits. A client arriving inside that window must not be slept
// through: it is the one client guaranteed to be waiting for an answer.
func TestRequestDuringTeardownIsNotMissed(t *testing.T) {
	d := New()
	_, seq := d.IdleFor() // the supervisor's decision point

	d.Request() // a client arrives while the session is being closed

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := d.Wait(ctx, seq); err != nil {
		t.Fatalf("a client that arrived during teardown was slept through: %v", err)
	}
}

func TestWaitReturnsWhenTheRelayStops(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Wait(ctx, 0); err == nil {
		t.Fatal("Wait ignored a cancelled context")
	}
}

// Zero means "wake on anything that ever happened", so a client that beat the
// supervisor to the start line still gets a session.
func TestWaitDoesNotSleepThroughAnEarlierRequest(t *testing.T) {
	d := New()
	d.Request()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := d.Wait(ctx, 0); err != nil {
		t.Fatalf("a request made before the supervisor waited was lost: %v", err)
	}
}
