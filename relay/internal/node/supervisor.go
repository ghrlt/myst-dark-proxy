package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/logfile"
)

// Supervisor runs a Mysterium node as a child process and keeps it alive.
type Supervisor struct {
	Binary      string
	DataDir     string
	TequilaHost string
	TequilaPort int
	ProxyHost   string
	ProxyPort   int
	// LogPath is the base name of the node's log, written one file per day
	// beside it (node-2026-11-09.log); LogKeepDays of them are kept, 0 all.
	LogPath     string
	LogKeepDays int
	ExtraArgs   []string
	Log         *slog.Logger

	// RestartMin/RestartMax bound the backoff between respawns.
	RestartMin time.Duration
	RestartMax time.Duration

	restartOnce sync.Once
	restarts    chan restartRequest

	logOnce sync.Once
	logW    *logfile.Writer
	logErr  error
}

// nodeLogPartSize is how large one file of the node's log grows before the day
// continues in the next part. The node logs at debug, every WireGuard
// handshake and keepalive, so a busy day is large.
const nodeLogPartSize = 128 << 20

// restartRequest asks Run to replace the node. done is closed once the process
// that was running has exited.
type restartRequest struct {
	reason string
	done   chan struct{}
}

// errRestarted ends a run that Restart asked for, so Run can tell it apart from
// a node that died on its own.
var errRestarted = errors.New("node restarted on request")

func (s *Supervisor) requests() chan restartRequest {
	s.restartOnce.Do(func() { s.restarts = make(chan restartRequest) })
	return s.restarts
}

// Restart replaces the running node with a fresh process, without the backoff
// or the warning a crash earns. It returns once the old process has exited, so
// whatever the caller asks the node next is answered by the new one rather than
// by the one on its way out; waiting for the new one to be ready is the
// caller's business, as it is after any other start.
//
// Called while the node is down between respawns, it skips the rest of the
// backoff instead: the process about to start is already a fresh one.
func (s *Supervisor) Restart(ctx context.Context, reason string) error {
	req := restartRequest{reason: reason, done: make(chan struct{})}
	select {
	case s.requests() <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-req.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Args builds the node command line. The desktop build has no entrypoint
// script, so every directory the node uses is named explicitly and kept inside
// our own data directory rather than scattered through the user profile.
func (s *Supervisor) Args() []string {
	args := []string{
		"--config-dir=" + s.DataDir,
		"--data-dir=" + s.DataDir,
		"--runtime-dir=" + filepath.Join(s.DataDir, "run"),
		"--log-dir=" + filepath.Join(s.DataDir, "logs"),
		"--consumer",
		"--proxymode",
		"--tequilapi.address=" + s.TequilaHost,
		"--tequilapi.port=" + strconv.Itoa(s.TequilaPort),
		"--proxy.bind.address=" + s.ProxyHost,
		"--ui.enable=false",
		// SSDP/Bonjour chatter is useless to a consumer and noisy on a LAN.
		"--local-service-discovery=false",
	}
	args = append(args, s.ExtraArgs...)
	return append(args, "daemon")
}

// Run starts the node and restarts it whenever it exits, until ctx is done.
func (s *Supervisor) Run(ctx context.Context) error {
	if s.RestartMin <= 0 {
		s.RestartMin = 2 * time.Second
	}
	if s.RestartMax <= 0 {
		s.RestartMax = 60 * time.Second
	}
	for _, d := range []string{s.DataDir, filepath.Join(s.DataDir, "run"), filepath.Join(s.DataDir, "logs"), filepath.Join(s.DataDir, "keystore")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	backoff := s.RestartMin
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errRestarted) {
			backoff = s.RestartMin
			continue
		}
		// A node that ran for a while and then died is a transient failure; one
		// that dies immediately is usually misconfiguration, so back off.
		if time.Since(start) > time.Minute {
			backoff = s.RestartMin
		}
		s.Log.Warn("node exited, restarting", "err", err, "ran_for", time.Since(start).Truncate(time.Second), "retry_in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-s.requests():
			close(req.done)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.RestartMax)
	}
}

// runOnce clears whatever a crashed previous run left behind and starts the
// node. clearStale failing is folded into the same retry loop as the node
// itself exiting, rather than being fatal to the whole relay: a port a killed
// node has not yet let go of is exactly the kind of thing more time fixes, and
// under load - the condition a crash is more likely to happen under in the
// first place - "more time" can be longer than one fixed wait is willing to
// give it.
func (s *Supervisor) runOnce(ctx context.Context) error {
	if err := s.clearStale(); err != nil {
		return err
	}
	return s.once(ctx)
}

func (s *Supervisor) once(ctx context.Context) error {
	logW, err := s.openLog()
	if err != nil {
		return err
	}

	cmd := exec.Command(s.Binary, s.Args()...)
	cmd.Dir = s.DataDir
	// Not a file: the node's output is copied through the day-by-day writer,
	// which is what lets it move to the next day's file while the node keeps
	// running. The copy ends when the node closes its end, and WaitDelay
	// bounds that, so an orphan holding the pipe cannot keep Wait from
	// returning after the node itself has gone.
	cmd.Stdout = logW
	cmd.Stderr = logW
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start node %s: %w", s.Binary, err)
	}
	s.Log.Info("node started", "pid", cmd.Process.Pid, "log", logW.Path())
	s.writePID(cmd.Process.Pid)
	defer s.removePID()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case req := <-s.requests():
		s.Log.Info("restarting node", "pid", cmd.Process.Pid, "reason", req.reason)
		s.stop(cmd, done)
		close(req.done)
		return errRestarted
	case <-ctx.Done():
		s.Log.Info("stopping node", "pid", cmd.Process.Pid)
		s.stop(cmd, done)
		return ctx.Err()
	}
}

