package api

import (
	"errors"
	"fmt"
	"net"
	"testing"
)

// TestIsAddrInUseMatchesARealPortConflict is the test that matters, because it
// does not name an errno at all: it provokes a genuine bind conflict and asserts
// the predicate recognises whatever the platform reports for it.
//
// The bug this guards against is that syscall.EADDRINUSE never matches on
// Windows. Go defines the unix errno names there as synthetic values for source
// compatibility (syscall.EADDRINUSE is 536870914), while a socket actually
// reports Winsock's WSAEADDRINUSE (10048). A test written against the constant
// would have passed on both platforms while the production check silently failed
// on one, so this one goes through the network stack instead.
func TestIsAddrInUseMatchesARealPortConflict(t *testing.T) {
	t.Parallel()
	first, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("could not bind a loopback listener on this host: %v", err)
	}
	defer first.Close()

	second, err := net.Listen("tcp4", first.Addr().String())
	if err == nil {
		second.Close()
		t.Skipf("this host allowed a second bind of %s, so there is no conflict to classify", first.Addr())
	}
	if !isAddrInUse(err) {
		t.Fatalf("isAddrInUse did not recognise a real port conflict: %v", err)
	}
}

// TestPredicatesMatchEveryDocumentedErrno pins that each predicate accepts every
// errno its platform's file declares, including when wrapped, since the call
// sites receive errors wrapped by net and by fmt.Errorf.
func TestPredicatesMatchEveryDocumentedErrno(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		errnos  []error
		matches func(error) bool
	}{
		{"isAddrInUse", addrInUseErrnos, isAddrInUse},
		{"isConnReset", connResetErrnos, isConnReset},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if len(tc.errnos) == 0 {
				t.Fatalf("%s declares no errnos to match", tc.name)
			}
			for _, errno := range tc.errnos {
				if !tc.matches(errno) {
					t.Errorf("%s(%v) = false, want true for a bare errno", tc.name, errno)
				}
				wrapped := fmt.Errorf("listen tcp4 127.0.0.1:1234: bind: %w", errno)
				if !tc.matches(wrapped) {
					t.Errorf("%s did not unwrap to %v", tc.name, errno)
				}
			}
			if tc.matches(errors.New("some unrelated failure")) {
				t.Errorf("%s matched an unrelated error", tc.name)
			}
		})
	}
}
