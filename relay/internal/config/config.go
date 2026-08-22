// Package config loads and validates the relay configuration from the
// environment. Every setting has a RELAY_/MYST_ prefixed variable and a sane
// default, so the service is runnable with only an identity file + passphrase.
package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// fileValues holds settings read from a config file. The environment always
// wins, so a service definition or a shell can override the file without
// editing it.
var fileValues = map[string]string{}

// LoadFile reads a KEY=VALUE file (the same shape as .env) into the fallback
// layer consulted by env(). A missing file is not an error: the defaults and
// the environment are enough to run.
func LoadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		k, v, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE, got %q", path, line, text)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		fileValues[k] = v
	}
	return sc.Err()
}

// AppDir is where a native install keeps its config, node binary and data.
// Under Docker everything is passed in explicitly and this is unused.
func AppDir() string {
	if v := os.Getenv("MYST_RELAY_HOME"); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "windows":
		// LOCALAPPDATA for a per-user install; ProgramData when running as a
		// machine-wide service, where the user profile is not the right home.
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "myst-relay")
		}
		if base := os.Getenv("ProgramData"); base != "" {
			return filepath.Join(base, "myst-relay")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "myst-relay")
		}
	}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "myst-relay")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "myst-relay")
	}
	return "myst-relay"
}

// ConfigPath is the default config file location for a native install.
func ConfigPath() string { return filepath.Join(AppDir(), "config.env") }

// Forward is a fixed local port that is spliced to a fixed remote endpoint
// through the tunnel. This is the "raw" frontend: it carries any byte stream,
// for clients that cannot speak a proxy protocol at all.
type Forward struct {
	Listen string // host:port the relay listens on
	Target string // host:port reached from the exit node
}

// Auth is optional proxy credentials, shared by the SOCKS5 and HTTP frontends.
type Auth struct {
	Username string
	Password string
}

// Enabled reports whether credentials were configured.
func (a Auth) Enabled() bool { return a.Username != "" }

// ExitPolicy constrains which Mysterium provider nodes may be used as exits.
// An empty field means "no constraint".
type ExitPolicy struct {
	ProviderID  string   // pin one specific provider, bypasses all other filters
	Countries   []string // ISO-3166 alpha-2, any-of
	IPTypes     []string // residential, cellular, hosting, business, ...
	ServiceType string   // wireguard
	MinQuality  float64
	MaxLatency  time.Duration
	Exclude     []string // provider IDs never to use
}

// Config is the fully resolved runtime configuration.
type Config struct {
	// Identity
	IdentityFile       string
	IdentityPassphrase string
	IdentityAddress    string // derived from the keystore file

	// Upstream myst node
	TequilaBaseURL string
	TequilaUser    string
	TequilaPass    string
	ProxyHost      string // where myst publishes its proxy listener
	ProxyPort      int    // fixed port we ask myst to bind, see connect options

	// Managed node. When Managed is set the relay downloads, starts and
	// supervises the node itself instead of attaching to one compose started.
	// This is how a desktop install works, where there is no container to run
	// the node in.
	Managed     bool
	NodeVersion string
	NodeDir     string // where the node binary lives
	NodeDataDir string // node config, keystore and database
	TequilaHost string
	TequilaPort int

	// Frontends. An empty listen address disables that frontend.
	SocksListen  string
	HTTPListen   string
	Forwards     []Forward
	HealthListen string

	// Behaviour
	Auth             Auth
	Exit             ExitPolicy
	StrictKillSwitch bool // tear down established streams when the tunnel drops
	DialTimeout      time.Duration
	IdleTimeout      time.Duration
	ReconnectMin     time.Duration
	ReconnectMax     time.Duration
	HealthInterval   time.Duration
	RotateEvery      time.Duration // 0 disables periodic exit rotation
	DNSMode          string        // provider | system | <ip>
	ExitCheckURL     string        // fetched through the tunnel to learn the exit address; "" disables
	LogLevel         string
	LogFile          string // empty logs to stdout
}

