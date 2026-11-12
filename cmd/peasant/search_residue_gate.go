package main

import (
	"fmt"
	"os"

	"github.com/peasant-labs/peasant/internal/store"
	"golang.org/x/term"
)

// stdinIsTerminal reports whether standard input is a terminal a user is
// watching. It is a variable (not a direct term.IsTerminal call) so tests
// can stub both sides without a PTY; production always uses the real
// stdin check, the same detector the push and upgrade commands use.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// refuseStaleSearchIndex reports whether opening the analytics store at
// dbPath must be skipped: an interactive caller always proceeds so migration
// v76's notice and whole-index rebuild run visibly, while a non-interactive
// caller skips when the v75 window left search residue (opening would run
// the minutes-long rebuild where nobody watches). It returns nil when the
// caller should open the store and a *store.SearchResidueRebuildError when
// it must print the refusal and not open.
func refuseStaleSearchIndex(dbPath string) error {
	return store.RefuseStaleSearchRebuild(stdinIsTerminal(), dbPath)
}

// maybePreopenStoreForWebStart opens and closes the analytics store so a
// terminal `peasant web start` migrates visibly before forking the detached
// child (whose stdin is not a terminal). On a non-interactive start it
// refuses an affected store instead of forking a server that could never
// start. A failed pre-open only warns here; the child reports an unopenable
// store the same way it always has.
func maybePreopenStoreForWebStart(isTTY bool, dbPath string) error {
	if !isTTY {
		return store.RefuseStaleSearchRebuild(false, dbPath)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: pre-open analytics store %s: %v\n", dbPath, err)
		return nil
	}
	_ = db.Close()
	return nil
}
