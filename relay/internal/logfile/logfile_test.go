package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEachDayGoesToItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "relay.log")
	c := &clock{t: at("2026-11-09 23:59")}
	w, err := open(base, 0, 0, c.now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	_, _ = w.Write([]byte("before midnight\n"))
	c.set(at("2026-11-10 00:00"))
	_, _ = w.Write([]byte("after midnight\n"))

	if got := read(t, filepath.Join(dir, "relay-2026-11-09.log")); got != "before midnight\n" {
		t.Fatalf("first day holds %q", got)
	}
	if got := read(t, filepath.Join(dir, "relay-2026-11-10.log")); got != "after midnight\n" {
		t.Fatalf("second day holds %q", got)
	}
	if w.Path() != filepath.Join(dir, "relay-2026-11-10.log") || Latest(base) != w.Path() {
		t.Fatalf("current file %s, latest %s", w.Path(), Latest(base))
	}
}

// A relay restarted during the day carries on in that day's file.
func TestARestartTheSameDayAppends(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "relay.log")
	c := &clock{t: at("2026-11-09 10:00")}
	for _, line := range []string{"first run\n", "second run\n"} {
		w, err := open(base, 0, 0, c.now)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(line))
		_ = w.Close()
	}
	if got := read(t, filepath.Join(dir, "relay-2026-11-09.log")); got != "first run\nsecond run\n" {
		t.Fatalf("day file holds %q", got)
	}
}

// A day past the size cap continues in numbered parts, never splitting a write.
func TestAFullDayContinuesInTheNextPart(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "node.log")
	c := &clock{t: at("2026-11-09 10:00")}
	w, err := open(base, 0, 10, c.now)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, _ = w.Write([]byte("12345678\n")) // 9 bytes: one per part
	}
	_ = w.Close()
	for i, name := range []string{"node-2026-11-09.log", "node-2026-11-09.2.log", "node-2026-11-09.3.log"} {
		if got := read(t, filepath.Join(dir, name)); got != "12345678\n" {
			t.Fatalf("part %d (%s) holds %q", i+1, name, got)
		}
	}
	if !strings.HasSuffix(Latest(base), "node-2026-11-09.3.log") {
		t.Fatalf("latest is %s", Latest(base))
	}

	// A restart the same day does not append to a part that is already full.
	w, err = open(base, 0, 10, c.now)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("x\n"))
	_ = w.Close()
	if got := read(t, filepath.Join(dir, "node-2026-11-09.4.log")); got != "x\n" {
		t.Fatalf("restart wrote %q to part 4", got)
	}
}

func TestDaysOutsideTheRetentionAreDeleted(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "relay.log")
	for _, name := range []string{
		"relay-2026-11-01.log", "relay-2026-11-01.2.log", // past keep
		"relay-2026-11-07.log", "relay-2026-11-08.log", // within keep
		"relay-notes.log", "other-2026-11-01.log", // not ours
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The single file an older version wrote, last touched long ago.
	_ = os.WriteFile(base, []byte("legacy"), 0o644)
	old := at("2026-10-01 00:00")
	_ = os.Chtimes(base, old, old)

	c := &clock{t: at("2026-11-09 08:00")}
	w, err := open(base, 3, 0, c.now) // keeps the 7th, 8th and 9th
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	for _, gone := range []string{"relay-2026-11-01.log", "relay-2026-11-01.2.log", "relay.log"} {
		if exists(gone) {
			t.Errorf("%s survived the retention", gone)
		}
	}
	for _, kept := range []string{"relay-2026-11-07.log", "relay-2026-11-08.log", "relay-2026-11-09.log", "relay-notes.log", "other-2026-11-01.log"} {
		if !exists(kept) {
			t.Errorf("%s was deleted", kept)
		}
	}
}
