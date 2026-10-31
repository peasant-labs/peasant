//go:build unix

package main

import (
	"os"

	"golang.org/x/term"
)

// inputTerminalAvailable reports whether Bubble Tea can read keys: stdin is a
// terminal, or the process's controlling terminal (/dev/tty) can be opened,
// which is what Bubble Tea falls back to when stdin is redirected.
func inputTerminalAvailable() bool {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}
