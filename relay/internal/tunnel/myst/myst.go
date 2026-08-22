// Package myst implements tunnel.Tunnel on top of a Mysterium node running in
// proxy mode (`myst --consumer --proxymode`).
//
// In proxy mode the node builds no TUN device and changes no routes: it exposes
// an HTTP CONNECT listener backed by a userspace WireGuard stack whose packets
// leave through the chosen provider. That is exactly the shape this relay wants
// - egress is a dialer, not a system-wide route, so nothing on the host can
// accidentally inherit it and nothing can silently bypass it.
//
// Two behaviours of the node in this mode are worth knowing, because both are
// invisible in its documentation and both were verified against 1.39.5:
//
//   - Sessions are keyed by the proxy port. Every /connection endpoint takes it
//     as ?id=; without it the node reports on connection 0 and always answers
//     "NotConnected".
//   - /connection/ip reports the node's own public address, not the session's,
//     so the exit address is established by fetching through the tunnel.
//
// A session is also a running cost: the provider charges per hour as well as
// per gibibyte, so one nobody is using is money spent carrying nothing. When a
// demand tracker is supplied the supervisor therefore opens a session only
// once a client asks for egress, and closes it again after the idle timeout.
package myst

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/demand"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/upstream"
)

// upstreamScanRange is how many ports past the requested one are probed when
// looking for the node's proxy listener. 1.39.5 honours proxy_port exactly;
// the scan is cheap insurance against a release that treats it as a base.
const upstreamScanRange = 8

// Options configures the tunnel.
type Options struct {
	Client     *tequilapi.Client
	Identity   string // 0x address
	Keystore   []byte // keystore JSON, imported when the node has not been seeded
	Passphrase string
	Exit       config.ExitPolicy

	ProxyHost string
	ProxyPort int
	DNSMode   string

	// ExitCheckURL is fetched through the tunnel once per session to learn the
	// exit address. Empty disables the check, at the cost of a blank exit_ip in
	// the health output.
	ExitCheckURL string

	DialTimeout    time.Duration
	HealthInterval time.Duration
	ReconnectMin   time.Duration
	ReconnectMax   time.Duration
	RotateEvery    time.Duration

	// Demand is how the frontends say a client wants egress now, and how the
	// supervisor learns that nothing is using the session any more. Leaving it
	// nil pins the tunnel up for as long as the relay runs, whatever the two
	// settings below say.
	Demand *demand.Tracker
	// OnDemand delays the first session until a client asks for one, so a relay
	// that is started at boot and used at noon is billed from noon.
	OnDemand bool
	// SessionIdle closes a session that has carried nothing for this long. 0
	// keeps it open until it fails or the relay stops.
	SessionIdle time.Duration

	Logger *slog.Logger
}

// Tunnel maintains one Mysterium session and dials through it.
type Tunnel struct {
	opt Options
	log *slog.Logger
	bc  *tunnel.Broadcaster

	// sessions counts how many have been opened, which is what says whether an
	// idle timeout is saving money or merely churning through providers.
	sessions atomic.Uint64

	// suspect carries a nudge from the data path to the supervisor: a stream
	// that could not reach the node's proxy is the earliest evidence the
	// tunnel is gone, and waiting for the next health tick to notice would
	// leave the gate open - admitting streams that are certain to fail.
	suspect chan struct{}

	mu           sync.RWMutex
	upstreamPort int
	cooldown     map[string]time.Time // provider -> retry-after
}

// New builds a tunnel. It does not contact the node; call Run for that.
func New(opt Options) *Tunnel {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.HealthInterval <= 0 {
		opt.HealthInterval = 5 * time.Second
	}
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = 30 * time.Second
	}
	return &Tunnel{
		opt:          opt,
		suspect:      make(chan struct{}, 1),
		log:          opt.Logger.With("component", "tunnel"),
		bc:           tunnel.NewBroadcaster(),
		upstreamPort: opt.ProxyPort,
		cooldown:     map[string]time.Time{},
	}
}

// Name identifies the tunnel.
func (t *Tunnel) Name() string { return "myst:" + t.opt.Identity }

// Status returns the latest snapshot.
func (t *Tunnel) Status() tunnel.Status { return t.bc.Status() }

// Watch subscribes to status changes.
func (t *Tunnel) Watch() (<-chan tunnel.Status, func()) { return t.bc.Watch() }

// Sessions is how many sessions have been opened since the relay started.
func (t *Tunnel) Sessions() uint64 { return t.sessions.Load() }

