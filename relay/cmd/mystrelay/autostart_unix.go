//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
)

func defaultConfigPath() string  { return config.ConfigPath() }
func appDirOf(cfg string) string { return filepath.Dir(cfg) }

// installAutostart writes a systemd unit. "logon" installs a user unit, which
// needs no privileges; "boot" needs a system unit, and this only writes it out
// and says what to run, rather than silently asking for a root password.
func installAutostart(o autostartOptions) error {
	unit := fmt.Sprintf(`[Unit]
Description=myst-relay - fail-closed proxy relay over the Mysterium network
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run -config %s
Restart=always
RestartSec=10

[Install]
WantedBy=%s
`, o.Executable, o.ConfigPath, map[string]string{"logon": "default.target", "boot": "multi-user.target"}[o.Scope])

	switch o.Scope {
	case "logon":
		dir := filepath.Join(userConfigHome(), "systemd", "user")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, taskName+".service")
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			return err
		}
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user daemon-reload: %w\n%s", err, out)
		}
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", taskName+".service").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user enable: %w\n%s", err, out)
		}
		return nil

	case "boot":
		staged := filepath.Join(o.AppDir, taskName+".service")
		if err := os.WriteFile(staged, []byte(unit), 0o644); err != nil {
			return err
		}
		return fmt.Errorf("a boot-time unit needs root. The unit has been written to %s; install it with:\n"+
			"  sudo cp %s /etc/systemd/system/%s.service\n"+
			"  sudo systemctl daemon-reload && sudo systemctl enable --now %s",
			staged, staged, taskName, taskName)

	default:
		return fmt.Errorf("scope must be logon or boot, got %q", o.Scope)
	}
}

func removeAutostart() error {
	_, _ = exec.Command("systemctl", "--user", "disable", "--now", taskName+".service").CombinedOutput()
	path := filepath.Join(userConfigHome(), "systemd", "user", taskName+".service")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	return nil
}

func autostartStatus() (string, error) {
	path := filepath.Join(userConfigHome(), "systemd", "user", taskName+".service")
	if _, err := os.Stat(path); err != nil {
		return fmt.Sprintf("not installed. Register it with: %s autostart install -scope logon", exeName()), nil
	}
	out, _ := exec.Command("systemctl", "--user", "is-active", taskName+".service").CombinedOutput()
	return fmt.Sprintf("installed: %s (%s)", path, strings.TrimSpace(string(out))), nil
}

func userConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config"
	}
	return filepath.Join(home, ".config")
}

// restartHint is what to type to put a newly installed binary into service.
func restartHint() string {
	if _, err := os.Stat(filepath.Join(userConfigHome(), "systemd", "user", taskName+".service")); err == nil {
		return "  systemctl --user restart " + taskName
	}
	if _, err := os.Stat("/etc/systemd/system/" + taskName + ".service"); err == nil {
		return "  sudo systemctl restart " + taskName
	}
	return "  stop it where it is running, then: " + exeName() + " run"
}
