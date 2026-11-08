package myst

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// lingerFor pretends the last session's device has d left to live.
func lingerFor(t *Tunnel, d time.Duration) {
	t.mu.Lock()
	t.lingerUntil = time.Now().Add(d)
	t.mu.Unlock()
}

// A node the relay owns is restarted rather than waited out: two minutes of
// darkness on every exit change is the cost the restart exists to avoid.
func TestALeftoverDeviceIsClearedByRestartingAnOwnedNode(t *testing.T) {
	var resets atomic.Int32
	tun := New(Options{
		Logger: quietLog(),
		ResetNode: func(context.Context, string) error {
			resets.Add(1)
			return nil
		},
	})
	lingerFor(tun, time.Hour)

	start := time.Now()
	if err := tun.clearLeftoverDevice(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resets.Load() != 1 {
		t.Fatalf("node reset %d times, want 1", resets.Load())
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waited %s although the node was restarted", time.Since(start))
	}
	if left := tun.deviceLingering(); left > 0 {
		t.Fatalf("device still recorded as lingering for %s after the restart", left)
	}

	// Nothing left over: nothing to restart.
	if err := tun.clearLeftoverDevice(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resets.Load() != 1 {
		t.Fatalf("node reset again with no device left over")
	}
}

// Without a node of its own to restart, the relay waits until the device has
// gone - and no longer than that.
func TestALeftoverDeviceIsWaitedOutWhenTheNodeIsNotOurs(t *testing.T) {
	tun := New(Options{Logger: quietLog()})
	lingerFor(tun, 200*time.Millisecond)

	start := time.Now()
	if err := tun.clearLeftoverDevice(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("waited %s for a device with 200ms left", waited)
	}
}

// A restart that fails must not cost the session: the device still goes away
// on its own, so the relay falls back to waiting for that.
func TestAFailedRestartFallsBackToWaiting(t *testing.T) {
	tun := New(Options{
		Logger:    quietLog(),
		ResetNode: func(context.Context, string) error { return errors.New("supervisor gone") },
	})
	lingerFor(tun, 200*time.Millisecond)

	start := time.Now()
	if err := tun.clearLeftoverDevice(context.Background()); err != nil {
		t.Fatalf("a failed restart ended the attempt: %v", err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("connected after %s, while the device had 200ms left", waited)
	}
}

// Shutting down while waiting is not a reason to sit out the rest of the wait.
func TestWaitingForADeviceStopsWithTheRelay(t *testing.T) {
	tun := New(Options{Logger: quietLog()})
	lingerFor(tun, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := tun.clearLeftoverDevice(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context's error", err)
	}
}

// A connect the node refuses may still have built a device - the node does not
// say how far it got - so even a refused attempt has to be cleared up after
// before the next one. This is the retry loop that was failing: attempt after
// attempt opened next to the device the one before had left.
func TestARefusedConnectStillGetsItsDeviceClearedBeforeTheRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/auth/authenticate":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/healthcheck":
			_ = json.NewEncoder(w).Encode(map[string]string{"version": "test"})
		case r.URL.Path == "/proposals":
			_ = json.NewEncoder(w).Encode(map[string]any{"proposals": []tequilapi.Proposal{offer("0xA")}})
		case r.URL.Path == "/connection" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"err_connection_cancelled","message":"Connection cancelled"}}`)
		case r.URL.Path == "/connection" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			// Unlock and anything else the session asks for on the way.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "{}")
		}
	}))
	defer srv.Close()

	var resets atomic.Int32
	tun := New(Options{
		Client:    tequilapi.New(srv.URL, "u", "p", srv.Client()),
		Identity:  "0x1",
		ProxyHost: "127.0.0.1",
		ProxyPort: 40000,
		Logger:    quietLog(),
		ResetNode: func(context.Context, string) error {
			resets.Add(1)
			return nil
		},
	})

	if err := tun.session(context.Background()); err == nil {
		t.Fatal("a refused connect reported success")
	}
	if resets.Load() != 0 {
		t.Fatalf("node reset before the first attempt, with nothing left over")
	}
	if left := tun.deviceLingering(); left < deviceLinger-time.Minute {
		t.Fatalf("refused attempt left the device recorded as lingering for %s, want about %s", left, deviceLinger)
	}

	_ = tun.session(context.Background())
	if resets.Load() != 1 {
		t.Fatalf("retry reset the node %d times, want 1", resets.Load())
	}
}
