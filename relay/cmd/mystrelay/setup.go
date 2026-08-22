package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/node"
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// cmdSetup writes a config file, installs the node, and optionally registers
// autostart. Every answer can also be supplied as a flag so the whole thing can
// be scripted.
func cmdSetup(args []string) error {
	fs := newFlagSet("setup")
	var (
		identity   = fs.String("identity", "", "path to the Mysterium keystore JSON (UTC--...--<address>)")
		passphrase = fs.String("passphrase", "", "passphrase for that keystore (usually empty)")
		passFile   = fs.String("passphrase-file", "", "read the passphrase from this file instead")
		socksPort  = fs.Int("socks-port", 7362, "fixed local SOCKS5 port")
		httpPort   = fs.Int("http-port", 7360, "fixed local HTTP proxy port")
		rawPort    = fs.Int("raw-port", 7339, "fixed local raw-forward port (needs -raw-target)")
		rawTarget  = fs.String("raw-target", "", "host:port the raw port forwards to")
		healthPort = fs.Int("health-port", 7348, "fixed local health/status port")
		country    = fs.String("country", "", "restrict exits to these countries, comma separated (e.g. NL,DE)")
		autostart  = fs.String("autostart", "", "none | logon | boot  (prompted for when omitted)")
		noPrompt   = fs.Bool("yes", false, "never prompt; use flags and defaults")
		home       = fs.String("home", "", "install directory (default "+config.AppDir()+")")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	appDir := *home
	if appDir == "" {
		appDir = config.AppDir()
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}

	in := bufio.NewReader(os.Stdin)
	ask := func(prompt, def string) string {
		if *noPrompt {
			return def
		}
		if def != "" {
			fmt.Printf("%s [%s]: ", prompt, def)
		} else {
			fmt.Printf("%s: ", prompt)
		}
		line, _ := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		return line
	}

	fmt.Printf("myst-relay setup\n\nInstalling into %s\n\n", appDir)

	// ---- identity -------------------------------------------------------
	idPath := *identity
	for {
		if idPath == "" {
			fmt.Println("The Mysterium keystore file is the UTC--<date>--<address> JSON your")
			fmt.Println("Mysterium app wrote. Copy it to this machine and give the path here.")
			idPath = ask("Keystore file", "")
		}
		idPath = strings.Trim(strings.TrimSpace(idPath), "\"'")
		addr, err := keystoreAddress(idPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %v\n\n", err)
			if *noPrompt {
				return err
			}
			idPath = ""
			continue
		}
		fmt.Printf("  identity %s\n\n", addr)
		break
	}

	// Copy it in, so the install does not break when the original moves.
	installedID := filepath.Join(appDir, "identity.json")
	if err := copyFile(idPath, installedID, 0o600); err != nil {
		return fmt.Errorf("copy keystore into %s: %w", appDir, err)
	}

	pass := *passphrase
	switch {
	case *passFile != "":
		b, err := os.ReadFile(*passFile)
		if err != nil {
			return err
		}
		pass = strings.TrimRight(string(b), "\r\n")
	case pass == "" && !*noPrompt:
		fmt.Println("Passphrase for that keystore. The Mysterium desktop app normally uses")
		fmt.Println("an empty one, so pressing Enter is usually right.")
		fmt.Println("(what you type here is visible; use -passphrase-file to avoid that)")
		pass = ask("Passphrase", "")
		fmt.Println()
	}

	// ---- ports ----------------------------------------------------------
	if !*noPrompt {
		fmt.Println("Fixed local ports. Point your browser and apps at these.")
		*socksPort = askInt(ask, "SOCKS5 port", *socksPort)
		*httpPort = askInt(ask, "HTTP proxy port", *httpPort)
		*healthPort = askInt(ask, "Status port", *healthPort)
		fmt.Println()
	}

	// ---- exit policy ----------------------------------------------------
	if !*noPrompt && *country == "" {
		fmt.Println("Restrict which countries traffic may leave from? Comma separated,")
		fmt.Println("e.g. NL,DE. Leave empty to use the best-scoring exit anywhere.")
		*country = ask("Exit countries", "")
		fmt.Println()
	}

	// ---- autostart ------------------------------------------------------
	choice := strings.ToLower(*autostart)
	if choice == "" && !*noPrompt {
		fmt.Println("Start automatically?")
		fmt.Println("  logon  at sign-in, as you            (no admin needed)")
		fmt.Println("  boot   before sign-in, as the system (needs admin)")
		fmt.Println("  none   start it yourself")
		choice = strings.ToLower(ask("Autostart", "logon"))
		fmt.Println()
	}
	if choice == "" {
		choice = "none"
	}
	if choice != "none" && choice != "logon" && choice != "boot" {
		return fmt.Errorf("autostart must be none, logon or boot, got %q", choice)
	}

	// ---- write the config ----------------------------------------------
	cfgPath := filepath.Join(appDir, "config.env")
	logFile := filepath.Join(appDir, "logs", "relay.log")
	settings := configFile(configValues{
		AppDir:     appDir,
		Identity:   installedID,
		Passphrase: pass,
		SocksPort:  *socksPort,
		HTTPPort:   *httpPort,
		RawPort:    *rawPort,
		RawTarget:  *rawTarget,
		HealthPort: *healthPort,
		Countries:  *country,
		LogFile:    logFile,
	})
	if err := os.WriteFile(cfgPath, []byte(settings), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", cfgPath)

	// ---- fetch the node -------------------------------------------------
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	nodeDir := filepath.Join(appDir, "node")
	binary, err := node.Ensure(context.Background(), nodeDir, node.DefaultVersion, log)
	if err != nil {
		return fmt.Errorf("install the Mysterium node: %w", err)
	}
	fmt.Printf("node ready at %s\n", binary)

	// ---- autostart ------------------------------------------------------
	if choice != "none" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := installAutostart(autostartOptions{
			Scope:      choice,
			Executable: exe,
			ConfigPath: cfgPath,
			AppDir:     appDir,
		}); err != nil {
			return fmt.Errorf("register autostart: %w", err)
		}
		fmt.Printf("autostart registered (%s)\n", choice)
	}

	fmt.Printf(`
Setup complete.

  start it now      %s run
  check it          %s status
  prove it works    %s verify

Point your applications at:
  SOCKS5   127.0.0.1:%d      (prefer this; DNS resolves at the exit)
  HTTP     127.0.0.1:%d
`, exeName(), exeName(), exeName(), *socksPort, *httpPort)
	if choice != "none" {
		fmt.Printf("\nIt will also start automatically at %s.\n", choice)
	}
	fmt.Printf("\nBrowser settings that matter for leaks are in docs/WINDOWS.md.\n")
	return nil
}

func askInt(ask func(string, string) string, prompt string, def int) int {
	for {
		got := ask(prompt, strconv.Itoa(def))
		n, err := strconv.Atoi(got)
		if err == nil && n > 0 && n < 65536 {
			return n
		}
		fmt.Fprintf(os.Stderr, "  %q is not a port number\n", got)
	}
}

type configValues struct {
	AppDir     string
	Identity   string
	Passphrase string
	SocksPort  int
	HTTPPort   int
	RawPort    int
	RawTarget  string
	HealthPort int
	Countries  string
	LogFile    string
}

func configFile(v configValues) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("# myst-relay configuration - written by \"mystrelay setup\".")
	w("# Environment variables override anything here.")
	w("")
	w("# The node is downloaded, started and supervised by the relay itself.")
	w("MYST_MANAGED=true")
	w("MYST_NODE_VERSION=%s", node.DefaultVersion)
	w("MYST_NODE_DIR=%s", filepath.Join(v.AppDir, "node"))
	w("MYST_NODE_DATA_DIR=%s", filepath.Join(v.AppDir, "node-data"))
	w("")
	w("# Identity. The file is a copy; the original is not touched.")
	w("MYST_IDENTITY_FILE=%s", v.Identity)
	w("MYST_IDENTITY_PASSPHRASE=%s", v.Passphrase)
	w("")
	w("# Fixed local ports. Loopback only: nothing on the network can use them.")
	w("RELAY_SOCKS_LISTEN=127.0.0.1:%d", v.SocksPort)
	w("RELAY_HTTP_LISTEN=127.0.0.1:%d", v.HTTPPort)
	w("RELAY_HEALTH_LISTEN=127.0.0.1:%d", v.HealthPort)
	if v.RawPort > 0 && v.RawTarget != "" {
		w("RELAY_RAW_LISTEN=127.0.0.1:%d", v.RawPort)
		w("RELAY_RAW_TARGET=%s", v.RawTarget)
	} else {
		w("# RELAY_RAW_LISTEN=127.0.0.1:7339")
		w("# RELAY_RAW_TARGET=example.com:443")
	}
	w("")
	w("# Kill switch. strict also tears down streams that are already open.")
	w("RELAY_KILLSWITCH_STRICT=true")
	w("")
	w("# Sessions cost MYST by the hour as well as by the gibibyte, so one is")
	w("# opened when a client first connects and closed again once nothing has")
	w("# used it for the idle timeout. Set RELAY_ON_DEMAND=false and")
	w("# RELAY_SESSION_IDLE_TIMEOUT=0 to hold one open all the time instead.")
	w("RELAY_ON_DEMAND=true")
	w("RELAY_SESSION_IDLE_TIMEOUT=15m")
	w("# RELAY_ACTIVATION_TIMEOUT=60s")
	w("")
	w("# Exit selection.")
	if v.Countries != "" {
		w("EXIT_COUNTRY=%s", v.Countries)
	} else {
		w("# EXIT_COUNTRY=NL,DE")
	}
	w("# EXIT_IP_TYPE=residential")
	w("EXIT_MIN_QUALITY=1.5")
	w("# RELAY_ROTATE_EVERY=30m")
	w("")
	w("RELAY_LOG_FILE=%s", v.LogFile)
	w("RELAY_LOG_LEVEL=info")
	return b.String()
}

// keystoreAddress validates that path really is a keystore and returns its
// address, so setup fails on the wrong file rather than at first connection.
func keystoreAddress(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
	var ks struct {
		Address string `json:"address"`
		Crypto  any    `json:"crypto"`
	}
	if err := json.Unmarshal(raw, &ks); err != nil {
		return "", fmt.Errorf("%s is not a keystore JSON file", path)
	}
	if ks.Address == "" {
		return "", fmt.Errorf("%s has no \"address\" field, so it is not a keystore", path)
	}
	return "0x" + strings.TrimPrefix(strings.ToLower(ks.Address), "0x"), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

func exeName() string {
	exe, err := os.Executable()
	if err != nil {
		return "mystrelay"
	}
	return filepath.Base(exe)
}
