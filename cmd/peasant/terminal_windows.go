//go:build windows

package main

import (
	"os"

	"golang.org/x/term"
)

// inputTerminalAvailable reports whether Bubble Tea can read keys: stdin is a
// console, or the process's console input buffer (CONIN$) can be opened, which
// is what Bubble Tea falls back to when stdin is redirected.
func inputTerminalAvailable() bool {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = in.Close()
	return true
}
