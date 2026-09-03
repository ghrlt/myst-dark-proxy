package myst

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
)

// fakeNode serves the two endpoints picking a proposal needs.
func fakeNode(t *testing.T, offers ...tequilapi.Proposal) *tequilapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/authenticate":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case "/proposals":
			_ = json.NewEncoder(w).Encode(map[string]any{"proposals": offers})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return tequilapi.New(srv.URL, "u", "p", srv.Client())
}

func offer(id string) tequilapi.Proposal {
	return tequilapi.Proposal{ProviderID: id, ServiceType: "wireguard"}
}

// An exit change that has to work through a candidate which fails to connect
// has not moved anywhere yet. Forgetting what it was leaving at the first pick
// would let the retry settle straight back onto the exit the operator asked to
// get off, which is the one outcome pressing the button rules out.
func TestTheExitBeingLeftIsAvoidedUntilAnotherOneIsReached(t *testing.T) {
	tun := New(Options{
		Client: fakeNode(t, offer("0xA"), offer("0xB"), offer("0xC")),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tun.setAvoid("0xA")

	// Every pick between the request and the arrival - the first, and each
	// retry after a candidate that could not be connected to - moves away.
	for attempt := range 5 {
		got, err := tun.pickProposal(context.Background())
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if got.ProviderID == "0xA" {
			t.Fatalf("attempt %d picked 0xA, the exit being left", attempt)
		}
	}

	// Arrived: the exit that was left is an ordinary candidate again, or it
	// would be refused for the rest of the relay's life on one click.
	tun.clearAvoid()
	if away := tun.avoiding(); away != "" {
		t.Fatalf("still avoiding %q after arriving somewhere", away)
	}
	seenA := false
	for range 50 {
		got, err := tun.pickProposal(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.ProviderID == "0xA" {
			seenA = true
			break
		}
	}
	if !seenA {
		t.Error("0xA never came back into the running after the move completed")
	}
}

// A policy narrow enough to match a single provider must still be able to
// connect: refusing to reconnect at all is a worse answer than reconnecting to
// the exit that was being left.
func TestTheOnlyExitLeftIsUsedEvenWhenItIsTheOneBeingLeft(t *testing.T) {
	tun := New(Options{
		Client: fakeNode(t, offer("0xA")),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tun.setAvoid("0xA")

	got, err := tun.pickProposal(context.Background())
	if err != nil {
		t.Fatalf("picking with nowhere else to go: %v", err)
	}
	if got.ProviderID != "0xA" {
		t.Fatalf("picked %s, want the only offer 0xA", got.ProviderID)
	}
}
