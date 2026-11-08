//go:build loadtest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/demand"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend/httpproxy"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/frontend/socks5"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/guard"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel/myst"
)

// simNode is a myst node in proxy mode: a tequilapi plus a CONNECT listener
// that exists only while a session does, and a session that the "provider"
// drops on a schedule.
type simNode struct {
	t         *testing.T
	proxyAddr string
	connectIn time.Duration
	dropAfter time.Duration
	// acceptDelay makes the node's proxy slow to accept, as a node busy
	// building a session or under a herd is.
	acceptDelay time.Duration

	mu       sync.Mutex
	ln       net.Listener
	status   string
	session  int
	connects atomic.Int64
	drops    atomic.Int64
}

func (n *simNode) openProxy() error {
	ln, err := net.Listen("tcp", n.proxyAddr)
	if err != nil {
		return err
	}
	n.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if n.acceptDelay > 0 {
				time.Sleep(n.acceptDelay)
			}
			go n.serveConnect(c)
		}
	}()
	return nil
}

func (n *simNode) serveConnect(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 4096)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := 0
	for !strings.Contains(string(buf[:got]), "\r\n\r\n") {
		k, err := c.Read(buf[got:])
		if err != nil {
			return
		}
		got += k
	}
	_ = c.SetReadDeadline(time.Time{})
	line := strings.SplitN(string(buf[:got]), " ", 3)
	if len(line) < 2 {
		return
	}
	up, err := net.DialTimeout("tcp", line[1], 5*time.Second)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer up.Close()
	_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func (n *simNode) closeProxy() {
	if n.ln != nil {
		_ = n.ln.Close()
		n.ln = nil
	}
	n.status = "NotConnected"
}

func (n *simNode) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		js := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch {
		case r.URL.Path == "/healthcheck":
			js(map[string]any{"version": "sim"})
		case r.URL.Path == "/auth/authenticate":
			js(map[string]string{"token": "t"})
		case r.URL.Path == "/identities":
			js(map[string]any{"identities": []map[string]string{{"id": "0xme"}}})
		case strings.HasSuffix(r.URL.Path, "/unlock"):
			w.WriteHeader(http.StatusAccepted)
		case strings.HasPrefix(r.URL.Path, "/identities/"):
			js(map[string]any{"id": "0xme", "registration_status": "Registered"})
		case r.URL.Path == "/proposals":
			var ps []tequilapi.Proposal
			for i := range 20 {
				ps = append(ps, tequilapi.Proposal{ProviderID: fmt.Sprintf("0xP%02d", i), ServiceType: "wireguard",
					Quality: tequilapi.Quality{Quality: 3}})
			}
			js(map[string]any{"proposals": ps})
		case r.URL.Path == "/connection/statistics":
			js(map[string]any{})
		case r.URL.Path == "/connection" && r.Method == http.MethodPut:
			n.connects.Add(1)
			time.Sleep(n.connectIn)
			n.mu.Lock()
			if err := n.openProxy(); err != nil {
				n.mu.Unlock()
				w.WriteHeader(500)
				js(map[string]string{"err": err.Error()})
				return
			}
			n.status = "Connected"
			n.session++
			id := n.session
			n.mu.Unlock()
			if n.dropAfter > 0 {
				time.AfterFunc(n.dropAfter, func() {
					n.mu.Lock()
					defer n.mu.Unlock()
					if n.session == id && n.status == "Connected" {
						n.drops.Add(1)
						n.closeProxy()
					}
				})
			}
			js(map[string]any{"status": "Connected", "session_id": strconv.Itoa(id)})
		case r.URL.Path == "/connection" && r.Method == http.MethodDelete:
			n.mu.Lock()
			n.closeProxy()
			n.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/connection":
			n.mu.Lock()
			st := n.status
			n.mu.Unlock()
			js(map[string]any{"status": st})
		default:
			w.WriteHeader(404)
		}
	})
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

func socksDial(relay, target string, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", relay, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial relay: %w", err)
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	host, port, _ := net.SplitHostPort(target)
	p, _ := strconv.Atoi(port)
	ip := net.ParseIP(host).To4()
	req := []byte{5, 1, 0, 5, 1, 0, 1}
	req = append(req, ip...)
	req = append(req, byte(p>>8), byte(p))
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 2+10)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, fmt.Errorf("socks reply: %w", err)
	}
	if resp[3] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks refused code %d", resp[3])
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

