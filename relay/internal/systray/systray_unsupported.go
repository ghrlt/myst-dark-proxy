//go:build !windows && !linux

package systray

import "fmt"

// A desktop this package has not been taught. The vocabulary still compiles, so
// callers can be written once, but there is no notification area to put an icon
// in and Run says so rather than pretending.

// Tray is the handle Run would have returned. Nothing here has an icon to
// change, so its methods do nothing.
type Tray struct{}

func (t *Tray) Set(Colour, string)          {}
func (t *Tray) Notify(string, string, bool) {}
func (t *Tray) Quit()                       {}

// Run reports that this platform has no notification area.
func Run(Options) error {
	return fmt.Errorf("no notification area is supported on this platform")
}

// Show and Warn fall back to the terminal, which is where a program with no
// desktop to draw on was started from.
func Show(title, body string) { fmt.Printf("\n%s\n\n%s\n", title, body) }
func Warn(title, body string) { Show(title, body) }

// DetachConsole has nothing to detach here.
func DetachConsole() {}
