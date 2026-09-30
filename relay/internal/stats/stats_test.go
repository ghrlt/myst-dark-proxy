package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "sessions.jsonl")

	want := []Record{
		{Kind: "session", At: time.Now().UTC().Truncate(time.Second), BytesSent: 100, BytesReceived: 900, Wei: "12345"},
		{Kind: "conn", At: time.Now().UTC().Truncate(time.Second), Admitted: 3, Refused: 1},
	}
	for _, r := range want {
		if err := Append(path, r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].At.Equal(want[i].At) || got[i].Kind != want[i].Kind {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAppendEmptyPathIsNoop(t *testing.T) {
	if err := Append("", Record{Kind: "session"}); err != nil {
		t.Fatalf("Append with empty path: %v", err)
	}
}

func TestLoadMissingFileIsEmptyNotError(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err != nil {
		t.Fatalf("Load on a missing file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d records from a missing file", len(got))
	}
}

func TestLoadSkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	if err := Append(path, Record{Kind: "session", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Simulate a write cut short mid-crash: append a truncated, unparsable line.
	if err := Append(path, Record{Kind: "session", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f := path
	appendRaw(t, f, "{not json")

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d well-formed records, want 2 (the malformed line should be skipped)", len(got))
	}
}

func appendRaw(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}
