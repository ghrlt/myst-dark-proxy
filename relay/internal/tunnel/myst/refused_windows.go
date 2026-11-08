package myst

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is what Windows answers a connect to a port nobody listens
// on - and, unlike Unix, one beyond a full accept backlog too. The syscall
// package has no name for it, and its ECONNREFUSED is an invented value that
// a real refusal never matches.
const wsaeconnrefused = syscall.Errno(10061)

// refused reports whether a dial failed because nothing is listening there,
// as opposed to this process being unable to try - out of sockets
// (WSAENOBUFS), out of ephemeral ports - or the attempt timing out.
func refused(err error) bool {
	return errors.Is(err, wsaeconnrefused) || errors.Is(err, syscall.ECONNREFUSED)
}
