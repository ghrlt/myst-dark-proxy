// Package tequilapi is a minimal client for the myst node's local HTTP API.
//
// Only the handful of endpoints the relay needs are modelled. Endpoints whose
// verb changed across node releases are probed (PUT then POST) so the service
// keeps working across myst upgrades instead of hard-failing on a 405.
package tequilapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxResponse guards against a runaway body. The proposals list is the only
// large response and sits in the low megabytes.
const maxResponse = 256 << 20

// Client talks to a single myst node.
type Client struct {
	baseURL string
	user    string
	pass    string
	http    *http.Client

	mu    sync.Mutex
	token string

	// lifecycle serialises Connect and Disconnect. The node runs one session
	// lifecycle at a time, and a Disconnect landing while a Connect is in
	// flight cancels it - the node reports that as err_connection_cancelled -
	// so the two must never overlap. A channel rather than a mutex, so that
	// waiting for a turn can be abandoned with the caller's context.
	lifecycle chan struct{}
}

// requestTimeout bounds an ordinary API call. Connect is exempt: building a
// session means the node negotiating with a provider, which can legitimately
// take longer, and abandoning the request does not stop the node - it leaves
// the half-built session behind. The caller's context is the bound there.
const requestTimeout = 60 * time.Second

// turn waits for the session lifecycle to be free, and returns what releases it.
func (c *Client) turn(ctx context.Context) (func(), error) {
	select {
	case c.lifecycle <- struct{}{}:
		return func() { <-c.lifecycle }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// New returns a client for baseURL. Node 1.39 serves the API at the root
// (http://myst:4050), not under a /tequilapi prefix.
func New(baseURL, user, pass string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{
		lifecycle: make(chan struct{}, 1),
		baseURL:   strings.TrimRight(baseURL, "/"),
		user:      user,
		pass:      pass,
		http:      hc,
	}
}

// APIError carries a non-2xx response.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	return fmt.Sprintf("tequilapi %s: HTTP %d: %s", e.Path, e.Status, body)
}

// NotFound reports whether the endpoint is absent on this node version.
func (e *APIError) NotFound() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusMethodNotAllowed
}

// Healthcheck reports node liveness. It needs no authentication, so it is the
// probe used while waiting for the node to finish booting.
type Healthcheck struct {
	Uptime  string `json:"uptime"`
	Process int    `json:"process"`
	Version string `json:"version"`
}

// Health returns the node healthcheck.
func (c *Client) Health(ctx context.Context) (*Healthcheck, error) {
	var out Healthcheck
	err := c.do(ctx, http.MethodGet, "/healthcheck", nil, &out, false)
	return &out, err
}

// WaitHealthy blocks until the node answers its healthcheck or ctx expires.
func (c *Client) WaitHealthy(ctx context.Context, every time.Duration) (*Healthcheck, error) {
	t := time.NewTicker(every)
	defer t.Stop()
	var last error
	for {
		hc, err := c.Health(ctx)
		if err == nil {
			return hc, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for myst node: %w (last error: %v)", ctx.Err(), last)
		case <-t.C:
		}
	}
}

// Identity is the node's view of a keystore entry.
type Identity struct {
	ID                 string `json:"id"`
	RegistrationStatus string `json:"registration_status"`
	ChannelAddress     string `json:"channel_address"`
	// balance is a big.Int rendered as a bare JSON number; it routinely
	// exceeds what uint64 can hold, so it is kept unparsed.
	Balance       json.Number `json:"balance"`
	BalanceTokens struct {
		Wei   string `json:"wei"`
		Ether string `json:"ether"`
		Human string `json:"human"`
	} `json:"balance_tokens"`
	HermesID string `json:"hermes_id"`
}

// Registered reports whether the identity may open sessions.
func (i Identity) Registered() bool {
	return strings.EqualFold(i.RegistrationStatus, "Registered")
}

// Identities lists keystore entries known to the node.
func (c *Client) Identities(ctx context.Context) ([]Identity, error) {
	var out struct {
		Identities []Identity `json:"identities"`
	}
	if err := c.do(ctx, http.MethodGet, "/identities", nil, &out, true); err != nil {
		return nil, err
	}
	return out.Identities, nil
}

// Identity fetches a single identity's status.
func (c *Client) Identity(ctx context.Context, id string) (*Identity, error) {
	var out Identity
	err := c.do(ctx, http.MethodGet, "/identities/"+id, nil, &out, true)
	return &out, err
}

// BalanceTokens is a token amount as the node renders it.
type BalanceTokens struct {
	Wei   string `json:"wei"`
	Ether string `json:"ether"`
	Human string `json:"human"`
}