// stop asks the node to quit, kills it if it will not, and returns once it has
// exited - which exited, the result of the one cmd.Wait, is what says so.
// Windows has no meaningful signal to send, so termination is the only lever
// there, and it is pulled at once rather than after the grace period.
func (s *Supervisor) stop(cmd *exec.Cmd, exited <-chan error) {
	if err := cmd.Process.Signal(os.Interrupt); err == nil {
		select {
		case <-exited:
			return
		case <-time.After(10 * time.Second):
		}
	}
	_ = cmd.Process.Kill()
	<-exited
}

// openLog opens the node's log once for the supervisor's lifetime: every node
// it starts writes to the same day-by-day writer.
func (s *Supervisor) openLog() (*logfile.Writer, error) {
	s.logOnce.Do(func() {
		if s.LogPath == "" {
			s.LogPath = filepath.Join(s.DataDir, "logs", "node.log")
		}
		s.logW, s.logErr = logfile.Open(s.LogPath, s.LogKeepDays, nodeLogPartSize)
	})
	return s.logW, s.logErr
}

func (s *Supervisor) pidPath() string { return filepath.Join(s.DataDir, "node.pid") }

func (s *Supervisor) writePID(pid int) {
	_ = os.WriteFile(s.pidPath(), []byte(strconv.Itoa(pid)), 0o644)
}

func (s *Supervisor) removePID() { _ = os.Remove(s.pidPath()) }

// clearStale deals with a node left behind by a relay that was killed rather
// than shut down. It only acts when the API port is actually occupied and a
// pidfile from a previous run names the culprit, so an unrelated process that
// happens to reuse the pid is not caught in the crossfire.
func (s *Supervisor) clearStale() error {
	addr := net.JoinHostPort(s.TequilaHost, strconv.Itoa(s.TequilaPort))
	if !portBusy(addr) {
		return nil
	}

	raw, err := os.ReadFile(s.pidPath())
	if err != nil {
		return fmt.Errorf("port %s is already in use and no previous node of ours is recorded.\n"+
			"Something else holds the node API port - another Mysterium app, or a node left by a different install.\n"+
			"Close it, or set MYST_TEQUILAPI_PORT to a free port", addr)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("port %s is in use and %s is unreadable; close whatever holds the port", addr, s.pidPath())
	}

	s.Log.Warn("a node from a previous run is still holding the API port, stopping it", "pid", pid, "addr", addr)
	proc, err := os.FindProcess(pid)
	if err == nil {
		_ = proc.Kill()
	}
	// A killed process does not always let go of its listening socket the
	// instant it dies - Windows in particular can lag here under load, which is
	// also exactly when a node is more likely to have been killed rather than
	// shut down cleanly in the first place. This is generous rather than fussy
	// about it: runOnce folds a failure here into the supervisor's own
	// restart-with-backoff loop, so giving up too early costs nothing but a
	// warning and another lap, while giving up too late holds up every attempt
	// to free the port during this one.
	const (
		clearStaleWait  = 200 * time.Millisecond
		clearStaleTries = 150 // 30s
	)
	for range clearStaleTries {
		if !portBusy(addr) {
			s.removePID()
			return nil
		}
		time.Sleep(clearStaleWait)
	}
	return fmt.Errorf("could not free %s after %s; stop the leftover node process (pid %d) by hand",
		addr, time.Duration(clearStaleTries)*clearStaleWait, pid)
}

func portBusy(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// ErrNotManaged is returned when no managed node is configured.
var ErrNotManaged = errors.New("node: not running a managed node")
