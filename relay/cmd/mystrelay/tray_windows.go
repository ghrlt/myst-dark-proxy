//go:build windows

package main

import (
	"os/exec"
	"strings"
	"syscall"
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
	reg, ok := readTask()
	if !ok {
		return false
	}
	action := strings.Join(reg.Arguments, " ")
	return strings.Contains(action, "run -config") && !strings.Contains(action, "tray -config")
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

// openPath opens a file or folder the way double-clicking it would: Explorer
// for a folder, or the file's own default handler. Start rather than Run,
// because Explorer answers a launch like this with exit codes that mean
// nothing - waiting on it would report a spurious failure on an open that
// worked.
func openPath(path string) error {
	return exec.Command("explorer.exe", path).Start()
}
