//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// The Linux half of the tray's platform seam: the systemd user unit is the
// machine's own supervisor, and a spawned relay wants a session of its own.

// unitName is what autostart_unix.go registers.
const unitName = taskName + ".service"

// detachChild puts the relay in its own process group.
//
// Without it the relay shares the tray's group and a Ctrl-C meant for the tray
// in a terminal would take the proxy down with it - which is the one thing a
// kill switch's own supervisor should not do by accident.
func detachChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// supervisorRunsRelay reports whether the systemd user unit is what runs the
// relay here. An inactive or absent unit is not a lever: there is nothing for
// it to stop, and starting it would fight with whatever is running instead.
func supervisorRunsRelay() bool {
	return systemctl("is-active", "--quiet") == nil
}

func supervisorStart() error { return systemctl("start") }

// supervisorStop stops the unit rather than the relay.
//
// The unit carries Restart=always, so a relay asked to exit cleanly is exactly
// the case that rule exists for and it would be back within ten seconds. Only
// systemd can make it stay stopped.
func supervisorStop() error { return systemctl("stop") }

func systemctl(args ...string) error {
	return exec.Command("systemctl", append(append([]string{"--user"}, args...), unitName)...).Run()
}
