// Package stats keeps a small history of what the relay has carried, so
// "what did this cost me last week" survives the relay restarting - unlike
// the running totals in tunnel/myst's accounting, which start over every time.
//
// A record is written in two cases: a Mysterium session ending (bytes,
// duration, cost, which exit), and a periodic tick of how many client
// connections the kill switch has let through. Both land in one append-only,
// newline-delimited JSON file, one line per record - deliberately not a
// database: the volume is a handful of lines an hour at most, and a file
// anyone can read with a text editor is worth more here than a schema.
package stats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Record is one line of the stats file.
type Record struct {
	Kind string    `json:"kind"` // "session" | "conn"
	At   time.Time `json:"at"`   // a session's end, or the moment a tick was taken

	// Session fields.
	Start         time.Time `json:"start"`
	Provider      string    `json:"provider,omitempty"`
	Country       string    `json:"country,omitempty"`
	IPType        string    `json:"ip_type,omitempty"`
	BytesSent     uint64    `json:"bytes_sent,omitempty"`
	BytesReceived uint64    `json:"bytes_received,omitempty"`
	Wei           string    `json:"wei,omitempty"`

	// Conn fields: connections the kill switch admitted or refused since the
	// previous "conn" record (or since the relay started, for the first one).
	Admitted uint64 `json:"admitted,omitempty"`
	Refused  uint64 `json:"refused,omitempty"`
}

// Append adds one record to path, creating it and its directory if either is
// missing. An empty path is a no-op: stats logging follows RELAY_STATS_FILE
// the same way the relay's own log follows RELAY_LOG_FILE, and "" means off.
func Append(path string, r Record) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = f.Write(line)
	return err
}

// Load reads every well-formed record in path, oldest first. A line that will
// not parse - the tail of a write cut short by a crash, most likely - is
// skipped rather than failing the whole read: history up to that point is
// still worth having. A missing file reads as no history at all, not an
// error: nothing has been logged yet is the ordinary case for a fresh install.
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