// dialer returns a proxy client for the currently discovered listener.
func (t *Tunnel) dialer() *upstream.HTTPConnect {
	t.mu.RLock()
	port := t.upstreamPort
	t.mu.RUnlock()
	return &upstream.HTTPConnect{Address: net.JoinHostPort(t.opt.ProxyHost, strconv.Itoa(port))}
}

// DialContext opens a stream through the current session. It refuses rather
// than dialing directly whenever the session is not up.
func (t *Tunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !t.bc.Status().State.Usable() {
		return nil, tunnel.ErrTunnelDown
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.opt.DialTimeout)
		defer cancel()
	}
	conn, err := t.dialer().DialContext(ctx, network, addr)
	if err != nil {
		if errors.Is(err, upstream.ErrUnreachable) {
			select {
			case t.suspect <- struct{}{}:
			default:
			}
			return nil, fmt.Errorf("%w: %w", tunnel.ErrTunnelDown, err)
		}
		if !t.bc.Status().State.Usable() {
			return nil, tunnel.ErrTunnelDown
		}
		return nil, err
	}
	return conn, nil
}

// Run maintains the session until ctx is cancelled.
func (t *Tunnel) Run(ctx context.Context) error {
	t.setState(tunnel.StateConnecting, "waiting for myst node")
	hc, err := t.opt.Client.WaitHealthy(ctx, 2*time.Second)
	if err != nil {
		return err
	}
	t.log.Info("myst node ready", "version", hc.Version, "uptime", hc.Uptime)

	if err := t.provisionIdentity(ctx); err != nil {
		return err
	}

	// A session the node kept from an earlier run is billed to this identity
	// and belongs to nobody. Drop it before deciding whether to open one, so
	// that "no session" really does mean "paying for nothing".
	t.dropStaleSession(ctx)

	backoff := t.opt.ReconnectMin
	sleep := t.opt.OnDemand && t.opt.Demand != nil
	var wake uint64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if sleep {
			t.setState(tunnel.StateIdle, "no client is asking for egress")
			t.log.Info("no session: waiting for a client",
				"idle_timeout", t.opt.SessionIdle, "sessions_so_far", t.sessions.Load())
			if wake, err = t.opt.Demand.Wait(ctx, wake); err != nil {
				return err
			}
			t.log.Info("a client asked for egress: opening a session")
			sleep, backoff = false, t.opt.ReconnectMin
		}

		err := t.session(ctx)
		var idle idleStop
		switch {
		case ctx.Err() != nil:
			t.closeSession(tunnel.StateDown, "shutting down")
			return ctx.Err()
		case errors.As(err, &idle):
			// Closed because nothing was using it; sleep until asked again.
			sleep, wake = true, idle.seq
		case err == nil:
			// Session ended on request (rotation): reconnect immediately,
			// unless the point of reconnecting has expired with it.
			backoff = t.opt.ReconnectMin
			if _, seq, expired := t.idleExpired(); expired {
				sleep, wake = true, seq
			}
		default:
			t.setState(tunnel.StateFailed, err.Error())
			t.log.Warn("session ended", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, t.opt.ReconnectMax)
			// Whoever this reconnect was for has stopped waiting. Retrying
			// would buy a session for nobody, so go back to sleep instead.
			if d, seq, expired := t.idleExpired(); expired {
				t.log.Info("nothing is waiting for a tunnel: staying disconnected",
					"idle_for", d.Truncate(time.Second))
				sleep, wake = true, seq
			}
		}
	}
}

// idleStop ends a session because nothing has used it for the idle timeout.
// It carries the demand sequence read at that moment: the supervisor sleeps on
// that sequence, so a client that arrives while the session is being torn down
// wakes it instead of being slept through.
type idleStop struct{ seq uint64 }

func (idleStop) Error() string { return "session closed after the idle timeout" }

// idleExpired reports whether the idle timeout has elapsed with nothing being
// carried, and on which demand sequence that was decided.
func (t *Tunnel) idleExpired() (time.Duration, uint64, bool) {
	if t.opt.Demand == nil || t.opt.SessionIdle <= 0 {
		return 0, 0, false
	}
	d, seq := t.opt.Demand.IdleFor()
	return d, seq, d >= t.opt.SessionIdle
}

// dropStaleSession clears whatever the node may still be holding on our proxy
// port, which is the only session this relay could be charged for.
func (t *Tunnel) dropStaleSession(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := t.opt.Client.Disconnect(ctx, t.opt.ProxyPort); err != nil {
		t.log.Debug("no session left over from a previous run", "err", err)
	}
}

