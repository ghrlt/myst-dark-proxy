package node

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
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