// Load reads the environment and returns a validated configuration.
func Load() (*Config, error) {
	c := &Config{
		IdentityFile:   env("MYST_IDENTITY_FILE", "/run/secrets/identity.json"),
		TequilaBaseURL: env("MYST_TEQUILAPI_URL", "http://myst:4050"),
		TequilaUser:    env("MYST_TEQUILAPI_USER", "myst"),
		TequilaPass:    env("MYST_TEQUILAPI_PASS", "mystberry"),
		ProxyHost:      env("MYST_PROXY_HOST", ""),
		ProxyPort:      envInt("MYST_PROXY_PORT", 40000),

		Managed:     envBool("MYST_MANAGED", runtime.GOOS == "windows"),
		NodeVersion: env("MYST_NODE_VERSION", "1.39.5"),
		TequilaHost: env("MYST_TEQUILAPI_HOST", "127.0.0.1"),
		TequilaPort: envInt("MYST_TEQUILAPI_PORT", 4050),

		SocksListen:  env("RELAY_SOCKS_LISTEN", "127.0.0.1:7362"),
		HTTPListen:   env("RELAY_HTTP_LISTEN", "127.0.0.1:7360"),
		HealthListen: env("RELAY_HEALTH_LISTEN", "127.0.0.1:9090"),

		StrictKillSwitch: envBool("RELAY_KILLSWITCH_STRICT", true),
		DialTimeout:      envDuration("RELAY_DIAL_TIMEOUT", 30*time.Second),
		IdleTimeout:      envDuration("RELAY_IDLE_TIMEOUT", 0),
		ReconnectMin:     envDuration("RELAY_RECONNECT_MIN", 2*time.Second),
		ReconnectMax:     envDuration("RELAY_RECONNECT_MAX", 60*time.Second),
		HealthInterval:   envDuration("RELAY_HEALTH_INTERVAL", 5*time.Second),
		RotateEvery:      envDuration("RELAY_ROTATE_EVERY", 0),
		DNSMode:          env("RELAY_DNS_MODE", "provider"),
		ExitCheckURL:     env("RELAY_EXIT_CHECK_URL", "https://api.ipify.org"),
		LogLevel:         env("RELAY_LOG_LEVEL", "info"),
		LogFile:          env("RELAY_LOG_FILE", ""),

		Auth: Auth{
			Username: env("RELAY_PROXY_USER", ""),
			Password: env("RELAY_PROXY_PASS", ""),
		},
		Exit: ExitPolicy{
			ProviderID:  env("EXIT_PROVIDER_ID", ""),
			Countries:   envList("EXIT_COUNTRY", nil),
			IPTypes:     envList("EXIT_IP_TYPE", nil),
			ServiceType: env("EXIT_SERVICE_TYPE", "wireguard"),
			MinQuality:  envFloat("EXIT_MIN_QUALITY", 1.0),
			MaxLatency:  envDuration("EXIT_MAX_LATENCY", 0),
			Exclude:     envList("EXIT_EXCLUDE_PROVIDERS", nil),
		},
	}

	pass, err := secret("MYST_IDENTITY_PASSPHRASE")
	if err != nil {
		return nil, err
	}
	c.IdentityPassphrase = pass

	if c.Auth.Password == "" {
		if p, err := secret("RELAY_PROXY_PASS"); err == nil {
			c.Auth.Password = p
		}
	}

	fwd, err := parseForwards(env("RELAY_RAW_LISTEN", ""), env("RELAY_RAW_TARGET", ""), env("RELAY_RAW_FORWARDS", ""))
	if err != nil {
		return nil, err
	}
	c.Forwards = fwd

	c.resolveNode()

	if err := c.resolveIdentity(); err != nil {
		return nil, err
	}
	return c, c.validate()
}

// resolveNode fills in the managed-node paths and points the API client at
// whichever node this install talks to.
func (c *Config) resolveNode() {
	home := AppDir()
	c.NodeDir = env("MYST_NODE_DIR", filepath.Join(home, "node"))
	c.NodeDataDir = env("MYST_NODE_DATA_DIR", filepath.Join(home, "node-data"))

	if c.Managed {
		// A managed node is ours alone: it listens on loopback and the relay
		// is the only thing that talks to it.
		if c.ProxyHost == "" {
			c.ProxyHost = c.TequilaHost
		}
		if env("MYST_TEQUILAPI_URL", "") == "" {
			c.TequilaBaseURL = "http://" + net.JoinHostPort(c.TequilaHost, strconv.Itoa(c.TequilaPort))
		}
		return
	}
	if c.ProxyHost == "" {
		c.ProxyHost = "myst" // the compose service name
	}
}

