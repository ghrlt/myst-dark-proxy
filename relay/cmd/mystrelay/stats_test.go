package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/stats"
)

func TestRecordConnectionsTicksOnlyWhenSomethingHappened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	g := gate.New(context.Background(), false)
	g.Open()
	sess, err := g.Enter()
	if err != nil {
		t.Fatal(err)
	}
	sess.Close()
	g.Close("test")
	if _, err := g.Enter(); err == nil {
		t.Fatal("expected the closed gate to refuse")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		recordConnections(ctx, g, path, time.Hour, log) // long interval: only the shutdown flush should fire
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	got, err := stats.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1 (the shutdown flush)", len(got))
	}
	if got[0].Kind != "conn" || got[0].Admitted != 1 || got[0].Refused != 1 {
		t.Errorf("record = %+v, want one admitted and one refused", got[0])
	}
}

func TestRecordConnectionsSkipsAnEmptyTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := gate.New(context.Background(), false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // nothing ever happened; the single shutdown-flush tick must write nothing
	recordConnections(ctx, g, path, time.Hour, log)

	got, err := stats.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d records from an idle gate, want 0", len(got))
	}
}
