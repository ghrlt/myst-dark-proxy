// Package guard keeps a panic in one goroutine from taking the whole relay
// down with it.
//
// A background process has no console anyone is watching: an unrecovered
// panic prints a stack trace to stderr and vanishes with it. Every goroutine
// that handles a client, a subsystem, or anything else that can legitimately
// fail should run through Go or Run instead of a bare "go func() {...}()", so
// the failure is recovered, logged to the same file everything else logs to,
// and handled the way any other error from that goroutine would be.
package guard

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Go runs fn on its own goroutine. A panic inside it is recovered and logged
// against log, naming what panicked (component), instead of crashing the
// process. Use this for goroutines whose failure should stay local - a single
// client's connection, for instance - and where there is nothing further to
// report the failure to.
func Go(log *slog.Logger, component string, fn func()) {
	go func() {
		defer recoverAndLog(log, component)
		fn()
	}()
}

// Run calls fn and returns what it returns. A panic inside fn is recovered,
// logged against log naming component, and turned into an error instead of
// crashing the process - so a caller feeding the result to the same fail/retry
// path it already uses for an ordinary error handles a panic exactly the same
// way.
func Run(log *slog.Logger, component string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logPanic(log, component, r)
			err = fmt.Errorf("%s: panic: %v", component, r)
		}
	}()
	return fn()
}

func recoverAndLog(log *slog.Logger, component string) {
	if r := recover(); r != nil {
		logPanic(log, component, r)
	}
}

func logPanic(log *slog.Logger, component string, r any) {
	log.Error("recovered from panic", "component", component, "panic", r, "stack", string(debug.Stack()))
}