// resolveIdentity reads the address out of the keystore file so the rest of the
// service never has to be told which identity it is operating.
func (c *Config) resolveIdentity() error {
	raw, err := os.ReadFile(c.IdentityFile)
	if err != nil {
		return fmt.Errorf("identity file %q: %w", c.IdentityFile, err)
	}
	var ks struct {
		Address string `json:"address"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(raw, &ks); err != nil {
		return fmt.Errorf("identity file %q is not a keystore JSON: %w", c.IdentityFile, err)
	}
	if ks.Address == "" {
		return fmt.Errorf("identity file %q has no address field", c.IdentityFile)
	}
	c.IdentityAddress = "0x" + strings.TrimPrefix(strings.ToLower(ks.Address), "0x")
	return nil
}

// KeystoreJSON returns the raw keystore document, used to import the identity
// into the myst node.
func (c *Config) KeystoreJSON() ([]byte, error) { return os.ReadFile(c.IdentityFile) }

// UpstreamProxy is the address of the myst SOCKS5 listener.
func (c *Config) UpstreamProxy() string {
	return net.JoinHostPort(c.ProxyHost, strconv.Itoa(c.ProxyPort))
}

func (c *Config) validate() error {
	if c.SocksListen == "" && c.HTTPListen == "" && len(c.Forwards) == 0 {
		return fmt.Errorf("no frontend enabled: set at least one of RELAY_SOCKS_LISTEN, RELAY_HTTP_LISTEN, RELAY_RAW_LISTEN")
	}
	for _, a := range []struct{ name, addr string }{
		{"RELAY_SOCKS_LISTEN", c.SocksListen},
		{"RELAY_HTTP_LISTEN", c.HTTPListen},
		{"RELAY_HEALTH_LISTEN", c.HealthListen},
	} {
		if a.addr == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(a.addr); err != nil {
			return fmt.Errorf("%s=%q is not host:port: %w", a.name, a.addr, err)
		}
	}
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		return fmt.Errorf("MYST_PROXY_PORT=%d out of range", c.ProxyPort)
	}
	if c.Auth.Username != "" && c.Auth.Password == "" {
		return fmt.Errorf("RELAY_PROXY_USER is set but RELAY_PROXY_PASS is empty")
	}
	if c.ReconnectMin <= 0 || c.ReconnectMax < c.ReconnectMin {
		return fmt.Errorf("invalid reconnect backoff window")
	}
	return nil
}

// parseForwards accepts either the single-forward form (RELAY_RAW_LISTEN +
// RELAY_RAW_TARGET) or the list form (RELAY_RAW_FORWARDS="listen=target,...").
// The list form exists so extra raw ports can be added without code changes.
func parseForwards(listen, target, list string) ([]Forward, error) {
	var out []Forward
	if listen != "" {
		if target == "" {
			return nil, fmt.Errorf("RELAY_RAW_LISTEN is set but RELAY_RAW_TARGET is empty")
		}
		out = append(out, Forward{Listen: listen, Target: target})
	}
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		l, t, ok := strings.Cut(item, "=")
		if !ok {
			return nil, fmt.Errorf("RELAY_RAW_FORWARDS entry %q is not listen=target", item)
		}
		out = append(out, Forward{Listen: strings.TrimSpace(l), Target: strings.TrimSpace(t)})
	}
	for _, f := range out {
		if _, _, err := net.SplitHostPort(f.Listen); err != nil {
			return nil, fmt.Errorf("raw listen %q is not host:port: %w", f.Listen, err)
		}
		if _, _, err := net.SplitHostPort(f.Target); err != nil {
			return nil, fmt.Errorf("raw target %q is not host:port: %w", f.Target, err)
		}
	}
	return out, nil
}

// secret reads NAME, falling back to the contents of the file named by
// NAME_FILE. The file form keeps passphrases out of the process environment.
func secret(name string) (string, error) {
	if v := env(name+"_FILE", ""); v != "" {
		b, err := os.ReadFile(v)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if v, ok := os.LookupEnv(name); ok {
		return v, nil
	}
	if v, ok := fileValues[name]; ok {
		return v, nil
	}
	return "", fmt.Errorf("%s (or %s_FILE) must be set", name, name)
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	if v, ok := fileValues[k]; ok && v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(env(k, "")); err == nil {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(env(k, ""), 64); err == nil {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, err := strconv.ParseBool(env(k, "")); err == nil {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	raw := env(k, "")
	if raw == "" {
		return def
	}
	if v, err := time.ParseDuration(raw); err == nil {
		return v
	}
	// bare numbers are seconds
	if v, err := strconv.Atoi(raw); err == nil {
		return time.Duration(v) * time.Second
	}
	return def
}

func envList(k string, def []string) []string {
	raw := env(k, "")
	if raw == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
