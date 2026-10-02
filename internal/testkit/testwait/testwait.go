// Package testwait bounds a test's waits with the test's own deadline, so a
// wait that never ends fails with its own message before the test binary's
// timeout panic. It imports only the standard library so that white-box
// package tests such as package ingest can import it. The compiler enforces
// the part that matters: if it ever reached internal/ingest, the white-box
// package ingest tests would fail to build with an import cycle. The rest of
// the stdlib-only claim is code-review evidence.
//
// T requires Deadline(), which only *testing.T provides: *testing.B and
// *testing.F cannot use this package. Every current caller is a *testing.T.
//
// Deadline() must not be called inside a testing/synctest bubble, so these
// helpers are not for bubbles; use synctest.Wait() there.
//
// Context registers its cancel with t.Cleanup. Call it once per wait scope
// (for example once before a retry loop), never once per loop iteration.
// Receive and Until need no cleanup: they cancel their own bound on return.
package testwait

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// T is the test handle these helpers need: the testing.TB surface plus
// Deadline, which only *testing.T provides.
type T interface {
	testing.TB
	Deadline() (time.Time, bool)
}

// The concrete test type must keep satisfying T; a signature change on
// *testing.T breaks this build rather than the callers'.
var _ T = (*testing.T)(nil)

const (
	// fallbackBudget bounds a wait when the test binary has no deadline
	// (`go test -timeout=0`): there is no deadline to stay ahead of, so the
	// wait still ends well inside the binary's own lifetime.
	fallbackBudget = time.Minute
	// maxHeadroom is the most a bound may precede the test deadline. On a
	// loaded machine that leaves comfortable headroom; a test already close to
	// its deadline still fails early instead of at the timeout panic.
	maxHeadroom = 5 * time.Second
	// untilTick is how often Until re-checks a condition that has no push
	// signal.
	untilTick = time.Millisecond
)

// source names where a wait bound came from, so a failure can say why it
// landed where it did.
type source int

const (
	// sourceDeadline: the bound is derived from the test binary's deadline.
	sourceDeadline source = iota
	// sourceFallback: the test binary has no deadline, so the fallback budget
	// set the bound.
	sourceFallback
)

func (s source) String() string {
	switch s {
	case sourceDeadline:
		return "test binary deadline"
	case sourceFallback:
		return "fallback budget"
	default:
		return fmt.Sprintf("unrecognized bound source %d", int(s))
	}
}

// bound computes the instant a wait must end. Without a deadline it is now
// plus fallbackBudget. With one it is the deadline minus a headroom capped at
// maxHeadroom (before the panic, but never past the deadline); a deadline that
// has already passed clamps the bound to now.
func bound(now, deadline time.Time, ok bool) (time.Time, source) {
	if !ok {
		return now.Add(fallbackBudget), sourceFallback
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return now, sourceDeadline
	}
	return deadline.Add(-min(maxHeadroom, remaining/2)), sourceDeadline
}

// Context returns a context derived from t.Context() that also ends at the
// wait bound. Its cancel is registered with t.Cleanup. Its cause names the
// bound source (binary deadline or fallback budget).
func Context(t T) context.Context {
	t.Helper()
	now := time.Now()
	deadline, ok := t.Deadline()
	at, src := bound(now, deadline, ok)
	ctx, cancel := context.WithCancelCause(t.Context())
	timer := time.AfterFunc(at.Sub(now), func() {
		cancel(fmt.Errorf(
			"test wait bound reached at %s (bound source: %s): the awaited state was not reached before the test's own bound; check that the double or producer signals this state; see TESTING.md, Waiting in tests",
			at.Format(time.RFC3339Nano), src))
	})
	t.Cleanup(func() {
		timer.Stop()
		cancel(nil)
	})
	return ctx
}

// Receive returns the next value from ch, or fails with an actionable message
// when the bound ends first.
func Receive[V any](t T, ch <-chan V, what string) V {
	t.Helper()
	now := time.Now()
	deadline, ok := t.Deadline()
	at, src := bound(now, deadline, ok)
	timer := time.NewTimer(at.Sub(now))
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf(
			"testwait.Receive(%q): no value arrived before the wait bound %s (bound source: %s); the awaited state was never reached, so the wait cannot continue; check that the double or producer signals this state; see TESTING.md, Waiting in tests",
			what, at.Format(time.RFC3339Nano), src)
		var zero V
		return zero
	}
}

// Until returns once cond reports true, re-checking on a 1 ms ticker. Use it
// only where no push signal exists, and name that reason at the call site.
func Until(t T, what string, cond func() bool) {
	t.Helper()
	if cond() {
		return
	}
	now := time.Now()
	deadline, ok := t.Deadline()
	at, src := bound(now, deadline, ok)
	timer := time.NewTimer(at.Sub(now))
	defer timer.Stop()
	ticker := time.NewTicker(untilTick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if cond() {
				return
			}
		case <-timer.C:
			if cond() {
				return
			}
			t.Fatalf(
				"testwait.Until(%q): the condition was still false at the wait bound %s (bound source: %s); the awaited state was never reached, so the wait cannot continue; check that the double or producer signals this state; see TESTING.md, Waiting in tests",
				what, at.Format(time.RFC3339Nano), src)
		}
	}
}
