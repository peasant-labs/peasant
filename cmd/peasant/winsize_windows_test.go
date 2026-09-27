//go:build windows

package main

import (
	"os"
	"testing"
)

// setTestTerminalSize has no windows implementation: windows has no
// TIOCSWINSZ/ioctl equivalent for a pseudo-terminal's window size. In
// practice this is never reached on windows anyway, because every caller
// first obtains its terminal from openTestTerminal (see prune_exact_test.go),
// which already skips there since the /dev/ptmx pseudo-terminal it allocates
// is Linux-specific. This stub exists only so the windows test binary
// compiles; it fails loudly if it is ever actually reached, since that would
// mean the earlier skip regressed. See the unix build of this helper.
func setTestTerminalSize(t *testing.T, f *os.File, rows, cols int) {
	t.Helper()
	t.Fatal("setTestTerminalSize: no TIOCSWINSZ equivalent on windows; openTestTerminal should have skipped before this point")
}
