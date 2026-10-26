//go:build windows

package api

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// Socket errors on Windows come from Winsock and carry WSA* codes, while Go
// defines the unix errno names in package syscall as synthetic values for source
// compatibility only — syscall.EADDRINUSE is 536870914 there, and no error a
// socket returns ever equals it. Comparing against the unix name alone is
// therefore always false on Windows, which is why both names are accepted here.
//
// The WSA constants come from golang.org/x/sys/windows, already required by this
// module for the session lock and the background process attributes, so this adds
// no dependency.
var (
	addrInUseErrnos = []error{syscall.EADDRINUSE, windows.WSAEADDRINUSE}
	connResetErrnos = []error{syscall.ECONNRESET, windows.WSAECONNRESET}
)

// isAddrInUse reports whether err is a refusal to bind an address already in
// use. Winsock reports WSAEADDRINUSE (10048) for this.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, windows.WSAEADDRINUSE)
}

// isConnReset reports whether err is a peer resetting the connection. Winsock
// reports WSAECONNRESET (10054), and its message is "An existing connection was
// forcibly closed by the remote host" rather than the unix "connection reset by
// peer", so a string match on the unix wording does not catch it either.
func isConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, windows.WSAECONNRESET)
}
