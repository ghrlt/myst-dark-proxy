package guard

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func testLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, nil))
}

func TestRun_PassesThroughNormalReturn(t *testing.T) {
	var buf bytes.Buffer
	log := testLogger(&buf)

	want := errors.New("boom")
	got := Run(log, "thing", func() error { return want })
	if !errors.Is(got, want) {
		t.Fatalf("Run() = %v, want %v", got, want)
	}
	if buf.Len() != 0 {
		t.Fatalf("Run() logged on a non-panicking call: %s", buf.String())
	}
}

func TestRun_RecoversPanic(t *testing.T) {
	var buf bytes.Buffer
	log := testLogger(&buf)

	err := Run(log, "thing", func() error { panic("kaboom") })
	if err == nil {
		t.Fatal("Run() returned nil error after a panic")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("error %q does not mention the panic value", err)
	}
	if !strings.Contains(buf.String(), "kaboom") {
		t.Errorf("nothing logged about the panic: %s", buf.String())
	}
}

func TestGo_RecoversPanic(t *testing.T) {
	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	// wroteOnce signals after the log write completes, not after the panicking
	// goroutine merely returns: recoverAndLog's write happens in a defer that
	// only runs once fn (and its own defers) have already unwound, so signalling
	// any earlier would race the assertion below against that write.
	log := testLogger(&wroteOnce{buf: &buf, done: &wg})

	Go(log, "thing", func() { panic("kaboom") })
	wg.Wait()

	if !strings.Contains(buf.String(), "kaboom") {
		t.Errorf("nothing logged about the panic: %s", buf.String())
	}
}

// wroteOnce signals done the first time it is written to.
type wroteOnce struct {
	buf  *bytes.Buffer
	done *sync.WaitGroup
	once sync.Once
}

func (w *wroteOnce) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.once.Do(w.done.Done)
	return n, err
}
