package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/gate"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tunnel"
)

const secret = "0123456789abcdef"

// fakeTunnel is a tunnel that answers every optional question, so the control
// surface can be exercised without a Mysterium node behind it.
type fakeTunnel struct {
	status   tunnel.Status
	rotated  chan string
	rotates  bool
	balance  tunnel.Balance
	balError error
}

func (f *fakeTunnel) Name() string              { return "fake" }
func (f *fakeTunnel) Run(context.Context) error { return nil }
func (f *fakeTunnel) Status() tunnel.Status     { return f.status }
func (f *fakeTunnel) Watch() (<-chan tunnel.Status, func()) {
	return make(chan tunnel.Status), func() {}
}
func (f *fakeTunnel) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, tunnel.ErrTunnelDown
}
func (f *fakeTunnel) Sessions() uint64 { return 3 }
func (f *fakeTunnel) Usage() tunnel.Usage {
	return tunnel.Usage{
		Session:  tunnel.Meter{BytesSent: 10, BytesReceived: 20, Wei: "5", MYST: "0.0000"},
		Total:    tunnel.Meter{BytesSent: 100, BytesReceived: 200, Wei: "50", MYST: "1.2500"},
		Sessions: 3,
	}
}
func (f *fakeTunnel) Rotate(reason string) bool {
	select {
	case f.rotated <- reason:
	default:
	}
	return f.rotates
}
func (f *fakeTunnel) Balance(context.Context) (tunnel.Balance, error) {
	return f.balance, f.balError
}

func newServer(t *testing.T, token string, stop func()) (*httptest.Server, *fakeTunnel) {
	t.Helper()
	tun := &fakeTunnel{
		status:  tunnel.Status{State: tunnel.StateUp, ExitIP: "203.0.113.7", Country: "NL"},
		rotated: make(chan string, 1),
		rotates: true,
		balance: tunnel.Balance{Identity: "0xabc", Registration: "Registered", MYST: "1.2500"},
	}
	g := gate.New(context.Background(), true)
	t.Cleanup(g.Shutdown)
	g.Open()

	srv := httptest.NewServer((&Server{
		Version: "test",
		Tunnel:  tun,
		Gate:    g,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Started: time.Now(),
		Token:   token,
		Stop:    stop,
	}).handler())
	t.Cleanup(srv.Close)
	return srv, tun
}

func do(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestStatusCarriesUsageAndControlFlag(t *testing.T) {
	srv, _ := newServer(t, secret, func() {})

	resp := do(t, srv, http.MethodGet, "/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Control {
		t.Error("a relay with a secret should say it takes instructions")
	}
	if rep.Usage == nil || rep.Usage.Total.MYST != "1.2500" {
		t.Errorf("usage: got %+v, want the tunnel's own figures", rep.Usage)
	}
	if rep.Version != "test" {
		t.Errorf("version: got %q, want %q", rep.Version, "test")
	}
}

// A relay with nowhere to keep a secret must not be steerable at all, and must
// say so in its status rather than offering controls that refuse.
func TestNoSecretMeansNoControls(t *testing.T) {
	srv, tun := newServer(t, "", func() { t.Error("stop must not be reachable without a secret") })

	for _, path := range []string{"/control/rotate", "/control/stop"} {
		if resp := do(t, srv, http.MethodPost, path, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s without a secret: got %d, want 404", path, resp.StatusCode)
		}
	}
	if resp := do(t, srv, http.MethodGet, "/control/balance", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("balance without a secret: got %d, want 404", resp.StatusCode)
	}
	select {
	case reason := <-tun.rotated:
		t.Fatalf("the tunnel was rotated without a secret, for %q", reason)
	default:
	}

	resp := do(t, srv, http.MethodGet, "/status", nil)
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if rep.Control {
		t.Error("a relay with no secret should not claim to take instructions")
	}
}

func TestControlRefusesTheWrongCallers(t *testing.T) {
	srv, tun := newServer(t, secret, func() { t.Error("stop must not be reachable") })

	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"no secret", http.MethodPost, nil, http.StatusForbidden},
		{"wrong secret", http.MethodPost, map[string]string{controlHeader: "nope"}, http.StatusForbidden},
		{"from a browser", http.MethodPost, map[string]string{
			controlHeader: secret, "Origin": "https://example.com",
		}, http.StatusForbidden},
		{"wrong method", http.MethodGet, map[string]string{controlHeader: secret}, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, srv, tc.method, "/control/rotate", tc.headers)
			if resp.StatusCode != tc.want {
				t.Errorf("got %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	select {
	case reason := <-tun.rotated:
		t.Fatalf("a refused request still rotated the tunnel, for %q", reason)
	default:
	}
}

func TestRotateReachesTheTunnel(t *testing.T) {
	srv, tun := newServer(t, secret, func() {})

	resp := do(t, srv, http.MethodPost, "/control/rotate?reason=from+a+test",
		map[string]string{controlHeader: secret})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got %d, want 202", resp.StatusCode)
	}
	var out struct {
		Changing bool   `json:"changing"`
		Detail   string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Changing {
		t.Error("a tunnel with a session to leave should report that it is changing")
	}
	select {
	case reason := <-tun.rotated:
		if reason != "from a test" {
			t.Errorf("reason: got %q, want %q", reason, "from a test")
		}
	default:
		t.Fatal("the tunnel was never asked to rotate")
	}
}

// Nothing to rotate is an answer, not a failure: an on-demand relay resting
// between clients has no exit to leave.
func TestRotateWithNoSessionIsAccepted(t *testing.T) {
	srv, tun := newServer(t, secret, func() {})
	tun.rotates = false

	resp := do(t, srv, http.MethodPost, "/control/rotate", map[string]string{controlHeader: secret})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got %d, want 202", resp.StatusCode)
	}
	var out struct {
		Changing bool `json:"changing"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Changing {
		t.Error("no session to leave should not be reported as a change")
	}
}

func TestBalanceIsUnavailableRatherThanZeroWhenTheNodeCannotAnswer(t *testing.T) {
	srv, tun := newServer(t, secret, func() {})
	tun.balError = errors.New("node not running")

	resp := do(t, srv, http.MethodGet, "/control/balance", map[string]string{controlHeader: secret})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", resp.StatusCode)
	}
}

func TestStopAnswersBeforeStopping(t *testing.T) {
	stopped := make(chan struct{})
	srv, _ := newServer(t, secret, func() { close(stopped) })

	resp := do(t, srv, http.MethodPost, "/control/stop", map[string]string{controlHeader: secret})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("got %d, want 202", resp.StatusCode)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay was never told to stop")
	}
}
