// Command mystrelay is the myst-relay service.
//
// It maintains one Mysterium session through a myst node running in proxy mode
// and republishes it on fixed local ports: SOCKS5, HTTP, and raw port-forward.
// Every one of those ports is fail-closed - while the tunnel is not up, clients
// are refused, never quietly served over the host's own connection.
//
// The session follows the clients rather than the relay's own uptime: providers
// charge by the hour as well as by the gibibyte, so by default none is opened
// until a client asks for egress, and one that has carried nothing for the idle
// timeout is closed again.
//
// Two deployment shapes share this binary. Under Docker the node runs as its
// own container and the relay attaches to it. On a desktop the relay downloads
// and supervises the node itself, so one executable is the whole service.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/demand"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend/httpproxy"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend/raw"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend/socks5"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/health"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/node"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/selfupdate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel/myst"
)

// version is stamped at build time.
var version = "dev"

const usage = `myst-relay - fail-closed proxy relay over the Mysterium network

usage: mystrelay <command> [flags]

commands:
  setup        Configure this machine: identity, ports, node download, autostart
  run          Run the relay in the foreground (what autostart invokes)
  status       Show what the running relay is doing, and what it has cost
  balance      Show what the identity paying for the exits has left
  rotate       Leave this exit node and pick another one now
  stop         Stop the running relay
  tray         Put a status icon in the notification area (Windows, Linux)
  verify       Prove traffic leaves through the exit node, and that it stops
               when the tunnel does
  autostart    install | remove | status
  update       Check for a newer release and install it over this binary
  version      Print the version

Run "mystrelay <command> -h" for a command's flags.
Configuration is read from %s, then overridden by environment variables.
`

func main() {
	// The container image ships no shell, so its healthcheck re-executes this
	// binary instead of shelling out to wget.
	if len(os.Args) > 1 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		os.Exit(selfCheck())
	}

	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "setup":
		err = cmdSetup(args)
	case "status":
		err = cmdStatus(args)
	case "balance":
		err = cmdBalance(args)
	case "rotate":
		err = cmdRotate(args)
	case "stop":
		err = cmdStop(args)
	case "tray":
		err = cmdTray(args)
	case "verify":
		err = cmdVerify(args)
	case "autostart":
		err = cmdAutostart(args)
	case "update":
		err = cmdUpdate(args)
	case "version":
		fmt.Printf("myst-relay %s (node %s, %s)\n", version, node.DefaultVersion, node.Platform())
	case "help", "-h", "--help":
		fmt.Printf(usage, config.ConfigPath())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprintf(os.Stderr, usage, config.ConfigPath())
		os.Exit(2)
	}

	var status statusExit
	if errors.As(err, &status) {
		os.Exit(int(status))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

// statusExit ends the process with a particular status and prints nothing: the
// command has already said what happened. "update -check" reports an available
// update this way, which is a finding rather than a failure.
type statusExit int

func (c statusExit) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

// loadConfig reads the config file, then the environment.
func loadConfig(explicit string) (*config.Config, error) {
	path := explicit
	if path == "" {
		path = config.ConfigPath()
	}
	if err := config.LoadFile(path); err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("%w\n\nSettings are read from %s and the environment.\nRun \"mystrelay setup\" to create that file", err, path)
	}
	return cfg, nil
}

