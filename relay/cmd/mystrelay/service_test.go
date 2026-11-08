package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeRelay answers /status the way a running relay does.
func fakeRelay() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		var rep statusReport
		rep.Tunnel.State = "idle"
		rep.Tunnel.Detail = "no client is asking for egress"
		_ = json.NewEncoder(w).Encode(rep)
	})
}

// freeAddr returns a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func clientFor(addr string) *relayClient {
	return &relayClient{base: "http://" + addr, http: &http.Client{Timeout: 2 * time.Second}}
}

func TestStartServiceWithNothingInstalledSaysHowToInstallOne(t *testing.T) {
	err := startService(serviceInfo{}, clientFor(freeAddr(t)), time.Second, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "autostart install") {
		t.Fatalf("got %v, want a pointer to autostart install", err)
	}
}

// "Started" has to mean the relay answers, not that the manager said yes: the
// command waits for the relay the service brings up, and reports its state.
func TestStartServiceStartsItAndWaitsForTheRelayToAnswer(t *testing.T) {
	addr := freeAddr(t)
	var srv *http.Server
	t.Cleanup(func() {
		if srv != nil {
			srv.Close()
		}
	})
	started := 0
	svc := serviceInfo{
		Installed: true, Via: "test unit", KnowsActive: true,
		Start: func() error {
			started++
			// The relay takes a moment to come up after the manager returns.
			go func() {
				time.Sleep(700 * time.Millisecond)
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					return
				}
				srv = &http.Server{Handler: fakeRelay()}
				_ = srv.Serve(ln)
			}()
			return nil
		},
	}
	var out bytes.Buffer
	if err := startService(svc, clientFor(addr), 10*time.Second, &out); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("service started %d times, want 1", started)
	}
	if !strings.Contains(out.String(), "relay started (test unit)") || !strings.Contains(out.String(), "tunnel idle") {
		t.Fatalf("output %q does not say the relay came up and what it is doing", out.String())
	}
}

func TestStartServiceReportsARelayThatNeverAnswers(t *testing.T) {
	svc := serviceInfo{
		Installed: true, Via: "test unit", KnowsActive: true, Logs: "journalctl --user -u myst-relay",
		Start: func() error { return nil },
	}
	err := startService(svc, clientFor(freeAddr(t)), time.Second, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "has not answered") || !strings.Contains(err.Error(), "journalctl") {
		t.Fatalf("got %v, want a timeout that says where to look", err)
	}
}

func TestStartServiceLeavesARunningServiceAlone(t *testing.T) {
	srv := httptest.NewServer(fakeRelay())
	defer srv.Close()
	svc := serviceInfo{
		Installed: true, Via: "test unit", Active: true, KnowsActive: true,
		Start: func() error { t.Fatal("started a service that was already running"); return nil },
	}
	var out bytes.Buffer
	if err := startService(svc, clientFor(strings.TrimPrefix(srv.URL, "http://")), time.Second, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already running") {
		t.Fatalf("output %q does not say it was already running", out.String())
	}
}

// A relay started by hand holds the ports. Starting the service next to it
// would only buy a unit that fails to bind and restarts forever.
func TestStartServiceRefusesWhenARelayRunsOutsideTheService(t *testing.T) {
	srv := httptest.NewServer(fakeRelay())
	defer srv.Close()
	svc := serviceInfo{
		Installed: true, Via: "test unit", Active: false, KnowsActive: true,
		Start: func() error { t.Fatal("started the service next to a relay already holding its ports"); return nil },
	}
	err := startService(svc, clientFor(strings.TrimPrefix(srv.URL, "http://")), time.Second, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not as the service") || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("got %v, want a refusal that says to stop the other relay", err)
	}
}
