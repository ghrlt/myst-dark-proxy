//go:build windows

package main

import (
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
)

// The Windows half of the tray's platform seam: the scheduled task is the
// machine's own supervisor, and a spawned relay must be given no console.

// createNoWindow starts the relay without a console window of its own.
const createNoWindow = 0x08000000

// detachChild keeps a spawned relay off the desktop. Without this a console
// program started from the tray puts a black window on screen and leaves it
// there for as long as the relay runs.
func detachChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// supervisorRunsRelay reports whether the registered task runs the relay rather
// than this tray.
//
// It reads the task's own definition instead of assuming, because both shapes
// are registered under the same name and only the arguments tell them apart. A
// task that starts the tray is not a lever for the relay: running it would put
// a second icon on the panel.
func supervisorRunsRelay() bool {
	if !taskExists() {
		return false
	}
	out, err := exec.Command("schtasks", "/query", "/tn", taskName, "/xml", "ONELINE").Output()
	if err != nil {
		return false
	}
	doc := decodeTaskXML(out)
	return strings.Contains(doc, "run -config") && !strings.Contains(doc, "tray -config")
}

func supervisorStart() error {
	return exec.Command("schtasks", "/run", "/tn", taskName).Run()
}

// supervisorStop ends the running task instance. For a task that runs the relay
// directly - which is the only shape supervisorRunsRelay reports true for -
// that instance is the relay itself.
func supervisorStop() error {
	return exec.Command("schtasks", "/end", "/tn", taskName).Run()
}

// decodeTaskXML turns what schtasks printed into text. It exports UTF-16 with a
// byte order mark; older builds and some locales print plain bytes instead, so
// both are accepted.
func decodeTaskXML(out []byte) string {
	if len(out) < 2 || out[0] != 0xFF || out[1] != 0xFE {
		return string(out)
	}
	units := make([]uint16, 0, (len(out)-2)/2)
	for i := 2; i+1 < len(out); i += 2 {
		units = append(units, uint16(out[i])|uint16(out[i+1])<<8)
	}
	return string(utf16.Decode(units))
}
