package node

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunOnceSurfacesClearStaleFailureAsAnOrdinaryError proves that a port
// still held by something else comes back from runOnce as a plain error -
// the same shape as once() failing - rather than being special-cased. Run's
// retry loop only knows how to back off and try again on an error; it does not
// know how to recover from clearStale being fatal on its own, which is exactly
// what moving the call inside runOnce was for.
func TestRunOnceSurfacesClearStaleFailureAsAnOrdinaryError(t *testing.T) {
	// Something else - not a node of ours - holds this port, and no pidfile
	// names a culprit of ours to stop. clearStale must refuse rather than kill
	// a process it has no record of, and runOnce must hand that refusal back
	// as this call's own error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	s := &Supervisor{
		DataDir:     t.TempDir(),
		TequilaHost: host,
		TequilaPort: mustAtoi(t, port),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	err = s.runOnce(context.Background())
	if err == nil {
		t.Fatal("runOnce succeeded with the port held by something unrelated")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("error %q does not explain that the port is taken", err)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not a port number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// A restart asked for by the tunnel replaces the node at once - no backoff, no
// "node exited" warning - and Restart does not return until the old process is
// gone, so the caller's next question cannot be answered by the node on its
// way out.
func TestRestartReplacesTheNodeWithoutBackoff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the node")
	}
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	script := filepath.Join(dir, "fake-myst")
	body := "#!/bin/sh\necho $$ >> " + starts + "\nexec sleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// A free port, so clearStale has nothing to clear.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	s := &Supervisor{
		Binary:      script,
		DataDir:     dir,
		TequilaHost: "127.0.0.1",
		TequilaPort: mustAtoi(t, port),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		// A backoff long enough that a restart taking it would fail the test.
		RestartMin: time.Minute,
		RestartMax: time.Minute,
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	defer func() {
		cancel()
		<-runDone
	}()

	first := waitForStarts(t, starts, 1)[0]

	rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rcancel()
	if err := s.Restart(rctx, "test"); err != nil {
		t.Fatal(err)
	}
	if pidAlive(first) {
		t.Fatalf("Restart returned while the old node (pid %d) was still running", first)
	}

	pids := waitForStarts(t, starts, 2)
	if pids[1] == first {
		t.Fatal("the same process was reported twice")
	}
}

func waitForStarts(t *testing.T, path string, n int) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(path)
		var pids []int
		for _, f := range strings.Fields(string(raw)) {
			pids = append(pids, mustAtoi(t, f))
		}
		if len(pids) >= n {
			return pids
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("node was not started %d times", n)
	return nil
}

func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
