//go:build !windows

package main

import (
	"fmt"
	"log/slog"
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
	unit := unitFile(o)

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
		return writeTrayAutostart(o)

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

// unitFile is the systemd unit that runs the relay.
//
// The comment at the top is not only for the reader. The status icon is a
// separate session entry, so a unit alone cannot say whether an icon was
// wanted and left out or simply predates the idea - and that line, written by
// every build that has an icon to leave out, is the difference. See
// adoptStatusIcon.
func unitFile(o autostartOptions) string {
	return fmt.Sprintf(`# Written by "mystrelay autostart install". The status icon is not started
# here: it is a session entry of its own, which -tray=false leaves out.
[Unit]
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
}

// unitMarker is the line above that says a unit knows about the status icon.
const unitMarker = "The status icon is not started"

// writeTrayAutostart adds the status icon to what the desktop session starts.
//
// The unit is what runs the relay here - systemd is a better supervisor than
// anything this program could be - so the icon is a separate entry that
// attaches to it, rather than the arrangement Windows gets where the icon owns
// the relay. Removing the entry is all it takes to go back to a relay with no
// icon; the relay itself is unaffected either way.
func writeTrayAutostart(o autostartOptions) error {
	path := trayDesktopPath()
	if !o.Tray {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	entry := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=myst-relay status icon
Comment=Status, usage, balance and exit control for the myst-relay proxy
Exec=%s tray -config %s
Terminal=false
X-GNOME-Autostart-enabled=true
`, o.Executable, o.ConfigPath)
	return os.WriteFile(path, []byte(entry), 0o644)
}

func trayDesktopPath() string {
	return filepath.Join(userConfigHome(), "autostart", taskName+"-tray.desktop")
}

func userUnitPath() string {
	return filepath.Join(userConfigHome(), "systemd", "user", taskName+".service")
}

// adoptStatusIcon gives the status icon to a registration made before there
// was one.
//
// Every build up to v0.5.0 wrote a user unit and nothing else, because there
// was no icon to start. Replacing the binary leaves that unit as it was, so a
// session that has been running the relay since before the icon existed would
// never see one - which is the whole of what its owner sees of this feature,
// and not something they can be expected to know to go and fix by hand. So the
// relay adds the missing session entry itself, once, on a start where it can
// see that this is what happened.
//
// Only a user unit is looked at: the relay under the system unit runs as root,
// whose session is nobody's desktop, and in a container there is no unit here
// at all. And only one written before the marker existed, so a session entry
// left out on purpose by a build that could have written one stays left out.
func adoptStatusIcon(log *slog.Logger) {
	unit, err := os.ReadFile(userUnitPath())
	if err != nil || strings.Contains(string(unit), unitMarker) {
		return
	}
	if _, err := os.Stat(trayDesktopPath()); err == nil {
		return
	}
	exe, cfgPath, ok := unitLaunch(string(unit))
	if !ok {
		return
	}
	self, err := os.Executable()
	if err != nil || filepath.Clean(self) != filepath.Clean(exe) {
		return
	}
	o := autostartOptions{
		Scope:      "logon",
		Executable: exe,
		ConfigPath: cfgPath,
		AppDir:     appDirOf(cfgPath),
		Tray:       true,
	}
	if err := writeTrayAutostart(o); err != nil {
		log.Warn("could not add the status icon to the session", "error", err)
		return
	}
	// The unit is rewritten only to date it, so that this is done once and a
	// later "-tray=false" is not undone by the next start.
	if err := os.WriteFile(userUnitPath(), []byte(unitFile(o)), 0o644); err == nil {
		_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	}
	log.Info("the desktop session now starts the status icon",
		"entry", trayDesktopPath(), "icon_from", "the next sign-in, or "+exeName()+" tray")
}

// unitLaunch reads the executable and config path back out of a unit's
// ExecStart line, which is written as: ExecStart=<exe> run -config <path>
func unitLaunch(unit string) (exe, cfgPath string, ok bool) {
	for _, line := range strings.Split(unit, "\n") {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 4 || fields[len(fields)-2] != "-config" {
			return "", "", false
		}
		return fields[0], fields[len(fields)-1], true
	}
	return "", "", false
}

func removeAutostart() error {
	_, _ = exec.Command("systemctl", "--user", "disable", "--now", taskName+".service").CombinedOutput()
	path := filepath.Join(userConfigHome(), "systemd", "user", taskName+".service")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(trayDesktopPath()); err != nil && !os.IsNotExist(err) {
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
	status := fmt.Sprintf("installed: %s (%s)", path, strings.TrimSpace(string(out)))
	if _, err := os.Stat(trayDesktopPath()); err == nil {
		status += "\nstatus icon: " + trayDesktopPath()
	}
	return status, nil
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
