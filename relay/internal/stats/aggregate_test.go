package stats

import (
	"testing"
	"time"
)

func TestAggregateBucketsShortSpansByTheirEnd(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	records := []Record{
		// 3 hours ago: falls in an earlier hour bucket of the "day" window.
		{Kind: "session", At: now.Add(-3 * time.Hour), Start: now.Add(-3*time.Hour - 10*time.Minute), BytesSent: 1000, BytesReceived: 2000, Wei: "100"},
		// 30 minutes ago: falls in the most recent hour bucket.
		{Kind: "session", At: now.Add(-30 * time.Minute), Start: now.Add(-40 * time.Minute), BytesSent: 50, BytesReceived: 50, Wei: "5"},
		{Kind: "conn", At: now.Add(-30 * time.Minute), Admitted: 4, Refused: 1},
		// Outside the 24h window entirely.
		{Kind: "session", At: now.Add(-48 * time.Hour), BytesSent: 999999, BytesReceived: 999999, Wei: "999"},
	}

	buckets := Aggregate(records, Day, now)
	if len(buckets) != 24 {
		t.Fatalf("got %d buckets, want 24", len(buckets))
	}

	var totalSent, totalRecv, totalAdmitted, totalRefused uint64
	for _, b := range buckets {
		totalSent += b.BytesSent
		totalRecv += b.BytesReceived
		totalAdmitted += b.Admitted
		totalRefused += b.Refused
	}
	if totalSent != 1050 || totalRecv != 2050 {
		t.Errorf("totals = sent %d recv %d, want sent 1050 recv 2050 (the 48h-old record must be excluded)", totalSent, totalRecv)
	}
	if totalAdmitted != 4 || totalRefused != 1 {
		t.Errorf("total admitted/refused = %d/%d, want 4/1", totalAdmitted, totalRefused)
	}

	// Both sessions are short next to an hour-wide bucket, so each should
	// land almost entirely in the single bucket containing its end.
	last := buckets[len(buckets)-1]
	if last.BytesSent != 50 {
		t.Errorf("last bucket sent %d, want the 30-minutes-ago session's 50", last.BytesSent)
	}
}

func TestAggregateSplitsALongSessionAcrossItsWholeSpan(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	// A 4-day session ending now, carrying 400 bytes in each direction and
	// costing 400 wei - a round number per day, so exact-division bars are
	// easy to check.
	records := []Record{
		{Kind: "session", Start: now.Add(-4 * 24 * time.Hour), At: now, BytesSent: 400, BytesReceived: 400, Wei: "400"},
	}

	buckets := Aggregate(records, Week, now)
	if len(buckets) != 7 {
		t.Fatalf("got %d buckets, want 7", len(buckets))
	}

	// The last 4 daily bars, the ones the session's span actually crosses,
	// should each get a quarter of the total - not the whole thing dumped on
	// the final bar the session happened to end in.
	for i, b := range buckets {
		wantCarried := uint64(0)
		if i >= 3 { // the 4 most recent bars
			wantCarried = 200 // 100 sent + 100 received
		}
		if b.Carried() != wantCarried {
			t.Errorf("bucket %d (%s) carried %d, want %d", i, b.Start.Format("Mon"), b.Carried(), wantCarried)
		}
	}

	total := Summarise(records, Week, now)
	if total.Carried() != 800 {
		t.Errorf("Summarise total carried = %d, want 800 (the full session, not a prorated share of it)", total.Carried())
	}
	if total.Sessions != 1 {
		t.Errorf("Summarise sessions = %d, want 1", total.Sessions)
	}
	if total.AvgDuration() != 4*24*time.Hour {
		t.Errorf("Summarise avg duration = %s, want 96h", total.AvgDuration())
	}
}

func TestSummariseCountsASessionOnceRegardlessOfSpan(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	records := []Record{
		// Starts well before the "last 24 hours" window but ends inside it:
		// must still count as exactly one session, averaged by its own full
		// length rather than the slice of it that happens to fall in view.
		{Kind: "session", Start: now.Add(-10 * 24 * time.Hour), At: now.Add(-1 * time.Hour), BytesSent: 1000, BytesReceived: 1000, Wei: "10"},
	}

	total := Summarise(records, Day, now)
	if total.Sessions != 1 {
		t.Fatalf("Sessions = %d, want 1", total.Sessions)
	}
	if total.AvgDuration() != 10*24*time.Hour-time.Hour {
		t.Errorf("AvgDuration = %s, want the session's own full length", total.AvgDuration())
	}
}

func TestAggregateIgnoresASessionEntirelyOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	records := []Record{
		{Kind: "session", Start: now.Add(-30 * 24 * time.Hour), At: now.Add(-20 * 24 * time.Hour), BytesSent: 500, BytesReceived: 500, Wei: "50"},
	}

	buckets := Aggregate(records, Week, now)
	for _, b := range buckets {
		if b.Carried() != 0 {
			t.Errorf("bucket %s carried %d, want 0: the session ended 20 days ago", b.Start, b.Carried())
		}
	}
	if total := Summarise(records, Week, now); total.Sessions != 0 {
		t.Errorf("Summarise sessions = %d, want 0", total.Sessions)
	}
}

func TestAllTimePicksCoarserBucketsForLongerHistory(t *testing.T) {
	now := time.Now()

	recent := []Record{{Kind: "session", At: now.Add(-10 * 24 * time.Hour)}}
	w := AllTime(recent, now)
	if w.Bucket != 24*time.Hour {
		t.Errorf("10 days of history: bucket = %s, want daily", w.Bucket)
	}

	old := []Record{{Kind: "session", At: now.Add(-400 * 24 * time.Hour)}}
	w = AllTime(old, now)
	if w.Bucket != 7*24*time.Hour {
		t.Errorf("400 days of history: bucket = %s, want weekly", w.Bucket)
	}

	ancient := []Record{{Kind: "session", At: now.Add(-3 * 365 * 24 * time.Hour)}}
	w = AllTime(ancient, now)
	if w.Bucket != 30*24*time.Hour {
		t.Errorf("3 years of history: bucket = %s, want monthly", w.Bucket)
	}
}
