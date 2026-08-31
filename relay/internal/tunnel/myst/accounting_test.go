package myst

import (
	"testing"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
)

// The node's counters restart with every session, so what one session carried
// has to be added to the total rather than replacing it - otherwise a relay
// that has run all day reports whatever its newest exit happens to have moved.
func TestSessionsAddUpRatherThanReplacingEachOther(t *testing.T) {
	var a accounting

	a.begin()
	a.observe(tequilapi.Statistics{BytesSent: 100, BytesReceived: 900, TokensSpent: "5"})
	live := a.snapshot(1)
	if live.Session.BytesReceived != 900 || live.Total.BytesReceived != 900 {
		t.Fatalf("during the first session: session=%d total=%d, want 900 and 900",
			live.Session.BytesReceived, live.Total.BytesReceived)
	}
	a.end()

	a.begin()
	a.observe(tequilapi.Statistics{BytesSent: 10, BytesReceived: 90, TokensSpent: "2"})
	got := a.snapshot(2)

	if got.Session.BytesSent != 10 || got.Session.BytesReceived != 90 {
		t.Errorf("session: got %d up %d down, want 10 and 90", got.Session.BytesSent, got.Session.BytesReceived)
	}
	if got.Total.BytesSent != 110 || got.Total.BytesReceived != 990 {
		t.Errorf("total: got %d up %d down, want 110 and 990", got.Total.BytesSent, got.Total.BytesReceived)
	}
	if got.Total.Wei != "7" {
		t.Errorf("total spend: got %s wei, want 7", got.Total.Wei)
	}
	if got.Sessions != 2 {
		t.Errorf("sessions: got %d, want 2", got.Sessions)
	}
}

// A session that ended is spent, and must stay in the total; the session half
// of the report goes back to nothing rather than showing the last one again.
func TestAClosedSessionLeavesTheTotalAlone(t *testing.T) {
	var a accounting

	a.begin()
	a.observe(tequilapi.Statistics{BytesSent: 7, BytesReceived: 11, TokensSpent: "3"})
	a.end()

	got := a.snapshot(1)
	if got.Session.BytesSent != 0 || got.Session.BytesReceived != 0 || got.Session.Wei != "0" {
		t.Errorf("session after closing: got %+v, want nothing", got.Session)
	}
	if got.Total.BytesSent != 7 || got.Total.BytesReceived != 11 || got.Total.Wei != "3" {
		t.Errorf("total after closing: got %+v, want 7 up, 11 down, 3 wei", got.Total)
	}
}

// A session left open when the next one begins - a supervisor that lost track,
// a path that skipped end - is folded in rather than silently discarded.
func TestAnUnclosedSessionIsStillCounted(t *testing.T) {
	var a accounting

	a.begin()
	a.observe(tequilapi.Statistics{BytesSent: 4, BytesReceived: 6, TokensSpent: "1"})
	a.begin() // no end in between

	if got := a.snapshot(2); got.Total.BytesReceived != 6 || got.Total.Wei != "1" {
		t.Errorf("total: got %+v, want the abandoned session folded in", got.Total)
	}
}

// Counters arriving for no session at all must not be attributed to one.
func TestCountersWithoutASessionAreIgnored(t *testing.T) {
	var a accounting

	a.observe(tequilapi.Statistics{BytesSent: 5, BytesReceived: 5, TokensSpent: "5"})

	if got := a.snapshot(0); got.Total.BytesSent != 0 || got.Total.Wei != "0" {
		t.Errorf("total: got %+v, want nothing", got.Total)
	}
}
