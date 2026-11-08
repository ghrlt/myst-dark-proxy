//go:build !windows

package myst

import (
	"errors"
	"syscall"
)

// refused reports whether a dial failed because nothing is listening there,
// as opposed to this process being unable to try - out of descriptors, out of
// ephemeral ports - or the attempt timing out.
func refused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
