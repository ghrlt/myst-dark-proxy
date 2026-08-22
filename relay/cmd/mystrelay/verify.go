package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/socks"
)

func cmdStatus(args []string) error {
	fs := newFlagSet("status")
	cfgPath := fs.String("config", "", "path to config.env")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	rep, err := fetchStatus(cfg.HealthListen)
	if err != nil {
		return fmt.Errorf("%w\n\nThe relay does not appear to be running. Start it with \"%s run\"", err, exeName())
	}

	t := rep.Tunnel
	k := rep.KillSwitch
	fmt.Printf("myst-relay  %s\n\n", tick(rep.Healthy))
	fmt.Printf("  tunnel       %s\n", t.State)
	if t.ProviderID != "" {
		fmt.Printf("  exit         %s  %s %s\n", orDash(t.ExitIP), orDash(t.Country), orDash(t.IPType))
		fmt.Printf("  provider     %s\n", t.ProviderID)
	}
	if t.Detail != "" {
		fmt.Printf("  detail       %s\n", t.Detail)
	}
	fmt.Printf("  kill switch  open=%v strict=%v  admitted=%d refused=%d active=%d\n",
		k.Open, k.Strict, k.Admitted, k.Refused, k.Active)
	if k.Reason != "" {
		fmt.Printf("               %s\n", k.Reason)
	}
	fmt.Printf("  uptime       %s\n\n", rep.Uptime)
	for _, e := range rep.Endpoints {
		fmt.Printf("  %-10s %s\n", e.Name, e.Addr)
	}
	return nil
}

// cmdVerify proves the relay is doing what it claims: traffic leaves through
// the exit, and it leaves from an address that is not this machine's.
func cmdVerify(args []string) error {
	fs := newFlagSet("verify")
	cfgPath := fs.String("config", "", "path to config.env")
	checkURL := fs.String("url", "https://api.ipify.org", "service that echoes the caller's address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	fails := 0
	bad := func(format string, a ...any) {
		fails++
		fmt.Printf("  \u2717 "+format+"\n", a...)
	}
	good := func(format string, a ...any) { fmt.Printf("  \u2713 "+format+"\n", a...) }

	fmt.Println("\nBaseline")
	direct, err := fetchIP(directClient(), *checkURL)
	if err != nil {
		bad("could not learn this machine's own address: %v", err)
	} else {
		good("this machine's public address is %s", direct)
	}

	fmt.Println("\nRelay")
	rep, err := fetchStatus(cfg.HealthListen)
	if err != nil {
		bad("the relay is not answering on %s: %v", cfg.HealthListen, err)
		fmt.Printf("\nStart it with \"%s run\" and try again.\n\n", exeName())
		return fmt.Errorf("relay not running")
	}
	if rep.Healthy {
		good("healthy, exit %s (%s, %s) via %s", orDash(rep.Tunnel.ExitIP), orDash(rep.Tunnel.Country),
			orDash(rep.Tunnel.IPType), short(rep.Tunnel.ProviderID))
	} else {
		bad("not healthy: tunnel=%s %s", rep.Tunnel.State, rep.KillSwitch.Reason)
	}

	check := func(label string, client *http.Client, addr string) {
		fmt.Printf("\n%s on %s\n", label, addr)
		got, err := fetchIP(client, *checkURL)
		switch {
		case err != nil:
			bad("no usable response: %v", err)
		case got == direct && direct != "":
			bad("egressed from this machine's own address (%s) - that is a leak", got)
		default:
			good("egressed from %s", got)
		}
	}

	if cfg.SocksListen != "" {
		check("SOCKS5", socksClient(cfg.SocksListen, cfg.Auth.Username, cfg.Auth.Password), cfg.SocksListen)
	}
	if cfg.HTTPListen != "" {
		check("HTTP proxy", httpProxyClient(cfg.HTTPListen, cfg.Auth.Username, cfg.Auth.Password), cfg.HTTPListen)
	}

	fmt.Println("\nLeak notes")
	fmt.Println("  - SOCKS5 here resolves names at the exit node, so DNS does not leak")
	fmt.Println("    through the relay. A client that resolves before connecting still can:")
	fmt.Println("    use socks5h:// in curl, and network.proxy.socks_remote_dns=true in Firefox.")
	fmt.Println("  - UDP is not carried. WebRTC and QUIC can leave a browser over UDP and")
	fmt.Println("    bypass this relay entirely - see docs/WINDOWS.md for the settings that")
	fmt.Println("    turn them off.")

	fmt.Println()
	if fails == 0 {
		fmt.Print("  all checks passed\n\n")
		return nil
	}
	fmt.Printf("  %d check(s) failed\n\n", fails)
	return fmt.Errorf("%d verification check(s) failed", fails)
}

// --- status document -------------------------------------------------------

type statusReport struct {
	Uptime string `json:"uptime"`
	Tunnel struct {
		State      string `json:"state"`
		ProviderID string `json:"provider_id"`
		Country    string `json:"country"`
		IPType     string `json:"ip_type"`
		ExitIP     string `json:"exit_ip"`
		Detail     string `json:"detail"`
	} `json:"tunnel"`
	KillSwitch struct {
		Open     bool   `json:"open"`
		Strict   bool   `json:"strict"`
		Reason   string `json:"reason"`
		Active   int    `json:"active_streams"`
		Admitted uint64 `json:"admitted_total"`
		Refused  uint64 `json:"refused_total"`
	} `json:"kill_switch"`
	Endpoints []struct {
		Name string `json:"name"`
		Addr string `json:"addr"`
	} `json:"endpoints"`
	Healthy bool `json:"healthy"`
}

func fetchStatus(listen string) (*statusReport, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	if host == "0.0.0.0" || host == "" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+net.JoinHostPort(host, port)+"/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := directClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rep statusReport
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// --- clients ---------------------------------------------------------------

func directClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}
}

func socksClient(addr, user, pass string) *http.Client {
	host, port, _ := net.SplitHostPort(addr)
	if host == "0.0.0.0" || host == "" || host == "::" {
		host = "127.0.0.1"
	}
	c := &socks.Client{Address: net.JoinHostPort(host, port), Username: user, Password: pass}
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext:       c.DialContext,
			Proxy:             nil,
			DisableKeepAlives: true,
		},
	}
}

func httpProxyClient(addr, user, pass string) *http.Client {
	host, port, _ := net.SplitHostPort(addr)
	if host == "0.0.0.0" || host == "" || host == "::" {
		host = "127.0.0.1"
	}
	u := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(u),
			DisableKeepAlives: true,
		},
	}
}

func fetchIP(client *http.Client, checkURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	got := strings.TrimSpace(string(body))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, trunc(got, 80))
	}
	if net.ParseIP(got) == nil {
		return "", fmt.Errorf("expected an address, got %q", trunc(got, 80))
	}
	return got, nil
}

// --- small helpers ---------------------------------------------------------

func tick(ok bool) string {
	if ok {
		return "\u2713 carrying traffic"
	}
	return "\u2717 not carrying traffic"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

var _ = os.Stdout
var _ = config.AppDir
