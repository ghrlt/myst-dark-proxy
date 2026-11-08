// Package logfile writes a log as one file per day.
//
// A single log that only ever grows is unusable long before it is a disk
// problem: finding last night's failure means scrolling through a month of
// everything else, and the old cure - deleting the file once it passed a size
// at startup - threw the history away at random, usually just before it was
// needed. So a log named by its base path, relay.log, is written as
// relay-2026-11-09.log, the next day's lines go to relay-2026-11-10.log, and
// days older than the retention are deleted as new ones start.
//
// A day that grows past the size cap - a crash loop, a node logging at debug
// under load - continues in relay-2026-11-09.2.log, .3 and so on, so one bad
// day cannot fill the disk with a single file nobody can open.
//
// Days are local days: the log is read by a person asking about "last night".
package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dayLayout = "2006-01-02"

// Writer is an io.Writer that writes to the current day's file. It is safe for
// concurrent use. Each Write lands whole in one file, so a writer that emits a
// line per call - slog does - never has a line split across two days.
type Writer struct {
	base     string
	keepDays int
	maxSize  int64
	now      func() time.Time

	mu   sync.Mutex
	f    *os.File
	day  string
	part int
	size int64
}

// Open starts writing the log whose base path is base (".../relay.log").
// keepDays is how many days of files to keep, today included; 0 keeps them
// all. maxSize is how large one file may grow before the day continues in the
// next part; 0 means no limit.
func Open(base string, keepDays int, maxSize int64) (*Writer, error) {
	return open(base, keepDays, maxSize, time.Now)
}

func open(base string, keepDays int, maxSize int64, now func() time.Time) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return nil, err
	}
	w := &Writer{base: base, keepDays: keepDays, maxSize: maxSize, now: now}
	if err := w.openDay(now()); err != nil {
		return nil, err
	}
	return w, nil
}

// Write appends p to today's file, moving to a new file first when the day
// has changed or the current one is full.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	switch {
	case w.f == nil || now.Format(dayLayout) != w.day:
		if err := w.openDay(now); err != nil {
			return 0, err
		}
	case w.maxSize > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxSize:
		if err := w.openPart(w.part + 1); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the current file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Path is the file being written right now.
func (w *Writer) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return PartPath(w.base, w.day, w.part)
}

// openDay moves to day's file, picking up where an earlier run on the same day
// left off rather than starting a new part per restart. The lock is held.
func (w *Writer) openDay(now time.Time) error {
	w.day = now.Format(dayLayout)
	part := 1
	if files, err := list(w.base); err == nil {
		for _, f := range files {
			if f.day == w.day && f.part > part {
				part = f.part
			}
		}
	}
	if err := w.openPart(part); err != nil {
		return err
	}
	// A full part left by an earlier run is not appended to.
	if w.maxSize > 0 && w.size >= w.maxSize {
		if err := w.openPart(part + 1); err != nil {
			return err
		}
	}
	w.prune(now)
	return nil
}

// openPart closes the current file and opens the given part of the current
// day. The lock is held.
func (w *Writer) openPart(part int) error {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	path := PartPath(w.base, w.day, part)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	w.f, w.part, w.size = f, part, size
	return nil
}

// prune deletes the days that have fallen out of the retention, and the
// single undated file older versions wrote once it is as old as they are.
func (w *Writer) prune(now time.Time) {
	if w.keepDays <= 0 {
		return
	}
	y, m, d := now.Date()
	cutoff := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(w.keepDays - 1))
	files, err := list(w.base)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.date.Before(cutoff) {
			_ = os.Remove(f.path)
		}
	}
	if st, err := os.Stat(w.base); err == nil && st.Mode().IsRegular() && st.ModTime().Before(cutoff) {
		_ = os.Remove(w.base)
	}
}

// PartPath names one file of the log: base "relay.log", day "2026-11-09" and
// part 1 give "relay-2026-11-09.log"; part 2 gives "relay-2026-11-09.2.log".
func PartPath(base, day string, part int) string {
	dir, stem, ext := split(base)
	name := stem + "-" + day
	if part > 1 {
		name += "." + strconv.Itoa(part)
	}
	return filepath.Join(dir, name+ext)
}

// Latest is the newest file of the log whose base path is base - what to point
// someone at who wants to see what it is saying now. With no dated file yet it
// is base itself, which is where a version before daily files wrote.
func Latest(base string) string {
	files, err := list(base)
	if err != nil || len(files) == 0 {
		return base
	}
	return files[len(files)-1].path
}

// Pattern is a glob matching every dated file of the log, for messages that
// need to say where it lives without naming one day.
func Pattern(base string) string {
	dir, stem, ext := split(base)
	return filepath.Join(dir, stem+"-*"+ext)
}

func split(base string) (dir, stem, ext string) {
	dir = filepath.Dir(base)
	name := filepath.Base(base)
	ext = filepath.Ext(name)
	if ext == "" {
		ext = ".log"
	}
	return dir, strings.TrimSuffix(name, filepath.Ext(name)), ext
}

type dated struct {
	path string
	day  string
	date time.Time
	part int
}

// list returns the dated files of the log, oldest first.
func list(base string) ([]dated, error) {
	dir, stem, ext := split(base)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	prefix := stem + "-"
	var out []dated
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext)
		day, partStr, hasPart := strings.Cut(rest, ".")
		date, err := time.ParseInLocation(dayLayout, day, time.Local)
		if err != nil {
			continue
		}
		part := 1
		if hasPart {
			if part, err = strconv.Atoi(partStr); err != nil || part < 2 {
				continue
			}
		}
		out = append(out, dated{path: filepath.Join(dir, name), day: day, date: date, part: part})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].day != out[j].day {
			return out[i].day < out[j].day
		}
		return out[i].part < out[j].part
	})
	return out, nil
}
