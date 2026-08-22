// Package gate implements the kill switch.
//
// The relay's leak-proofing is layered, and this package is the innermost of
// the three:
//
//  1. Network: the relay container sits on an internal-only Docker network. It
//     has no route to the internet at all, so a leak is not merely forbidden,
//     it is unroutable.
//  2. Code: client traffic is only ever dialled through a tunnel.Tunnel, which
//     returns ErrTunnelDown instead of falling back to a direct dial.
//  3. Policy: this gate. While the tunnel is not up, new streams are refused;
//     in strict mode, established streams are torn down as well, so no stream
//     survives across an exit change.
package gate

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrClosed is returned by Enter while the gate is shut.
type ErrClosed struct{ Reason string }

func (e *ErrClosed) Error() string {
	if e.Reason == "" {
		return "kill switch: closed"
	}
	return "kill switch: closed (" + e.Reason + ")"
}

// IsClosed reports whether err came from a shut gate.
func IsClosed(err error) bool {
	var e *ErrClosed
	return errors.As(err, &e)
}

// Session is a permit to carry one stream. Its Context is cancelled when the
// gate shuts in strict mode, which is the signal handlers use to tear the
// stream down.
type Session struct {
	Context context.Context
	done    func()
	once    sync.Once
}

// Close releases the permit.
func (s *Session) Close() { s.once.Do(s.done) }

type epoch struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// Gate admits or refuses streams based on tunnel state.
type Gate struct {
	strict bool
	parent context.Context

	mu       sync.RWMutex
	open     bool
	reason   string
	changed  time.Time
	current  *epoch
	orphans  []*epoch // permissive-mode epochs, cancelled at shutdown
	active   int
	admitted uint64
	refused  uint64
}

// New returns a gate that starts closed. strict tears established streams down
// when the gate shuts; otherwise they are allowed to drain.
func New(parent context.Context, strict bool) *Gate {
	g := &Gate{strict: strict, parent: parent, reason: "starting up", changed: time.Now()}
	g.current = g.newEpoch()
	g.current.cancel() // closed until the tunnel reports up
	return g
}

func (g *Gate) newEpoch() *epoch {
	ctx, cancel := context.WithCancel(g.parent)
	return &epoch{ctx: ctx, cancel: cancel}
}

// Open admits traffic.
func (g *Gate) Open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open {
		return
	}
	g.current = g.newEpoch()
	g.open = true
	g.reason = ""
	g.changed = time.Now()
}

// Close refuses traffic, recording why.
func (g *Gate) Close(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		g.reason = reason
		return
	}
	g.open = false
	g.reason = reason
	g.changed = time.Now()
	if g.strict {
		g.current.cancel()
	} else {
		g.orphans = append(g.orphans, g.current)
	}
}

// Enter requests a permit. The returned session must be closed by the caller.
func (g *Gate) Enter() (*Session, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		g.refused++
		return nil, &ErrClosed{Reason: g.reason}
	}
	g.active++
	g.admitted++
	ep := g.current
	return &Session{
		Context: ep.ctx,
		done: func() {
			g.mu.Lock()
			g.active--
			g.mu.Unlock()
		},
	}, nil
}

// Shutdown cancels every outstanding session.
func (g *Gate) Shutdown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = false
	g.reason = "shutting down"
	g.current.cancel()
	for _, e := range g.orphans {
		e.cancel()
	}
	g.orphans = nil
}

// Stats is a snapshot of gate counters for health reporting.
type Stats struct {
	Open     bool      `json:"open"`
	Strict   bool      `json:"strict"`
	Reason   string    `json:"reason,omitempty"`
	Since    time.Time `json:"since"`
	Active   int       `json:"active_streams"`
	Admitted uint64    `json:"admitted_total"`
	Refused  uint64    `json:"refused_total"`
}

// Stats returns the current counters.
func (g *Gate) Stats() Stats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return Stats{
		Open:     g.open,
		Strict:   g.strict,
		Reason:   g.reason,
		Since:    g.changed,
		Active:   g.active,
		Admitted: g.admitted,
		Refused:  g.refused,
	}
}