// provisionIdentity makes the configured keystore usable by the node: import it
// if the node has never seen it, then unlock it, then require registration.
func (t *Tunnel) provisionIdentity(ctx context.Context) error {
	id := t.opt.Identity

	known := false
	if list, err := t.opt.Client.Identities(ctx); err == nil {
		for _, i := range list {
			if strings.EqualFold(i.ID, id) {
				known = true
				break
			}
		}
	}

	// The compose file seeds the keystore directly, so this only runs when the
	// relay is pointed at a node it does not share a filesystem with.
	if !known && len(t.opt.Keystore) > 0 {
		if _, err := t.opt.Client.ImportIdentity(ctx, t.opt.Keystore, t.opt.Passphrase, true); err != nil {
			return fmt.Errorf("import identity %s into the node: %w", id, err)
		}
		t.log.Info("identity imported", "identity", id)
	}

	if err := t.opt.Client.Unlock(ctx, id, t.opt.Passphrase); err != nil {
		return fmt.Errorf("unlock identity %s (wrong passphrase, or keystore not present in the node): %w", id, err)
	}

	info, err := t.opt.Client.Identity(ctx, id)
	if err != nil {
		return fmt.Errorf("read identity %s: %w", id, err)
	}
	if !info.Registered() {
		return fmt.Errorf("identity %s is %q, not Registered: register and fund it in the Mysterium app before using this relay",
			id, info.RegistrationStatus)
	}
	t.log.Info("identity ready",
		"identity", id,
		"registration", info.RegistrationStatus,
		"balance_myst", info.BalanceTokens.Human,
		"channel", info.ChannelAddress)
	return nil
}

// ensureReady waits for the node and unlocks the identity on it.
func (t *Tunnel) ensureReady(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := t.opt.Client.WaitHealthy(waitCtx, 2*time.Second); err != nil {
		return fmt.Errorf("myst node not ready: %w", err)
	}
	if err := t.opt.Client.Unlock(ctx, t.opt.Identity, t.opt.Passphrase); err != nil {
		return fmt.Errorf("unlock identity %s: %w", t.opt.Identity, err)
	}
	return nil
}

// session picks a provider, connects, and blocks until the session degrades,
// the rotation interval elapses, or ctx is cancelled.
func (t *Tunnel) session(ctx context.Context) error {
	// A node that restarted has forgotten everything: it holds the keystore
	// locked again and answers "unlock required" to every connect. Unlocking
	// per session is idempotent and cheap, and it is what makes the relay
	// survive a node restart instead of retrying forever.
	if err := t.ensureReady(ctx); err != nil {
		return err
	}

	prop, err := t.pickProposal(ctx)
	if err != nil {
		return err
	}

	t.setState(tunnel.StateConnecting, "connecting to "+prop.ProviderID)
	t.log.Info("connecting",
		"provider", prop.ProviderID,
		"country", prop.Location.Country,
		"ip_type", prop.Location.IPType,
		"isp", prop.Location.ISP,
		"quality", prop.Quality.Quality)

	connectCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	// A session left behind by a previous run would make Connect fail.
	_ = t.opt.Client.Disconnect(connectCtx, t.opt.ProxyPort)

	info, err := t.opt.Client.Connect(connectCtx, tequilapi.ConnectRequest{
		ConsumerID:  t.opt.Identity,
		ProviderID:  prop.ProviderID,
		ServiceType: prop.ServiceType,
		ConnectOptions: tequilapi.ConnectOptions{
			DNS:       t.opt.DNSMode,
			ProxyPort: t.opt.ProxyPort,
		},
	})
	if err != nil {
		t.penalise(prop.ProviderID, 10*time.Minute)
		return fmt.Errorf("connect to %s: %w", prop.ProviderID, err)
	}

	port, err := t.discoverUpstream(ctx)
	if err != nil {
		t.penalise(prop.ProviderID, 5*time.Minute)
		return fmt.Errorf("provider %s connected but no proxy listener appeared: %w", prop.ProviderID, err)
	}
	t.mu.Lock()
	t.upstreamPort = port
	t.mu.Unlock()

	status := tunnel.Status{
		State:      tunnel.StateUp,
		ProviderID: prop.ProviderID,
		SessionID:  info.SessionID,
		Country:    prop.Location.Country,
		IPType:     prop.Location.IPType,
		Since:      time.Now(),
	}
	if info.Proposal != nil {
		status.Country = info.Proposal.Location.Country
		status.IPType = info.Proposal.Location.IPType
	}

	// Prove the exit actually carries traffic before announcing the tunnel as
	// usable. A session can establish perfectly and still be useless - a
	// provider whose DNS resolver is dead answers every CONNECT with 502 - and
	// a relay that reported itself healthy in that state would be telling the
	// one lie a kill switch must never tell. Providers are untrusted third
	// parties, so the tunnel is not up until it has completed a real request.
	ip, err := t.proveEgress(ctx)
	if err != nil {
		t.penalise(prop.ProviderID, 30*time.Minute)
		t.closeSession(tunnel.StateDown, "session closed")
		return fmt.Errorf("provider %s established a session but cannot carry traffic: %w", prop.ProviderID, err)
	}
	status.ExitIP = ip
	t.bc.Set(status)
	t.sessions.Add(1)

	t.log.Info("tunnel up",
		"provider", prop.ProviderID,
		"session", info.SessionID,
		"exit_ip", status.ExitIP,
		"country", status.Country,
		"upstream", net.JoinHostPort(t.opt.ProxyHost, strconv.Itoa(port)))

	return t.monitor(ctx, prop.ProviderID)
}

