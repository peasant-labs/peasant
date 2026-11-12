package main

import (
	"io"
	"os"

	"github.com/peasant-labs/peasant/internal/tui/discoveryprogress"
	"golang.org/x/term"
)

type discoverySpinner = discoveryprogress.Spinner

// newDiscoverySpinner leaves discovery quiet when stderr is not a terminal.
func newDiscoverySpinner(w io.Writer) *discoverySpinner {
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	return discoveryprogress.New(w, isTTY)
}
