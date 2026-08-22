//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
)

func defaultConfigPath() string  { return config.ConfigPath() }
func appDirOf(cfg string) string { return filepath.Dir(cfg) }

// installAutostart registers a Scheduled Task.
//
// The relay is a console program, so launching it directly at logon would leave
// a console window on screen for as long as it runs. PowerShell starts it
// hidden instead, which is the least surprising thing for something meant to
// sit in the background. A "boot" task runs as SYSTEM in session 0 and has no
// window to hide in the first place.
func installAutostart(o autostartOptions) error {
	quoted := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	launch := fmt.Sprintf(
		`powershell -NoProfile -WindowStyle Hidden -Command "Start-Process -FilePath %s -ArgumentList 'run','-config',%s -WindowStyle Hidden"`,
		quoted(o.Executable), quoted(o.ConfigPath))

	args := []string{"/create", "/tn", taskName, "/tr", launch, "/f"}
	switch o.Scope {
	case "logon":
		args = append(args, "/sc", "onlogon", "/rl", "limited")
	case "boot":
		args = append(args, "/sc", "onstart", "/ru", "SYSTEM", "/rl", "highest")
	default:
		return fmt.Errorf("scope must be logon or boot, got %q", o.Scope)
	}

	out, err := exec.Command("schtasks", args...).CombinedOutput()
	if err != nil {
		hint := ""
		if o.Scope == "boot" {
			hint = "\nA boot task must be created from an Administrator prompt."
		}
		return fmt.Errorf("schtasks: %w\n%s%s", err, strings.TrimSpace(string(out)), hint)
	}
	return nil
}

func removeAutostart() error {
	out, err := exec.Command("schtasks", "/delete", "/tn", taskName, "/f").CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if strings.Contains(strings.ToLower(text), "cannot find") {
			return nil
		}
		return fmt.Errorf("schtasks: %w\n%s", err, text)
	}
	return nil
}

func autostartStatus() (string, error) {
	out, err := exec.Command("schtasks", "/query", "/tn", taskName, "/fo", "list").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("not installed. Register it with: %s autostart install -scope logon", exeName()), nil
	}
	return strings.TrimSpace(string(out)), nil
}
