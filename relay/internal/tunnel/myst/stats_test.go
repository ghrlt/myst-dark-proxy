package myst

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/stats"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
)

func TestFinishSessionAppendsARecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	tun := &Tunnel{
		opt: Options{StatsFile: path},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	started := tunnel.Status{ProviderID: "0xprovider", Country: "NL", IPType: "residential"}
	tun.acct.begin()
	tun.acct.observe(tequilapi.Statistics{BytesSent: 100, BytesReceived: 900, TokensSpent: "42"})
	tun.finishSession(started)

	got, err := stats.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	r := got[0]
	if r.Kind != "session" || r.Provider != "0xprovider" || r.Country != "NL" || r.IPType != "residential" {
		t.Errorf("record = %+v, want provider/country/ip_type from the session that started", r)
	}
	if r.BytesSent != 100 || r.BytesReceived != 900 || r.Wei != "42" {
		t.Errorf("record = %+v, want the folded meter's bytes and wei", r)
	}
}

func TestFinishSessionWritesNothingWithNoStatsFile(t *testing.T) {
	tun := &Tunnel{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	tun.acct.begin()
	// No panic, no file created: opt.StatsFile is "" by default.
	tun.finishSession(tunnel.Status{ProviderID: "0xprovider"})
}

func TestFinishSessionWritesNothingWithoutAnOpenSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	tun := &Tunnel{
		opt: Options{StatsFile: path},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// finishSession called with no matching begin() - should not happen in
	// practice, but must not fabricate a record for a session that never was.
	tun.finishSession(tunnel.Status{ProviderID: "0xprovider"})

	got, err := stats.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d records, want 0", len(got))
	}
}