func cmdRun(args []string) error {
	fs := newFlagSet("run")
	cfgPath := fs.String("config", "", "path to config.env")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// An update on Windows cannot delete the binary it replaced - it was the
	// running image at the time. This is a later start, so it can.
	if exe, err := selfupdate.Running(); err == nil {
		selfupdate.CleanOld(exe)
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	return run(cfg)
}

func run(cfg *config.Config) error {
	log, closeLog, err := newLogger(cfg.LogLevel, cfg.LogFile)
	if err != nil {
		return err
	}
	defer closeLog()
	slog.SetDefault(log)

	log.Info("myst-relay starting",
		"version", version,
		"identity", cfg.IdentityAddress,
		"managed_node", cfg.Managed,
		"kill_switch", killSwitchMode(cfg.StrictKillSwitch),
		"sessions", sessionPolicy(cfg))

	// An autostart registration written before the status icon existed still
	// starts the relay and nothing else, and replacing the binary leaves it
	// that way. This is where that is noticed and put right, on a goroutine
	// because it asks the machine's scheduler and nothing below waits on the
	// answer.
	go adoptStatusIcon(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		runErr  error
	)
	fail := func(what string, err error) {
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}
		errOnce.Do(func() {
			runErr = fmt.Errorf("%s: %w", what, err)
			cancel()
		})
	}

	// A desktop install owns its node: fetch it, seed the identity into its
	// keystore, and keep the process alive for as long as we run.
	if cfg.Managed {
		binary, err := node.Ensure(runCtx, cfg.NodeDir, cfg.NodeVersion, log)
		if err != nil {
			return err
		}
		if err := seedKeystore(cfg); err != nil {
			return err
		}
		sup := &node.Supervisor{
			Binary:      binary,
			DataDir:     cfg.NodeDataDir,
			TequilaHost: cfg.TequilaHost,
			TequilaPort: cfg.TequilaPort,
			ProxyHost:   cfg.TequilaHost,
			ProxyPort:   cfg.ProxyPort,
			LogPath:     filepath.Join(cfg.NodeDataDir, "logs", "node.log"),
			Log:         log,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			fail("node", sup.Run(runCtx))
		}()
	}

	api := tequilapi.New(cfg.TequilaBaseURL, cfg.TequilaUser, cfg.TequilaPass, &http.Client{Timeout: 60 * time.Second})

	keystore, err := cfg.KeystoreJSON()
	if err != nil {
		return err
	}

	// A session costs by the hour as well as by the byte, so unless it is
	// configured to stay up regardless, the tracker is what ties its lifetime
	// to actual clients: the frontends announce demand, the supervisor opens
	// and closes sessions to match.
	var wants *demand.Tracker
	if cfg.SessionsFollowClients() {
		wants = demand.New()
	}

	egress := myst.New(myst.Options{
		Client:         api,
		Identity:       cfg.IdentityAddress,
		Keystore:       keystore,
		Passphrase:     cfg.IdentityPassphrase,
		Exit:           cfg.Exit,
		ProxyHost:      cfg.ProxyHost,
		ProxyPort:      cfg.ProxyPort,
		DNSMode:        cfg.DNSMode,
		ExitCheckURL:   cfg.ExitCheckURL,
		DialTimeout:    cfg.DialTimeout,
		HealthInterval: cfg.HealthInterval,
		ReconnectMin:   cfg.ReconnectMin,
		ReconnectMax:   cfg.ReconnectMax,
		RotateEvery:    cfg.RotateEvery,
		Demand:         wants,
		OnDemand:       cfg.OnDemand,
		SessionIdle:    cfg.SessionIdle,
		Logger:         log,
	})

	killSwitch := gate.New(ctx, cfg.StrictKillSwitch)
	defer killSwitch.Shutdown()

	base := frontend.Base{
		Dialer:      egress,
		Gate:        killSwitch,
		Log:         log,
		DialTimeout: cfg.DialTimeout,
		IdleTimeout: cfg.IdleTimeout,
		Demand:      wants,
		ReadyWait:   cfg.ReadyTimeout,
	}

	var frontends []frontend.Frontend
	if cfg.SocksListen != "" {
		b := base
		b.Listen = cfg.SocksListen
		frontends = append(frontends, socks5.New(b, cfg.Auth.Username, cfg.Auth.Password))
	}
	if cfg.HTTPListen != "" {
		b := base
		b.Listen = cfg.HTTPListen
		frontends = append(frontends, httpproxy.New(b, cfg.Auth.Username, cfg.Auth.Password))
	}
	for _, f := range cfg.Forwards {
		b := base
		b.Listen = f.Listen
		frontends = append(frontends, raw.New(b, f.Target))
	}

	endpoints := make([]health.Endpoint, 0, len(frontends))
	for _, f := range frontends {
		endpoints = append(endpoints, health.Endpoint{Name: f.Name(), Addr: f.Addr()})
	}

	// The tunnel drives the kill switch: the gate is open exactly while the
	// egress path is usable, and closes the instant it is not.
	wg.Add(1)
	go func() {
		defer wg.Done()
		driveKillSwitch(runCtx, egress, killSwitch, wants != nil, log)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		fail("tunnel", egress.Run(runCtx))
	}()

	for _, f := range frontends {
		wg.Add(1)
		go func(f frontend.Frontend) {
			defer wg.Done()
			fail("frontend "+f.Name(), f.Serve(runCtx))
		}(f)
	}

	if cfg.HealthListen != "" {
		// The control endpoints exist only when there is a secret to check
		// callers against. Failing to write one is not a reason to refuse to
		// run: the relay carries traffic exactly as before, it just answers
		// questions rather than taking instructions.
		token, err := ensureControlToken(cfg)
		if err != nil {
			log.Info("controls off: no secret could be stored", "err", err)
		}
		hs := &health.Server{
			Listen:      cfg.HealthListen,
			Version:     version,
			Tunnel:      egress,
			Gate:        killSwitch,
			Demand:      wants,
			OnDemand:    cfg.OnDemand,
			SessionIdle: cfg.SessionIdle,
			Endpoints:   endpoints,
			Log:         log,
			Started:     time.Now(),
			Token:       token,
			Stop:        cancel,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			fail("health", hs.Serve(runCtx))
		}()
	}

	<-runCtx.Done()
	log.Info("shutting down")
	killSwitch.Shutdown()
	wg.Wait()
	return runErr
}