// RefreshBalance makes the node re-read the identity's balance and returns it.
// The node caches the consumer balance and, since 1.39.9, leaves it at 0 until
// something asks for a refresh, so GET /identities/{id} alone cannot be
// trusted to say what is there.
func (c *Client) RefreshBalance(ctx context.Context, id string) (BalanceTokens, error) {
	var out struct {
		Tokens BalanceTokens `json:"balance_tokens"`
	}
	err := c.do(ctx, http.MethodPut, "/identities/"+id+"/balance/refresh", nil, &out, true)
	return out.Tokens, err
}

// ImportIdentity uploads a keystore document to the node. Nodes that predate
// the endpoint return an APIError for which NotFound reports true; callers fall
// back to a pre-seeded keystore directory in that case.
func (c *Client) ImportIdentity(ctx context.Context, keystore []byte, passphrase string, setDefault bool) (*Identity, error) {
	body := map[string]any{
		"data":               base64.StdEncoding.EncodeToString(keystore),
		"current_passphrase": passphrase,
		"new_passphrase":     passphrase,
		"set_default":        setDefault,
	}
	var out Identity
	err := c.do(ctx, http.MethodPost, "/identities-import", body, &out, true)
	return &out, err
}

// Unlock decrypts an identity so it can sign session promises.
func (c *Client) Unlock(ctx context.Context, id, passphrase string) error {
	body := map[string]string{"passphrase": passphrase}
	return c.do(ctx, http.MethodPut, "/identities/"+id+"/unlock", body, nil, true)
}

// Location describes where a provider egresses.
type Location struct {
	Continent string `json:"continent"`
	Country   string `json:"country"`
	Region    string `json:"region"`
	City      string `json:"city"`
	ASN       int    `json:"asn"`
	ISP       string `json:"isp"`
	IPType    string `json:"ip_type"`
}

// Quality is the network's measured opinion of a provider.
type Quality struct {
	Quality    float64 `json:"quality"`
	Latency    float64 `json:"latency"`
	Bandwidth  float64 `json:"bandwidth"`
	Uptime     float64 `json:"uptime"`
	PacketLoss float64 `json:"packetLoss"`
}

// Proposal is one offer from one provider.
type Proposal struct {
	ProviderID  string   `json:"provider_id"`
	ServiceType string   `json:"service_type"`
	Location    Location `json:"location"`
	Quality     Quality  `json:"quality"`
}

// ProposalFilter narrows the proposal query server-side.
type ProposalFilter struct {
	ServiceType string
	Country     string
	IPType      string
	QualityMin  float64
}

