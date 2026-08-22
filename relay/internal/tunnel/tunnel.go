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
	watchers := make([]chan Status, 0, len(b.watcher))
	for _, ch := range b.watcher {
		watchers = append(watchers, ch)
	}
	b.mu.Unlock()

	for _, ch := range watchers {
		// Watchers are edge-triggered and coalescing: a slow one may miss
		// intermediate states but always converges on the latest.
		select {
		case ch <- s:
		default:
		}
	}
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