func TestLoadThroughReconnects(t *testing.T) {
	rate := envIntDefault("LOAD_RATE", 10)
	dur := time.Duration(envIntDefault("LOAD_SECONDS", 60)) * time.Second
	hold := time.Duration(envIntDefault("LOAD_HOLD_MS", 3000)) * time.Millisecond

	logf, _ := os.Create(os.Getenv("LOAD_LOG"))
	if logf == nil {
		logf, _ = os.Create(os.DevNull)
	}
	log := slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	node := &simNode{
		t:           t,
		proxyAddr:   "127.0.0.1:" + strconv.Itoa(freePort(t)),
		connectIn:   time.Duration(envIntDefault("LOAD_CONNECT_MS", 2000)) * time.Millisecond,
		dropAfter:   time.Duration(envIntDefault("LOAD_DROP_S", 15)) * time.Second,
		acceptDelay: time.Duration(envIntDefault("LOAD_ACCEPT_DELAY_MS", 0)) * time.Millisecond,
		status:      "NotConnected",
	}
	api := httptest.NewServer(node.handler())
	defer api.Close()
	ipcheck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("1.2.3.4"))
	}))
	defer ipcheck.Close()
	echo := echoServer(t)

	_, pp, _ := net.SplitHostPort(node.proxyAddr)
	proxyPort, _ := strconv.Atoi(pp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wants := demand.New()
	egress := myst.New(myst.Options{
		Client:         tequilapi.New(api.URL, "u", "p", &http.Client{}),
		Identity:       "0xme",
		Exit:           config.ExitPolicy{},
		ProxyHost:      "127.0.0.1",
		ProxyPort:      proxyPort,
		ExitCheckURL:   ipcheck.URL,
		DialTimeout:    10 * time.Second,
		HealthInterval: time.Second,
		ReconnectMin:   500 * time.Millisecond,
		ReconnectMax:   4 * time.Second,
		Demand:         wants,
		OnDemand:       true,
		SessionIdle:    15 * time.Minute,
		ResetNode: func(ctx context.Context, reason string) error {
			node.mu.Lock()
			node.closeProxy()
			node.mu.Unlock()
			time.Sleep(300 * time.Millisecond)
			return nil
		},
		Logger: log,
	})
	g := gate.New(ctx, true)

	var fatal atomic.Value
	fail := func(what string, err error) {
		if err != nil && ctx.Err() == nil {
			fatal.Store(what + ": " + err.Error())
			cancel()
		}
	}
	go driveKillSwitch(ctx, egress, g, true, log)
	go func() { fail("tunnel", guard.Run(log, "tunnel", func() error { return egress.Run(ctx) })) }()

	socksAddr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	httpAddr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	base := frontend.Base{Dialer: egress, Gate: g, Log: log, DialTimeout: 10 * time.Second,
		Demand: wants, ReadyWait: time.Duration(envIntDefault("LOAD_READY_S", 60)) * time.Second,
		MaxConns: envIntDefault("LOAD_MAXCONNS", 1000)}
	sb := base
	sb.Listen = socksAddr
	hb := base
	hb.Listen = httpAddr
	go func() { fail("socks5", socks5.New(sb, "", "").Serve(ctx)) }()
	go func() { fail("http", httpproxy.New(hb, "", "").Serve(ctx)) }()
	time.Sleep(200 * time.Millisecond)

	var ok, refused, broken, other atomic.Int64
	errs := sync.Map{}
	var wg sync.WaitGroup
	tick := time.NewTicker(time.Second / time.Duration(rate))
	defer tick.Stop()
	end := time.After(dur)
	peak := 0
	go func() {
		for ctx.Err() == nil {
			if n := runtime.NumGoroutine(); n > peak {
				peak = n
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
loop:
	for {
		select {
		case <-end:
			break loop
		case <-ctx.Done():
			break loop
		case <-tick.C:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := socksDial(socksAddr, echo, 90*time.Second)
			if err != nil {
				msg := err.Error()
				if strings.Contains(msg, "code 2") {
					refused.Add(1)
				} else {
					other.Add(1)
				}
				v, _ := errs.LoadOrStore(msg, new(atomic.Int64))
				v.(*atomic.Int64).Add(1)
				return
			}
			defer c.Close()
			deadline := time.Now().Add(hold)
			buf := make([]byte, 5)
			for time.Now().Before(deadline) {
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := c.Write([]byte("hello")); err != nil {
					broken.Add(1)
					return
				}
				if _, err := io.ReadFull(c, buf); err != nil {
					broken.Add(1)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			ok.Add(1)
		}()
	}
	wg.Wait()

	t.Logf("ok=%d refused=%d broken-midstream=%d other=%d connects=%d drops=%d sessions=%d peak_goroutines=%d",
		ok.Load(), refused.Load(), broken.Load(), other.Load(), node.connects.Load(), node.drops.Load(), egress.Sessions(), peak)
	errs.Range(func(k, v any) bool { t.Logf("  %5d x %s", v.(*atomic.Int64).Load(), k); return true })
	if f := fatal.Load(); f != nil {
		t.Fatalf("relay stopped: %v", f)
	}
}

func envIntDefault(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}