// seedKeystore places the configured identity where a managed node will find
// it, so the node is asked to unlock an identity it already holds - the same
// arrangement the Docker deployment gets from a read-only bind mount.
func seedKeystore(cfg *config.Config) error {
	dir := filepath.Join(cfg.NodeDataDir, "keystore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	want, err := cfg.KeystoreJSON()
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, "identity.json")
	if have, err := os.ReadFile(dest); err == nil && string(have) == string(want) {
		return nil
	}
	return os.WriteFile(dest, want, 0o600)
}

// driveKillSwitch mirrors tunnel state onto the gate.
//
// onDemand changes what a shut gate means. With sessions tied to clients, "no
// session" and "building one" are resting states, not faults: the gate is armed
// instead of closed, so a client that arrives waits for the session its own
// arrival started rather than being refused. Anything else - a failure, a node
// that went away - still shuts the gate outright and refuses immediately, which
// is what keeps a broken tunnel from looking like a slow one.
func driveKillSwitch(ctx context.Context, t tunnel.Tunnel, g *gate.Gate, onDemand bool, log *slog.Logger) {
	updates, cancel := t.Watch()
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case st, ok := <-updates:
			if !ok {
				return
			}
			if st.State.Usable() {
				g.Open()
				log.Info("kill switch open",
					"provider", st.ProviderID, "exit_ip", st.ExitIP, "country", st.Country)
				continue
			}
			reason := string(st.State)
			if st.Detail != "" {
				reason += ": " + st.Detail
			}
			if onDemand && (st.State == tunnel.StateIdle || st.State == tunnel.StateConnecting) {
				g.Arm(reason)
				log.Info("kill switch armed", "reason", reason)
				continue
			}
			g.Close(reason)
			log.Warn("kill switch closed", "reason", reason)
		}
	}
}

// selfCheck queries the local health endpoint and maps it to an exit code.
func selfCheck() int {
	listen := os.Getenv("RELAY_HEALTH_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:7348"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: bad RELAY_HEALTH_LISTEN:", err)
		return 2
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: HTTP %d: %s\n", resp.StatusCode, body)
		return 1
	}
	return 0
}

// sessionPolicy describes the session lifecycle in one line, for the log.
func sessionPolicy(cfg *config.Config) string {
	switch {
	case cfg.OnDemand && cfg.SessionIdle > 0:
		return "opened when a client asks, closed after " + cfg.SessionIdle.String() + " idle"
	case cfg.OnDemand:
		return "opened when a client asks, then kept open"
	case cfg.SessionIdle > 0:
		return "opened at startup, closed after " + cfg.SessionIdle.String() + " idle"
	}
	return "opened at startup and kept open"
}

func killSwitchMode(strict bool) string {
	if strict {
		return "strict (established streams are dropped when the tunnel falls)"
	}
	return "permissive (established streams may drain)"
}

// newLogger writes to stdout, or to a file when one is configured - which is
// what an autostarted background process needs, having no console to write to.
func newLogger(level, file string) (*slog.Logger, func(), error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	out := io.Writer(os.Stdout)
	closer := func() {}
	if file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return nil, nil, err
		}
		if st, err := os.Stat(file); err == nil && st.Size() > 16<<20 {
			_ = os.Remove(file)
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", file, err)
		}
		out = f
		closer = func() { _ = f.Close() }
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: lvl})), closer, nil
}
