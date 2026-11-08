package tunnel

import (
	"sync"
	"testing"
)

// A watcher that has not read yet must find the latest state waiting for it,
// not the first one it missed: the kill switch acting on a stale "connecting"
// while the tunnel is up would keep clients waiting at a shut gate.
func TestASlowWatcherConvergesOnTheLatestState(t *testing.T) {
	b := NewBroadcaster()
	ch, cancel := b.Watch()
	defer cancel()

	b.Set(Status{State: StateConnecting})
	b.Set(Status{State: StateUp})

	if got := (<-ch).State; got != StateUp {
		t.Fatalf("watcher read %q, want %q", got, StateUp)
	}
}

// Cancelling a watch while a status is being published used to close the
// channel between Set copying it and sending on it, which panics.
func TestCancellingAWatchDuringSetDoesNotPanic(t *testing.T) {
	b := NewBroadcaster()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 2000 {
			s := StateUp
			if i%2 == 0 {
				s = StateConnecting
			}
			b.Set(Status{State: s})
		}
	}()
	go func() {
		defer wg.Done()
		for range 2000 {
			_, cancel := b.Watch()
			cancel()
		}
	}()
	wg.Wait()
}
