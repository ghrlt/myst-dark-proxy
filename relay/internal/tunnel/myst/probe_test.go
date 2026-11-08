package myst

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// A listener that refuses one probe and answers the next - as one under a
// burst of connects does - is not a lost session.
func TestOneRefusedProbeDoesNotEndTheSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)

	tn := New(Options{ProxyHost: "127.0.0.1", ProxyPort: port, Logger: quietLog()})

	// Back before the second probe.
	go func() {
		time.Sleep(listenerProbeGap / 2)
		if l, err := net.Listen("tcp", addr); err == nil {
			t.Cleanup(func() { l.Close() })
		}
	}()
	if !tn.upstreamAlive(context.Background()) {
		t.Fatal("a listener that came back on the second probe was declared gone")
	}
}

// One that stays gone is still found out.
func TestAListenerThatStaysGoneIsDeclaredGone(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	port, _ := strconv.Atoi(p)

	tn := New(Options{ProxyHost: "127.0.0.1", ProxyPort: port, Logger: quietLog()})
	start := time.Now()
	if tn.upstreamAlive(context.Background()) {
		t.Fatal("a closed listener was reported alive")
	}
	if d := time.Since(start); d > time.Duration(listenerProbes)*listenerProbeGap+time.Second {
		t.Fatalf("took %s to notice", d)
	}
}
