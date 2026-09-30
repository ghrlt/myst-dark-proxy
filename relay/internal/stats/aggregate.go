package stats

import (
	"math/big"
	"time"
)

// Window names a lookback period and the span each bar of its chart covers.
// AllTime picks its own bucket size (see below); the rest are fixed, since
// their whole point is a stable, recognisable shape.
type Window struct {
	Label    string
	Lookback time.Duration
	Bucket   time.Duration
}

var (
	Hour  = Window{"last hour", time.Hour, 5 * time.Minute}             // 12 bars
	Day   = Window{"last 24 hours", 24 * time.Hour, time.Hour}          // 24 bars
	Week  = Window{"last 7 days", 7 * 24 * time.Hour, 24 * time.Hour}   // 7 bars
	Month = Window{"last 30 days", 30 * 24 * time.Hour, 24 * time.Hour} // 30 bars
)

// AllTime looks back to the oldest record on hand, bucketed coarsely enough
// that the chart stays readable whether that is a week ago or five years ago.
func AllTime(records []Record, now time.Time) Window {
	earliest := now
	for _, r := range records {
		t := r.At
		if r.Kind == "session" && !r.Start.IsZero() && r.Start.Before(t) {
			t = r.Start
		}
		if t.Before(earliest) {
			earliest = t
		}
	}
	span := now.Sub(earliest)
	bucket := 24 * time.Hour
	switch {
	case span > 2*365*24*time.Hour:
		bucket = 30 * 24 * time.Hour
	case span > 60*24*time.Hour:
		bucket = 7 * 24 * time.Hour
	}
	return Window{"all time", span, bucket}
}

// bucketSize is w.Bucket, defaulting down to something usable when it was
// left unset.
func (w Window) bucketSize() time.Duration {
	switch {
	case w.Bucket > 0:
		return w.Bucket
	case w.Lookback > 0:
		return w.Lookback
	default:
		return 24 * time.Hour
	}
}

// bounds is the [start, end) span the window's bars cover, and how many of
// them there are. end is pinned to now; start is however many whole buckets
// back from it that covers at least Lookback.
func (w Window) bounds(now time.Time) (start, end time.Time, n int) {
	bucket := w.bucketSize()
	n = max(int(w.Lookback/bucket), 1)
	start = now.Add(-time.Duration(n) * bucket)
	end = start.Add(time.Duration(n) * bucket)
	return start, end, n
}

// Bucket is one bar's worth of aggregated history.
type Bucket struct {
	Start         time.Time
	BytesSent     uint64
	BytesReceived uint64
	Admitted      uint64
	Refused       uint64
	Wei           *big.Int
}

// Carried is the figure the "data carried" chart is drawn from: everything
// this bucket moved, in either direction.
func (b Bucket) Carried() uint64 { return b.BytesSent + b.BytesReceived }

// Aggregate buckets records into w's bars, oldest first, for charting bytes,
// cost and connections independently of one another.
//
// A session is a span, not a point, and this relay is not asked to rotate
// exits often - a session can run for days. What it carried and cost is
// split across every bar its span overlaps, in proportion to how much of the
// span fell in each: crediting an entire three-day session to the one hour it
// happened to end in would make the "last 24 hours" chart meaningless on the
// day after a long session closes.
//
// A "conn" tick is a point in time, not a span - it already describes one
// short interval - so it lands in whichever single bar contains it.
func Aggregate(records []Record, w Window, now time.Time) []Bucket {
	start, end, n := w.bounds(now)
	bucketSize := w.bucketSize()

	buckets := make([]Bucket, n)
	for i := range buckets {
		buckets[i].Start = start.Add(time.Duration(i) * bucketSize)
		buckets[i].Wei = new(big.Int)
	}

	for _, r := range records {
		switch r.Kind {
		case "session":
			spanStart, spanEnd := sessionSpan(r)
			if spanEnd.Before(start) || !spanStart.Before(end) {
				continue
			}
			total := spanEnd.Sub(spanStart)
			wei := parseWei(r.Wei)
			for i := range buckets {
				barStart := buckets[i].Start
				barEnd := barStart.Add(bucketSize)
				os, oe := maxTime(spanStart, barStart), minTime(spanEnd, barEnd)
				if !oe.After(os) {
					continue
				}
				frac := float64(oe.Sub(os)) / float64(total)
				buckets[i].BytesSent += uint64(float64(r.BytesSent) * frac)
				buckets[i].BytesReceived += uint64(float64(r.BytesReceived) * frac)
				buckets[i].Wei.Add(buckets[i].Wei, shareOfWei(wei, oe.Sub(os), total))
			}
		case "conn":
			if r.At.Before(start) || r.At.After(end) {
				continue
			}
			i := min(int(r.At.Sub(start)/bucketSize), n-1)
			buckets[i].Admitted += r.Admitted
			buckets[i].Refused += r.Refused
		}
	}
	return buckets
}

// Totals is a window's headline figures, computed straight from the records
// that overlap it rather than by summing Aggregate's bars: a session is
// counted once and averaged by its own full duration, not smeared and
// re-added across however many bars its span happened to cross.
type Totals struct {
	Sessions      uint64
	SessionTime   time.Duration
	BytesSent     uint64
	BytesReceived uint64
	Wei           *big.Int
	Admitted      uint64
	Refused       uint64
}

func (t Totals) Carried() uint64 { return t.BytesSent + t.BytesReceived }

// AvgDuration is zero when there were no sessions, rather than a division by
// zero.
func (t Totals) AvgDuration() time.Duration {
	if t.Sessions == 0 {
		return 0
	}
	return t.SessionTime / time.Duration(t.Sessions)
}

// Summarise totals every record whose span overlaps w at all.
func Summarise(records []Record, w Window, now time.Time) Totals {
	start, end, _ := w.bounds(now)

	out := Totals{Wei: new(big.Int)}
	for _, r := range records {
		switch r.Kind {
		case "session":
			spanStart, spanEnd := sessionSpan(r)
			if spanEnd.Before(start) || spanStart.After(end) {
				continue
			}
			out.Sessions++
			out.SessionTime += spanEnd.Sub(spanStart)
			out.BytesSent += r.BytesSent
			out.BytesReceived += r.BytesReceived
			out.Wei.Add(out.Wei, parseWei(r.Wei))
		case "conn":
			if r.At.Before(start) || r.At.After(end) {
				continue
			}
			out.Admitted += r.Admitted
			out.Refused += r.Refused
		}
	}
	return out
}

// sessionSpan is a session record's [start, end), tolerant of a record
// missing Start (older data, or a clock that ran backwards) by treating it as
// a point at At instead of letting a division by a zero or negative duration
// happen later.
func sessionSpan(r Record) (start, end time.Time) {
	end = r.At
	start = r.Start
	if start.IsZero() || !end.After(start) {
		start = end.Add(-time.Nanosecond)
	}
	return start, end
}

func parseWei(s string) *big.Int {
	if n, ok := new(big.Int).SetString(s, 10); ok {
		return n
	}
	return new(big.Int)
}

// shareOfWei is total scaled by part/whole, truncating rather than rounding:
// the same direction every bar's share is off in, so bars stay comparable to
// one another even though they will not sum back to the exact original.
func shareOfWei(total *big.Int, part, whole time.Duration) *big.Int {
	if whole <= 0 {
		return new(big.Int).Set(total)
	}
	return new(big.Int).Div(new(big.Int).Mul(total, big.NewInt(int64(part))), big.NewInt(int64(whole)))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
