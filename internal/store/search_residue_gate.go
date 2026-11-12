package store

import (
	"fmt"
	"os"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// NeedsSearchResidueRebuild reports whether opening the database at dbPath
// for write would trigger migration v76's whole-index rebuild: the schema is
// exactly 75 and the residue predicate holds. It opens the file read-only
// (SQLITE_OPEN_READONLY plus query_only, mirroring the read-only inspection
// path) and runs no migration, so calling it never modifies the store. A
// missing file is a fresh install, not residue: it reports false.
//
// Non-interactive entry points call this before Open and skip when it
// reports true, so a background hook or server never pays the minutes-long
// rebuild invisibly; terminal entry points open as usual and the v76 notice
// plus rebuild run visibly.
func NeedsSearchResidueRebuild(dbPath string) (bool, error) {
	if dbPath == "" || dbPath == ":memory:" {
		return false, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: stat %s to check for v75 search residue: %w; no files were changed", dbPath, err)
	}
	version, err := SchemaVersionAt(dbPath)
	if err != nil {
		return false, err
	}
	if version != 75 {
		return false, nil
	}
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadOnly)
	if err != nil {
		return false, fmt.Errorf("store: open %s read-only to check for v75 search residue: %w; no files were changed", dbPath, err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA query_only = ON`, nil); err != nil {
		return false, fmt.Errorf("store: pin %s read-only to check for v75 search residue: %w; no files were changed", dbPath, err)
	}
	return searchIndexResiduePresent(conn)
}

// SearchResidueRebuildError is the actionable refusal a non-interactive
// entry point returns instead of opening a store whose migration would
// rebuild the whole search index. The message carries the six actionable
// parts (what, why, where, when, meaning, fix) so a hook log, a server
// response, or a refusal on stderr each explain the skip and name the
// terminal command that performs the repair.
type SearchResidueRebuildError struct {
	// DBPath is the analytics database file the caller refused to open.
	DBPath string
}

// Error renders the six-part refusal. The fix names `peasant harvest` in a
// terminal (which opens the store, prints the rebuild notice, and runs the
// visible whole-index repair) and `peasant web start` in a terminal as the
// alternative that heals the store before forking the background server.
func (e *SearchResidueRebuildError) Error() string {
	return fmt.Sprintf("peasant skipped opening the analytics store at %s because migration v76 would rebuild the whole search index (what failed: the open was refused before any migration ran). "+
		"why: the store is at schema version 75 with stale union-index postings left by 0.16.1 (rows deleted or updated after v75 keep their old terms, and session_entries reuses rowids, so a stale term can match a new, unrelated row). "+
		"where: detected by store.NeedsSearchResidueRebuild on %s before migration v76 ran (internal/store/search_residue_gate.go). "+
		"when: at open, before any migration; the store was not migrated and no files were changed. "+
		"meaning: the requested publish, index, or serve did not run; stored sessions and history are unchanged. "+
		"fix: run `peasant harvest` in a terminal once (it opens the store, prints the rebuild notice, and runs the visible repair), or run `peasant web start` in a terminal", e.DBPath, e.DBPath)
}

// RefuseStaleSearchRebuild reports whether opening dbPath must be skipped:
// interactive callers (isTTY) always proceed so the v76 notice and rebuild
// run visibly, while non-interactive callers skip when the residue check
// holds. It returns nil when the caller should open the store and a
// *SearchResidueRebuildError when it must print the refusal and not open.
func RefuseStaleSearchRebuild(isTTY bool, dbPath string) error {
	if isTTY {
		return nil
	}
	skip, err := NeedsSearchResidueRebuild(dbPath)
	if err != nil {
		return err
	}
	if !skip {
		return nil
	}
	return &SearchResidueRebuildError{DBPath: dbPath}
}
