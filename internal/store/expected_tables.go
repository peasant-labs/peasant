package store

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// retiredSearchIndex is the legacy FTS table migration V76 removes.
const retiredSearchIndex = "session_entries_fts"

// ExpectedTableNames lists, by name, every table a database at
// CurrentSchemaVersion holds: it replays the embedded head-schema baseline
// into an in-memory database and reads the tables back, so the list follows
// the real schema (virtual tables and their shadow tables included) instead
// of a hand-kept copy. The legacy session_entries_fts index and its shadow
// tables are left out: V76 drops them once the entries triggers point at
// session_search_fts again, so no current database carries them. The filter
// stays as a guard so a stale snapshot carrying the retired index still
// compares clean.
func ExpectedTableNames() ([]string, error) {
	conn, err := sqlite.OpenConn(":memory:", 0)
	if err != nil {
		return nil, fmt.Errorf("store: open a scratch database for the expected schema: %w", err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteScript(conn, baselineSchemaSQL, nil); err != nil {
		return nil, fmt.Errorf("store: load the head-schema baseline: %w", err)
	}
	var tables []string
	if err := sqlitex.Execute(conn, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			name := stmt.ColumnText(0)
			if name == retiredSearchIndex || strings.HasPrefix(name, retiredSearchIndex+"_") {
				return nil
			}
			tables = append(tables, name)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list the head-schema tables: %w", err)
	}
	return tables, nil
}
