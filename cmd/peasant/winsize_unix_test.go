//go:build unix

package main

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// setTestTerminalSize sets a pseudo-terminal's window size via TIOCSWINSZ so
// the mounted renderer draws against a stable width and height instead of
// whatever the allocating PTY happened to start with. The dimensions are
// incidental to what these tests assert (cancellation acknowledgment and
// terminal-state restoration, not resize behavior itself), so this seam only
// needs to exist on platforms that have a pseudo-terminal at all. See the
// windows build of this helper.
func setTestTerminalSize(t *testing.T, f *os.File, rows, cols int) {
	t.Helper()
	if err := unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)}); err != nil {
		t.Fatal(err)
	}
}
