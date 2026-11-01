//go:build windows

package main

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/term"
)

// TestRequireTerminalRefusesRedirectedStdout checks the Windows probe on the
// case `go test` always provides: stdout is redirected, so a Bubble Tea
// program would draw into a pipe and must be refused with the clean error.
func TestRequireTerminalRefusesRedirectedStdout(t *testing.T) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		t.Skip("stdout is a console; this test needs it redirected")
	}
	var noTTY noTerminalError
	if err := requireTerminal("tui"); !errors.As(err, &noTTY) {
		t.Fatalf("requireTerminal with redirected stdout = %v, want noTerminalError", err)
	}
}