// Proposals lists offers currently visible to the node.
func (c *Client) Proposals(ctx context.Context, f ProposalFilter) ([]Proposal, error) {
	q := make([]string, 0, 4)
	if f.ServiceType != "" {
		q = append(q, "service_type="+f.ServiceType)
	}
	if f.Country != "" {
		q = append(q, "country="+f.Country)
	}
	if f.IPType != "" {
		q = append(q, "ip_type="+f.IPType)
	}
	if f.QualityMin > 0 {
		q = append(q, fmt.Sprintf("quality_min=%g", f.QualityMin))
	}
	path := "/proposals"
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	var out struct {
		Proposals []Proposal `json:"proposals"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out, true); err != nil {
		return nil, err
	}
	return out.Proposals, nil
}

// ConnectOptions are the per-session knobs the relay cares about. ProxyPort is
// the reason this service can promise a fixed port: it pins the SOCKS5 listener
// the node opens for the session instead of letting it float.
type ConnectOptions struct {
	DNS       string `json:"dns,omitempty"`
	ProxyPort int    `json:"proxy_port,omitempty"`
}

// ConnectRequest starts a session.
type ConnectRequest struct {
	ConsumerID     string         `json:"consumer_id"`
	ProviderID     string         `json:"provider_id"`
	ServiceType    string         `json:"service_type"`
	ConnectOptions ConnectOptions `json:"connect_options"`
}

// ConnectionStatus is the node's session state.
type ConnectionStatus struct {
	Status     string    `json:"status"`
	SessionID  string    `json:"session_id"`
	ConsumerID string    `json:"consumer_id"`
	Proposal   *Proposal `json:"proposal,omitempty"`
}

// Connected reports whether traffic can flow.
func (s ConnectionStatus) Connected() bool { return strings.EqualFold(s.Status, "Connected") }

// Connection returns the state of the session bound to the given connection id.
//
// In proxy mode the node keys each session by the proxy port it was asked to
// bind, and these endpoints take it as ?id=. Omitting it silently reports on
// connection 0 - which is always "NotConnected" here, and would make a
// supervisor tear down a perfectly healthy tunnel on its first health tick.
func (c *Client) Connection(ctx context.Context, id int) (*ConnectionStatus, error) {
	var out ConnectionStatus
	err := c.do(ctx, http.MethodGet, connPath("/connection", id), nil, &out, true)
	return &out, err
}

// Connect opens a session.
func (c *Client) Connect(ctx context.Context, req ConnectRequest) (*ConnectionStatus, error) {
	release, err := c.turn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	var out ConnectionStatus
	err = c.do(ctx, http.MethodPut, "/connection", req, &out, true)
	return &out, err
}

// Disconnect tears down the session with the given connection id.
func (c *Client) Disconnect(ctx context.Context, id int) error {
	release, err := c.turn(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = c.do(ctx, http.MethodDelete, connPath("/connection", id), nil, nil, true)
	var apiErr *APIError
	if err != nil && errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusUnprocessableEntity) {
		return nil // nothing was connected
	}
	return err
}

func connPath(path string, id int) string {
	if id <= 0 {
		return path
	}
	return path + "?id=" + strconv.Itoa(id)
}

// Statistics reports session counters.
type Statistics struct {
	Duration      int64  `json:"duration"`
	BytesSent     uint64 `json:"bytes_sent"`
	BytesReceived uint64 `json:"bytes_received"`
	// tokens_spent is a big.Int rendered as a bare JSON number. A session that
	// runs long enough overflows uint64, and a decode that fails takes the
	// byte counters down with it, so it is kept unparsed. spent_tokens carries
	// the same figure as strings and is what Wei prefers.
	TokensSpent json.Number `json:"tokens_spent"`
	SpentTokens struct {
		Wei   string `json:"wei"`
		Ether string `json:"ether"`
		Human string `json:"human"`
	} `json:"spent_tokens"`
}

// Wei is what the session has cost so far, in the smallest unit of MYST.
func (s Statistics) Wei() *big.Int {
	for _, raw := range []string{strings.TrimSpace(s.SpentTokens.Wei), strings.TrimSpace(s.TokensSpent.String())} {
		if raw == "" {
			continue
		}
		if n, ok := new(big.Int).SetString(raw, 10); ok {
			return n
		}
	}
	return new(big.Int)
}

// Statistics returns the counters for one session.
//
// The session id is the session's UUID, not the connection id every other
// /connection endpoint takes: this one alone looks the session up by the id the
// node reported when it opened it, and answers with a zeroed document for
// anything else - which is what a proxy port passed here quietly produces. An
// empty id asks for whichever session the node holds, which for this relay is
// the only one there is.
func (c *Client) Statistics(ctx context.Context, sessionID string) (*Statistics, error) {
	path := "/connection/statistics"
	if sessionID != "" {
		path += "?id=" + url.QueryEscape(sessionID)
	}
	var out Statistics
	err := c.do(ctx, http.MethodGet, path, nil, &out, true)
	return &out, err
}

// authenticate obtains a bearer token, tolerating both endpoint spellings.
func (c *Client) authenticate(ctx context.Context) (string, error) {
	body := map[string]string{"username": c.user, "password": c.pass}
	for _, path := range []string{"/auth/authenticate", "/auth/login"} {
		var out struct {
			Token string `json:"token"`
		}
		err := c.do(ctx, http.MethodPost, path, body, &out, false)
		if err == nil {
			if out.Token == "" {
				continue // cookie-only login is not usable for API calls
			}
			return out.Token, nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("tequilapi: no usable authentication endpoint")
}

func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	tok, err := c.authenticate(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.token = tok
	c.mu.Unlock()
	return tok, nil
}

func (c *Client) invalidate() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *Client) do(ctx context.Context, method, path string, in, out any, auth bool) error {
	err := c.attempt(ctx, method, path, in, out, auth)
	var apiErr *APIError
	if auth && err != nil && errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		// Token expired or the node restarted: re-authenticate once.
		c.invalidate()
		err = c.attempt(ctx, method, path, in, out, auth)
	}
	return err
}

func (c *Client) attempt(ctx context.Context, method, path string, in, out any, auth bool) error {
	if !(method == http.MethodPut && path == "/connection") {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The node validates the Host header against --tequilapi.allowed-hostnames;
	// "localhost" is always accepted, whatever container name we dialled.
	req.Host = "localhost"
	if auth {
		tok, err := c.bearer(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tequilapi %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &APIError{Status: resp.StatusCode, Path: path, Body: string(raw)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Streamed rather than buffered: /proposals routinely runs to several
	// megabytes, and a body cap small enough to feel safe truncates it into
	// invalid JSON. maxResponse is a runaway guard, not a working limit.
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponse))
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty body, nothing to fill in
		}
		return fmt.Errorf("tequilapi %s: decode response: %w", path, err)
	}
	return nil
}
