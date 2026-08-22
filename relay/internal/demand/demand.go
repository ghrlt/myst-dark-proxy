// Package demand tracks whether anything actually wants egress right now.
//
// It exists because a Mysterium session is billed per hour as well as per
// gibibyte: a session nobody is using still costs the identity money, and on
// the reference setup an idle hour costs about as much as 130 KiB of traffic.
// A relay that is up all day but used for an hour spends most of its balance
// carrying nothing.
//
// So the frontends announce demand (a client asked for a stream) and hold it
// for as long as they carry one, and the tunnel supervisor uses that to decide
// whether a session should exist at all: it sleeps until the first Request,
// and closes the session again once nothing has been carried for the idle
// timeout. Nothing here weakens the kill switch - a relay with no session is
// closed, not open.
package demand

import (
	"context"
	"sync"
	"time"
)

// Tracker is the demand signal shared by the frontends and the supervisor.
// The zero value is not usable; call New.
type Tracker struct {
	mu     sync.Mutex
	active int
	last   time.Time
	seq    uint64
	change chan struct{} // closed and replaced on every Request
}

// New returns a tracker with no demand recorded.
func New() *Tracker {
	return &Tracker{last: time.Now(), change: make(chan struct{})}
}

// Request records that a client wants egress now and wakes anything waiting in
// Wait. It never blocks: a client asking is not a client being served.
func (t *Tracker) Request() {
	t.mu.Lock()
	t.last = time.Now()
	t.seq++
	close(t.change)
	t.change = make(chan struct{})
	t.mu.Unlock()
}

// Hold marks one stream as being carried, which is what stops the idle timer.
// The returned release must be called when the stream ends; calling it more
// than once is safe, and failing to call it keeps the session alive, so it
// belongs in a defer.
func (t *Tracker) Hold() func() {
	t.mu.Lock()
	t.active++
	t.last = time.Now()
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			t.active--
			t.last = time.Now()
			t.mu.Unlock()
		})
	}
}

// Active is the number of streams currently held.
func (t *Tracker) Active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// IdleFor reports how long there has been no demand - zero while a stream is
// being carried - together with the request sequence that reading observed.
//
// The sequence is the whole point of returning two values: a supervisor that
// decides to close an idle session on the strength of the duration, and then
// waits with the sequence it saw at that instant, cannot miss a client that
// arrives while the session is being torn down.
func (t *Tracker) IdleFor() (time.Duration, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active > 0 {
		return 0, t.seq
	}
	return time.Since(t.last), t.seq
}

// Wait blocks until a Request newer than seq arrives, and returns the sequence
// it woke on. Passing a zero sequence means "wake on any request ever made",
// so a client that arrived before the supervisor got here is not slept through.
func (t *Tracker) Wait(ctx context.Context, seq uint64) (uint64, error) {
	for {
		t.mu.Lock()
		if t.seq != seq {
			current := t.seq
			t.mu.Unlock()
			return current, nil
		}
		change := t.change
		t.mu.Unlock()

		select {
		case <-ctx.Done():
			return seq, ctx.Err()
		case <-change:
		}
	}
}
