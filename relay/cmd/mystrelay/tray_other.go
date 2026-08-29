//go:build !windows && !linux

package main

import "fmt"

// cmdTray keeps the command table the same shape on every platform, and says
// the same thing on the ones with no notification area this program knows how
// to reach: the plain commands are what an install here uses instead.
func cmdTray([]string) error {
	return fmt.Errorf("there is no notification area this build knows how to use here\n\nThe same things are commands:\n  %[1]s status    what it is doing, and what it has cost\n  %[1]s balance   what the identity has left\n  %[1]s rotate    change exit node\n  %[1]s stop      stop the relay",
		exeName())
}