// monitor watches the live session and returns when it is no longer usable.
func (t *Tunnel) monitor(ctx context.Context, provider string) error {
	ticker := time.NewTicker(t.opt.HealthInterval)
	defer ticker.Stop()

	var rotate <-chan time.Time
	if t.opt.RotateEvery > 0 {
		tm := time.NewTimer(t.opt.RotateEvery)
		defer tm.Stop()
		rotate = tm.C
	}

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rotate:
			t.log.Info("rotating exit", "after", t.opt.RotateEvery)
			t.penalise(provider, t.opt.RotateEvery)
			t.closeSession(tunnel.StateConnecting, "rotating exit")
			return nil
		case <-t.suspect:
			// A stream just failed to reach the proxy; check now, not in 5s.
		case <-ticker.C:
		}

		// Nothing has been carried for the idle timeout. From here the session
		// is pure cost - it is billed by the hour whether or not it moves a
		// byte - so close it and wait to be asked for another one.
		//
		// A client that arrives in the moment between this decision and the
		// gate shutting has its stream torn down like any other kill-switch
		// closure, which is the fail-closed direction; its arrival is already
		// recorded, so the retry is answered by the session it started.
		if d, seq, expired := t.idleExpired(); expired {
			t.log.Info("session idle: closing it",
				"idle_for", d.Truncate(time.Second), "timeout", t.opt.SessionIdle, "provider", provider)
			t.closeSession(tunnel.StateIdle, "idle for "+d.Truncate(time.Second).String())
			return idleStop{seq: seq}
		}

		// Cheapest and most decisive check first: the listener is local, so a
		// closed port is immediate and unambiguous evidence.
		if !t.upstreamAlive(ctx) {
			t.setState(tunnel.StateFailed, "proxy listener gone")
			return errors.New("node proxy listener disappeared")
		}

		cs, err := t.opt.Client.Connection(ctx, t.opt.ProxyPort)
		if err != nil {
			failures++
			if failures >= 2 {
				t.setState(tunnel.StateFailed, "node unreachable")
				return fmt.Errorf("node unreachable: %w", err)
			}
			continue
		}
		if !cs.Connected() {
			t.setState(tunnel.StateFailed, "session status "+cs.Status)
			return fmt.Errorf("session left Connected state: %s", cs.Status)
		}
		failures = 0
	}
}

// closeSession asks the node to drop the session and records why the relay is
// no longer carrying traffic - down after a failure, idle when the session was
// closed because nothing was using it.
func (t *Tunnel) closeSession(state tunnel.State, detail string) {
	t.setState(state, detail)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := t.opt.Client.Disconnect(ctx, t.opt.ProxyPort); err != nil {
		t.log.Debug("disconnect returned an error", "err", err)
	}
}

// proveEgress fetches the check URL through the freshly built session and
// returns the address it reports. It dials the node's proxy directly rather
// than going through DialContext, because the tunnel is deliberately not
// marked usable until this succeeds.
//
// With RELAY_EXIT_CHECK_URL empty the check is skipped: the caller then trusts
// the node's session state alone, and a provider that cannot carry traffic
// will look healthy until a client discovers otherwise.
func (t *Tunnel) proveEgress(ctx context.Context) (string, error) {
	if t.opt.ExitCheckURL == "" {
		return "", nil
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:       t.dialer().DialContext,
			DisableKeepAlives: true,
			Proxy:             nil,
		},
	}

	var last error
	for attempt := range 2 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ip, err := t.fetchIP(attemptCtx, client)
		cancel()
		if err == nil {
			return ip, nil
		}
		last = err
	}
	return "", last
}

