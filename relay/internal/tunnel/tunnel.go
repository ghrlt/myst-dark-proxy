// Package tunnel defines the egress abstraction the frontends dial through.
//
// Everything above this package is transport-agnostic: the SOCKS5, HTTP and raw
// frontends only ever see a Tunnel. The Mysterium implementation lives in
// tunnel/myst; adding a second egress kind (a second myst node, a plain SOCKS5
// upstream, WireGuard, ...) means implementing this interface and nothing else.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"
)

// State is the lifecycle position of a tunnel.
type State string

const (
	StateDown       State = "down"
	StateConnecting State = "connecting"
	StateUp         State = "up"
	StateFailed     State = "failed"
	// StateIdle is "no session, on purpose": sessions are opened on demand and
	// nothing has asked for one. No traffic is carried - Usable is false, and
	// the kill switch stays shut - but nothing is wrong either, and nothing is
	// being billed. The next client's arrival is what builds a session.
	StateIdle State = "idle"
)

// Usable reports whether traffic may be carried.
func (s State) Usable() bool { return s == StateUp }

// ErrTunnelDown is returned by DialContext whenever the egress path is not
// usable. Callers must surface it as a refusal: there is deliberately no
// direct-dial fallback anywhere in this codebase.
var ErrTunnelDown = errors.New("tunnel: egress is down")

// Status is an immutable snapshot of a tunnel.
type Status struct {
	State      State     `json:"state"`
	ProviderID string    `json:"provider_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Country    string    `json:"country,omitempty"`
	IPType     string    `json:"ip_type,omitempty"`
	ExitIP     string    `json:"exit_ip,omitempty"`
	Since      time.Time `json:"since"`
	Detail     string    `json:"detail,omitempty"`
}

// Tunnel is a maintained egress path.
type Tunnel interface {
	// Name identifies the tunnel in logs and health output.
	Name() string
	// Run maintains the tunnel until ctx is cancelled. It reconnects on its own
	// and only returns an error the caller cannot recover from.
	Run(ctx context.Context) error
	// Status returns the current snapshot.
	Status() Status
	// Watch returns a channel receiving every status change, plus a cancel
	// function that must be called to release it.
	Watch() (<-chan Status, func())
	// DialContext opens a stream to addr through the tunnel. It must return
	// ErrTunnelDown rather than dialing directly when the path is unusable.
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Broadcaster is the status fan-out shared by tunnel implementations.
type Broadcaster struct {
	mu      sync.RWMutex
	status  Status
	next    int
	watcher map[int]chan Status
}

// NewBroadcaster returns a broadcaster seeded with a down status.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		status:  Status{State: StateDown, Since: time.Now()},
		watcher: map[int]chan Status{},
	}
}

// Status returns the latest snapshot.
func (b *Broadcaster) Status() Status {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status
}

// Set publishes a new status. The Since timestamp is preserved while the state
// is unchanged so uptime is meaningful.
func (b *Broadcaster) Set(s Status) {
	b.mu.Lock()
	if s.Since.IsZero() {
		if s.State == b.status.State && !b.status.Since.IsZero() {
			s.Since = b.status.Since
		} else {
			s.Since = time.Now()
		}
	}
	if s == b.status {
		b.mu.Unlock()
		return
	}
	b.status = s
	// Sent with the lock held. The sends never block, so holding it costs
	// nothing - and releasing it first let a watcher's cancel close the
	// channel in between, which makes the send panic, select or no select.
	for _, ch := range b.watcher {
		// Watchers are edge-triggered and coalescing: a slow one may miss
		// intermediate states but always converges on the latest. A full
		// buffer holds an older state, so it is replaced rather than kept.
		select {
		case ch <- s:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- s:
			default:
			}
		}
	}
	b.mu.Unlock()
}

// Watch registers a watcher, pre-loaded with the current status.
func (b *Broadcaster) Watch() (<-chan Status, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Status, 1)
	ch <- b.status
	b.watcher[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.watcher[id]; ok {
			delete(b.watcher, id)
			close(c)
		}
	}
}

// mystDecimals is how many decimal places one MYST is divided into on chain.
// Everything the node reports about money is an integer count of the smallest
// unit, and this is what turns one into the other.
const mystDecimals = 18

// Meter is what one span of tunnel use carried, and what the exit charged for
// it.
//
// Wei is a decimal string rather than a number because MYST carries eighteen
// decimals: a relay left running for months spends more of them than a uint64
// can hold, and a float would round away the digits the figure exists to show.
type Meter struct {
	BytesSent     uint64 `json:"bytes_sent"`
	BytesReceived uint64 `json:"bytes_received"`
	Wei           string `json:"wei"`
	MYST          string `json:"myst"`
	Duration      string `json:"duration,omitempty"`
}

// Usage is what the relay has carried and what it has cost: the session open
// right now on its own, and every session since the relay started added up.
//
// The two are separate because the node's own counters restart with each
// session, so its answer alone says nothing about the day's spending.
type Usage struct {
	Session  Meter  `json:"session"`
	Total    Meter  `json:"total"`
	Sessions uint64 `json:"sessions_total"`
}

// Balance is what the identity has left to spend, as the node reports it.
type Balance struct {
	Identity     string    `json:"identity"`
	Registration string    `json:"registration"`
	MYST         string    `json:"myst"`
	Wei          string    `json:"wei"`
	Channel      string    `json:"channel,omitempty"`
	At           time.Time `json:"at"`
}

// FormatMYST renders an amount of wei as MYST to four decimal places - enough
// to see what a session cost, few enough to read at a glance.
func FormatMYST(wei *big.Int) string {
	if wei == nil {
		return "0.0000"
	}
	// Scale down to ten-thousandths of a MYST first, then split that integer
	// into its whole and fractional halves. Keeping the whole calculation in
	// integers is what stops the answer being rounded twice.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(mystDecimals-4), nil)
	units := new(big.Int).Quo(new(big.Int).Abs(wei), scale)
	whole, frac := new(big.Int).QuoRem(units, big.NewInt(10000), new(big.Int))
	sign := ""
	if wei.Sign() < 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s%s.%04d", sign, whole, frac)
}
