package tequilapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The node keys /connection/statistics on the session's UUID while every other
// /connection endpoint keys on the proxy port, and it answers an id it does not
// know with a document of zeros rather than an error. Sending the wrong id
// therefore reads as "this session has carried nothing", which is why this is
// pinned by a test rather than left to the endpoint's resemblance to its
// neighbours.
func TestStatisticsAsksBySessionID(t *testing.T) {
	var gotID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/authenticate" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		gotID = r.URL.Query().Get("id")
		_, _ = w.Write([]byte(`{"bytes_sent":1,"bytes_received":2,"duration":3,` +
			`"tokens_spent":378649423365978,"spent_tokens":{"wei":"378649423365978"}}`))
	}))
	defer srv.Close()

	st, err := New(srv.URL, "u", "p", nil).Statistics(context.Background(), "a07df02c-c055-4dce-b347-801ede317dcd")
	if err != nil {
		t.Fatalf("statistics: %v", err)
	}
	if gotID != "a07df02c-c055-4dce-b347-801ede317dcd" {
		t.Errorf("asked for id %q, want the session id", gotID)
	}
	if st.BytesSent != 1 || st.BytesReceived != 2 {
		t.Errorf("counters: got %d up %d down, want 1 and 2", st.BytesSent, st.BytesReceived)
	}
	if got := st.Wei().String(); got != "378649423365978" {
		t.Errorf("spend: got %s wei, want 378649423365978", got)
	}
}

// tokens_spent is a bare JSON number holding wei, and wei runs past uint64 well
// before a session runs past a few MYST. Decoding it into one would fail the
// whole document, taking the byte counters down with the figure that overflowed.
func TestStatisticsSurvivesASpendLargerThanUint64(t *testing.T) {
	const huge = "123456789012345678901234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/authenticate" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		_, _ = w.Write([]byte(`{"bytes_sent":10,"bytes_received":20,"tokens_spent":` + huge + `}`))
	}))
	defer srv.Close()

	st, err := New(srv.URL, "u", "p", nil).Statistics(context.Background(), "sid")
	if err != nil {
		t.Fatalf("statistics: %v", err)
	}
	if st.BytesSent != 10 || st.BytesReceived != 20 {
		t.Errorf("counters: got %d up %d down, want 10 and 20", st.BytesSent, st.BytesReceived)
	}
	if got := st.Wei().String(); got != huge {
		t.Errorf("spend: got %s wei, want %s", got, huge)
	}
}

// A session id the relay has not learned yet must not be sent as an empty id=,
// which the node reads as "whichever session you hold" rather than "none".
func TestStatisticsOmitsAnUnknownSessionID(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/authenticate" {
			_, _ = w.Write([]byte(`{"token":"t"}`))
			return
		}
		raw = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "u", "p", nil).Statistics(context.Background(), ""); err != nil {
		t.Fatalf("statistics: %v", err)
	}
	if raw != "" {
		t.Errorf("query: got %q, want no id at all", raw)
	}
}
