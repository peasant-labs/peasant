package storetest

import (
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// ResidueTerm is one docsize posting a residue fixture seeds: the rowid and
// the text whose token count the residue predicate compares.
type ResidueTerm struct {
	ID   int64
	Text string
}

// AffectedUnion, AffectedLegacy, CleanUnion, and CleanLegacy are the one
// shared affected/clean term sets every residue test seeds: affected carries
// union residue (a deleted row's stale posting), clean carries identical
// postings. Sharing them keeps the gate, push, web, sync, and entry-point
// tests pinned to the same predicate.
var (
	AffectedUnion  = []ResidueTerm{{ID: 1, Text: "kangaroo ghost"}, {ID: 2, Text: "keep me"}}
	AffectedLegacy = []ResidueTerm{{ID: 2, Text: "keep me"}}
	CleanUnion     = []ResidueTerm{{ID: 1, Text: "keep me"}, {ID: 2, Text: "steady state"}}
	CleanLegacy    = []ResidueTerm{{ID: 1, Text: "keep me"}, {ID: 2, Text: "steady state"}}
)

// SeedResidueDB builds the minimal store the residue gate reads: a stamped
// user_version plus the two FTS docsize tables seeded from the given rows.
// Only what store.NeedsSearchResidueRebuild inspects exists, so cases pin
// the gate and nothing else. It mirrors the store package's own fixture
// builder (which cannot be shared across the package boundary); keep the
// two in step.
func SeedResidueDB(t *testing.T, path string, version int, unionTerms, legacyTerms []ResidueTerm) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close residue fixture database: %v", err)
		}
	}()
	seedTerms := func(table string, terms []ResidueTerm) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, `CREATE VIRTUAL TABLE `+table+` USING fts5(t)`, nil); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
		for _, term := range terms {
			if err := sqlitex.ExecuteTransient(conn, `INSERT INTO `+table+`(rowid, t) VALUES (?, ?)`, &sqlitex.ExecOptions{
				Args: []any{term.ID, term.Text},
			}); err != nil {
				t.Fatalf("seed %s row %d: %v", table, term.ID, err)
			}
		}
	}
	seedTerms("session_search_fts", unionTerms)
	seedTerms("session_entries_fts", legacyTerms)
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf(`PRAGMA user_version = %d`, version), nil); err != nil {
		t.Fatalf("stamp user_version %d: %v", version, err)
	}
}
