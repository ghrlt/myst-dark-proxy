package myst

import (
	"context"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
)

// balanceTTL is how long an answer about money is reused. A tray asking every
// few seconds must not turn into a question to the node every few seconds, and
// a balance moves far too slowly for the staleness to matter.
const balanceTTL = 30 * time.Second

// accounting adds up what sessions have carried and what they have cost.
//
// It exists because the node's counters are per session and restart with each
// one: asking the node alone would say what the current exit has charged and
// nothing about the day. Sessions are folded into a running total as they end,
// so what the relay has spent survives its exits coming and going.
type accounting struct {
	mu sync.Mutex

	open  bool
	since time.Time
	sent  uint64
	recv  uint64
	wei   *big.Int

	totalSent uint64
	totalRecv uint64
	totalWei  *big.Int
	totalFor  time.Duration

	balance   tunnel.Balance
	balanceAt time.Time
}

// begin opens a session span. Anything the previous one left unfolded is
// folded first, so a session that ended without being closed cleanly is still
// counted rather than silently dropped.
func (a *accounting) begin() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fold()
	a.open, a.since = true, time.Now()
	a.wei = new(big.Int)
}

// observe records the node's counters for the live session. They are absolute
// for that session rather than incremental, so they are stored, not added.
func (a *accounting) observe(st tequilapi.Statistics) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.open {
		return
	}
	a.sent, a.recv = st.BytesSent, st.BytesReceived
	a.wei = st.Wei()
}

// end closes the session span and folds it into the totals.
func (a *accounting) end() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fold()
}

// fold moves the current session into the totals. Callers hold the lock.
func (a *accounting) fold() {
	if !a.open {
		return
	}
	a.totalSent += a.sent
	a.totalRecv += a.recv
	if a.totalWei == nil {
		a.totalWei = new(big.Int)
	}
	if a.wei != nil {
		a.totalWei.Add(a.totalWei, a.wei)
	}
	a.totalFor += time.Since(a.since)
	a.open, a.sent, a.recv, a.wei = false, 0, 0, nil
}

// snapshot renders the counters for the status document.
func (a *accounting) snapshot(sessions uint64) tunnel.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()

	total := tunnel.Meter{
		BytesSent:     a.totalSent,
		BytesReceived: a.totalRecv,
		Wei:           weiString(a.totalWei),
		MYST:          tunnel.FormatMYST(a.totalWei),
		Duration:      a.totalFor.Truncate(time.Second).String(),
	}
	out := tunnel.Usage{Total: total, Sessions: sessions}
	if !a.open {
		out.Session = tunnel.Meter{Wei: "0", MYST: tunnel.FormatMYST(nil)}
		return out
	}

	live := time.Since(a.since)
	out.Session = tunnel.Meter{
		BytesSent:     a.sent,
		BytesReceived: a.recv,
		Wei:           weiString(a.wei),
		MYST:          tunnel.FormatMYST(a.wei),
		Duration:      live.Truncate(time.Second).String(),
	}
	// The live session is not in the totals yet, so add it here rather than
	// reporting a total that trails the session it contains.
	out.Total.BytesSent += a.sent
	out.Total.BytesReceived += a.recv
	sum := new(big.Int).Set(orZero(a.totalWei))
	sum.Add(sum, orZero(a.wei))
	out.Total.Wei = sum.String()
	out.Total.MYST = tunnel.FormatMYST(sum)
	out.Total.Duration = (a.totalFor + live).Truncate(time.Second).String()
	return out
}

// cachedBalance returns the last answer while it is still fresh.
func (a *accounting) cachedBalance() (tunnel.Balance, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.balanceAt.IsZero() || time.Since(a.balanceAt) > balanceTTL {
		return a.balance, false
	}
	return a.balance, true
}

func (a *accounting) setBalance(b tunnel.Balance) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.balance, a.balanceAt = b, time.Now()
}

// Usage is what this tunnel has carried and what it has cost.
func (t *Tunnel) Usage() tunnel.Usage { return t.acct.snapshot(t.sessions.Load()) }

// Balance reports what the identity has left to spend.
//
// The node is the only thing that knows, so this fails while the node is not
// running - which is honest: a stale figure presented as current is worse than
// saying the question cannot be answered right now.
func (t *Tunnel) Balance(ctx context.Context) (tunnel.Balance, error) {
	if b, ok := t.acct.cachedBalance(); ok {
		return b, nil
	}
	info, err := t.opt.Client.Identity(ctx, t.opt.Identity)
	if err != nil {
		return tunnel.Balance{}, err
	}
	wei := strings.TrimSpace(info.BalanceTokens.Wei)
	if wei == "" {
		wei = info.Balance.String()
	}
	human := strings.TrimSpace(info.BalanceTokens.Human)
	if human == "" {
		human = strings.TrimSpace(info.BalanceTokens.Ether)
	}
	if human == "" {
		if n, ok := new(big.Int).SetString(wei, 10); ok {
			human = tunnel.FormatMYST(n)
		}
	}
	b := tunnel.Balance{
		Identity:     info.ID,
		Registration: info.RegistrationStatus,
		MYST:         human,
		Wei:          wei,
		Channel:      info.ChannelAddress,
		At:           time.Now(),
	}
	if b.Identity == "" {
		b.Identity = t.opt.Identity
	}
	t.acct.setBalance(b)
	return b, nil
}

func weiString(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}

func orZero(n *big.Int) *big.Int {
	if n == nil {
		return new(big.Int)
	}
	return n
}
