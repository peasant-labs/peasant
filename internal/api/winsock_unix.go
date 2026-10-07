//go:build unix

package api

import (
	"errors"
	"syscall"
)

// addrInUseErrnos and connResetErrnos name every errno the predicates below
// accept on this platform. They exist so winsock_test.go can assert the contract
// without constructing a platform-specific error itself.
var (
	addrInUseErrnos = []error{syscall.EADDRINUSE}
	connResetErrnos = []error{syscall.ECONNRESET}
)

// isAddrInUse reports whether err is a refusal to bind an address already in
// use. On unix that is exactly syscall.EADDRINUSE, so this is the check these
// call sites have always made.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// isConnReset reports whether err is a peer resetting the connection.
func isConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}
