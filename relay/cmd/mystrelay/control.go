package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
)

// controlHeader carries the shared secret to the relay's control endpoints.
// It must match the name the health server checks.
const controlHeader = "X-Relay-Control"

// ensureControlToken returns the secret that lets this machine instruct the
// relay, writing one on the first start.
//
// It lives in a file rather than being passed around because the two sides are
// different processes started at different times by different things: the relay
// from a scheduled task, the tray icon and the CLI from whoever is signed in.
// The file is the only thing they share. A deployment that cannot write one -
// the container runs on a read-only filesystem - simply gets no secret, and a
// relay with no secret takes no instructions at all.
func ensureControlToken(cfg *config.Config) (string, error) {
	if cfg.ControlToken != "" {
		return cfg.ControlToken, nil
	}
	path := cfg.ControlTokenFile
	if path == "" {
		return "", fmt.Errorf("no control secret and nowhere to keep one")
	}
	if tok := readTokenFile(path); tok != "" {
		return tok, nil
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// 0600 is what keeps this out of other accounts' reach on Unix. On Windows
	// the mode is not enforced, and what protects it there is where it sits:
	// under the signed-in account's own LOCALAPPDATA.
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func readTokenFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// --- client ----------------------------------------------------------------

// relayClient talks to a running relay over its status port.
type relayClient struct {
	base      string // http://127.0.0.1:7348
	secret    string // given directly, when it was
	tokenFile string
	http      *http.Client
}

func newRelayClient(cfg *config.Config) *relayClient {
	return &relayClient{
		base:      healthBase(cfg.HealthListen),
		secret:    cfg.ControlToken,
		tokenFile: cfg.ControlTokenFile,
		http:      directClient(),
	}
}

// token reads the secret afresh each time rather than caching it. The tray
// outlives the relays it watches, and a relay writing its first secret minutes
// after the icon went up would otherwise never be reachable.
func (c *relayClient) token() string {
	if c.secret != "" {
		return c.secret
	}
	return readTokenFile(c.tokenFile)
}

// healthBase turns a listen address into a URL to ask.
//
// A relay told to listen on every address is still asked on loopback: the
// question is being put to the relay on this machine, and the wildcard says
// where it accepts, not where it lives.
func healthBase(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (c *relayClient) status(ctx context.Context) (*statusReport, error) {
	var rep statusReport
	if err := c.call(ctx, http.MethodGet, "/status", false, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

func (c *relayClient) balance(ctx context.Context) (*balanceReport, error) {
	var rep balanceReport
	if err := c.call(ctx, http.MethodGet, "/control/balance", true, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// rotate asks for a different exit and returns what the relay says it did.
func (c *relayClient) rotate(ctx context.Context, reason string) (changed bool, detail string, err error) {
	var out struct {
		Changing bool   `json:"changing"`
		Detail   string `json:"detail"`
	}
	path := "/control/rotate?reason=" + urlValue(reason)
	if err := c.call(ctx, http.MethodPost, path, true, &out); err != nil {
		return false, "", err
	}
	return out.Changing, out.Detail, nil
}

func (c *relayClient) stop(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/control/stop", true, nil)
}

// call performs one request, turning the relay's error document into an error.
func (c *relayClient) call(ctx context.Context, method, path string, auth bool, out any) error {
	secret := ""
	if auth {
		if secret = c.token(); secret == "" {
			return fmt.Errorf("no control secret found; this relay takes no instructions from here")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	if auth {
		req.Header.Set(controlHeader, secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, trunc(strings.TrimSpace(string(body)), 120))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(bytes.NewReader(body)).Decode(out)
}

// urlValue escapes a query value without pulling in net/url for one field.
func urlValue(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteByte(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

// balanceReport is the /control/balance document.
type balanceReport struct {
	Identity     string    `json:"identity"`
	Registration string    `json:"registration"`
	MYST         string    `json:"myst"`
	Wei          string    `json:"wei"`
	Channel      string    `json:"channel"`
	At           time.Time `json:"at"`
}

// --- commands --------------------------------------------------------------

func cmdRotate(args []string) error {
	fs := newFlagSet("rotate")
	cfgPath := fs.String("config", "", "path to config.env")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := newRelayClient(cfg)
	changed, detail, err := client.rotate(ctx, "asked from the command line")
	if err != nil {
		return notRunning(err)
	}
	fmt.Println(detail)
	if !changed {
		return nil
	}
	// Wait for the new exit rather than reporting a request as a result: what
	// the operator wants to know is which exit they ended up on.
	if rep := awaitExit(ctx, client); rep != nil {
		fmt.Printf("  exit %s  %s %s  via %s\n",
			orDash(rep.Tunnel.ExitIP), orDash(rep.Tunnel.Country), orDash(rep.Tunnel.IPType), short(rep.Tunnel.ProviderID))
	}
	return nil
}

// awaitExit polls until a session is up again, and gives up quietly: an
// on-demand relay with nothing using it will not build one just to be looked at.
func awaitExit(ctx context.Context, client *relayClient) *statusReport {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
		rep, err := client.status(ctx)
		if err != nil {
			return nil
		}
		if rep.Healthy {
			return rep
		}
		if rep.Tunnel.State == "idle" {
			return nil
		}
	}
	return nil
}

func cmdBalance(args []string) error {
	fs := newFlagSet("balance")
	cfgPath := fs.String("config", "", "path to config.env")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b, err := newRelayClient(cfg).balance(ctx)
	if err != nil {
		return notRunning(err)
	}
	fmt.Printf("\n  balance      %s MYST\n", b.MYST)
	fmt.Printf("  identity     %s\n", b.Identity)
	fmt.Printf("  registration %s\n", b.Registration)
	if b.Channel != "" {
		fmt.Printf("  channel      %s\n", b.Channel)
	}
	fmt.Println()
	return nil
}

func cmdStop(args []string) error {
	fs := newFlagSet("stop")
	cfgPath := fs.String("config", "", "path to config.env")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := newRelayClient(cfg)
	if err := client.stop(ctx); err != nil {
		return notRunning(err)
	}
	// Report it stopped only once it has: the ports going quiet is the proof,
	// and the ports are what anything else on this machine is waiting for.
	for range 60 {
		time.Sleep(250 * time.Millisecond)
		if _, err := client.status(ctx); err != nil {
			fmt.Println("relay stopped")
			return nil
		}
	}
	fmt.Println("stop requested, but the relay is still answering")
	return nil
}

// humanBytes renders a byte count the way a person reads one. Powers of 1024,
// because that is what the exit providers bill in.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n)/unit, 0
	for value >= unit && exp < 3 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", value, [...]string{"KiB", "MiB", "GiB", "TiB"}[exp])
}

// notRunning turns a connection failure into the sentence that helps.
func notRunning(err error) error {
	if isRefused(err) {
		return fmt.Errorf("the relay does not appear to be running. Start it with \"%s run\"", exeName())
	}
	return err
}

// isRefused reports whether the failure was reaching the relay at all, rather
// than something the relay said. Any transport-level error against a loopback
// port means nothing is listening on it.
func isRefused(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
}
