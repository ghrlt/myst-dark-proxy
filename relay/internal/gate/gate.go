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
//
// A shut gate has two flavours. Closed means refuse, now: the tunnel failed and
// the honest answer to a client is an immediate error. Armed means shut on
// purpose with a session about to be built - the state a relay rests in when
// sessions are opened on demand - and a client that arrives is made to wait for
// the session its own arrival starts. Neither flavour carries a byte.
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
	armed    bool          // shut, but a session is expected: waiters may wait
	change   chan struct{} // closed and replaced on every state change
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
	g := &Gate{
		strict:  strict,
		parent:  parent,
		reason:  "starting up",
		changed: time.Now(),
		change:  make(chan struct{}),
	}
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
	g.armed = false
	g.reason = ""
	g.changed = time.Now()
	g.broadcast()
}

// Close refuses traffic, recording why. Callers waiting in WaitOpen are
// released with that refusal rather than left hanging.
func (g *Gate) Close(reason string) { g.shut(reason, false) }

// Arm shuts the gate the same way Close does, but marks the closure as
// expected: there is no session because nothing has asked for one, and a
// client's arrival is what builds it. Waiters in WaitOpen keep waiting.
func (g *Gate) Arm(reason string) { g.shut(reason, true) }

func (g *Gate) shut(reason string, armed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		g.reason = reason
		if g.armed != armed {
			// Waiting just became pointless (or worth doing); say so.
			g.armed = armed
			g.broadcast()
		}
		return
	}
	g.open = false
	g.armed = armed
	g.reason = reason
	g.changed = time.Now()
	if g.strict {
		g.current.cancel()
	} else {
		g.orphans = append(g.orphans, g.current)
	}
	g.broadcast()
}

// broadcast releases everything blocked in WaitOpen. The lock must be held.
func (g *Gate) broadcast() {
	close(g.change)
	g.change = make(chan struct{})
}

// WaitOpen blocks until the gate admits traffic, and is how an on-demand relay
// makes a client wait for the session its own arrival starts. It carries no
// traffic and grants no permit: the caller still has to get through Enter.
//
// Only the state it finds on arrival decides whether waiting is on offer. A
// gate that is already closed refuses at once - a client whose tunnel has
// failed deserves the bad news now rather than in a minute, and that is what
// keeps the kill switch's reaction time honest. A caller admitted to the wait
// then stays in it until the gate opens or its own patience runs out, because
// the first provider tried is quite often not the one that works: releasing it
// on that first failure would refuse the one client actually waiting for the
// session, moments before the retry that succeeds.
func (g *Gate) WaitOpen(ctx context.Context) error {
	arrival := true
	for {
		g.mu.Lock()
		if g.open {
			g.mu.Unlock()
			return nil
		}
		if arrival && !g.armed {
			reason := g.reason
			g.refused++
			g.mu.Unlock()
			return &ErrClosed{Reason: reason}
		}
		arrival = false
		change, reason := g.change, g.reason
		g.mu.Unlock()

		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.refused++
			g.mu.Unlock()
			return &ErrClosed{Reason: "waited for a session, none came up (" + reason + ")"}
		case <-change:
		}
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
	g.armed = false
	g.reason = "shutting down"
	g.broadcast()
	g.current.cancel()
	for _, e := range g.orphans {
		e.cancel()
	}
	g.orphans = nil
}

// Stats is a snapshot of gate counters for health reporting.
type Stats struct {
	Open     bool      `json:"open"`
	Armed    bool      `json:"armed,omitempty"`
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
		Armed:    g.armed,
		Strict:   g.strict,
		Reason:   g.reason,
		Since:    g.changed,
		Active:   g.active,
		Admitted: g.admitted,
		Refused:  g.refused,
	}
}