func (t *Tunnel) fetchIP(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.opt.ExitCheckURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered HTTP %d through the tunnel", t.opt.ExitCheckURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	got := strings.TrimSpace(string(body))
	if net.ParseIP(got) == nil {
		return "", fmt.Errorf("%s returned %q, which is not an address", t.opt.ExitCheckURL, got)
	}
	return got, nil
}

// discoverUpstream waits for the node's proxy listener and returns its port.
func (t *Tunnel) discoverUpstream(ctx context.Context) (int, error) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		for offset := 0; offset <= upstreamScanRange; offset++ {
			port := t.opt.ProxyPort + offset
			if t.portOpen(ctx, port, 2*time.Second) {
				if offset != 0 {
					t.log.Warn("node opened its proxy listener on a shifted port",
						"requested", t.opt.ProxyPort, "actual", port)
				}
				return port, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return 0, fmt.Errorf("no listener on %s:%d-%d",
		t.opt.ProxyHost, t.opt.ProxyPort, t.opt.ProxyPort+upstreamScanRange)
}

func (t *Tunnel) upstreamAlive(ctx context.Context) bool {
	t.mu.RLock()
	port := t.upstreamPort
	t.mu.RUnlock()
	return t.portOpen(ctx, port, 3*time.Second)
}

func (t *Tunnel) portOpen(ctx context.Context, port int, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(t.opt.ProxyHost, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// pickProposal applies the exit policy and returns the best available offer.
func (t *Tunnel) pickProposal(ctx context.Context) (*tequilapi.Proposal, error) {
	policy := t.opt.Exit
	serviceType := policy.ServiceType
	if serviceType == "" {
		serviceType = "wireguard"
	}

	if policy.ProviderID != "" {
		return &tequilapi.Proposal{ProviderID: policy.ProviderID, ServiceType: serviceType}, nil
	}

	filter := tequilapi.ProposalFilter{ServiceType: serviceType, QualityMin: policy.MinQuality}
	// The node filters by a single country/ip_type server-side; any further
	// values in the allow-lists are applied below.
	if len(policy.Countries) == 1 {
		filter.Country = policy.Countries[0]
	}
	if len(policy.IPTypes) == 1 {
		filter.IPType = policy.IPTypes[0]
	}

	all, err := t.opt.Client.Proposals(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}

	candidates := t.filter(all, policy)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no provider matches the exit policy (%d offers seen; country=%v ip_type=%v quality>=%g)",
			len(all), policy.Countries, policy.IPTypes, policy.MinQuality)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].Quality, candidates[j].Quality
		if a.Quality != b.Quality {
			return a.Quality > b.Quality
		}
		if a.Bandwidth != b.Bandwidth {
			return a.Bandwidth > b.Bandwidth
		}
		return a.Latency < b.Latency
	})

	// Pick randomly from the top of the list so repeated reconnects do not all
	// stampede the single best-scoring provider.
	top := min(len(candidates), 5)
	choice := candidates[rand.Intn(top)]
	return &choice, nil
}

func (t *Tunnel) filter(all []tequilapi.Proposal, policy config.ExitPolicy) []tequilapi.Proposal {
	now := time.Now()
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]tequilapi.Proposal, 0, len(all))
	for _, p := range all {
		if until, ok := t.cooldown[p.ProviderID]; ok && now.Before(until) {
			continue
		}
		if contains(policy.Exclude, p.ProviderID) {
			continue
		}
		if len(policy.Countries) > 0 && !contains(policy.Countries, p.Location.Country) {
			continue
		}
		if len(policy.IPTypes) > 0 && !contains(policy.IPTypes, p.Location.IPType) {
			continue
		}
		if policy.MinQuality > 0 && p.Quality.Quality < policy.MinQuality {
			continue
		}
		// Latency 0 means "not measured", not "instant"; do not let it pass a
		// max-latency filter that a measured node would fail.
		if policy.MaxLatency > 0 && p.Quality.Latency > float64(policy.MaxLatency.Milliseconds()) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// penalise puts a provider on cooldown so a bad exit is not picked again
// immediately.
func (t *Tunnel) penalise(provider string, d time.Duration) {
	if provider == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cooldown[provider] = time.Now().Add(d)
	// Keep the map from growing without bound over long uptimes.
	if len(t.cooldown) > 512 {
		now := time.Now()
		for k, v := range t.cooldown {
			if now.After(v) {
				delete(t.cooldown, k)
			}
		}
	}
}

func (t *Tunnel) setState(s tunnel.State, detail string) {
	next := tunnel.Status{State: s, Detail: detail}
	if s == tunnel.StateUp {
		next = t.bc.Status()
		next.State = s
		next.Detail = detail
	}
	t.bc.Set(next)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
