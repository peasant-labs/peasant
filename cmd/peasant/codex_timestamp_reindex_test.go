package main

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// codexIndexerWithoutTimestamps is the managed Codex indexer revision that
// stored every turn with a zero timestamp (#683).
const codexIndexerWithoutTimestamps = 20

// TestSettledCodexSessionIsReindexedForTimestamps: a Codex session settled by
// the indexer that dropped timestamps must be re-selected by an ordinary
// harvest (no --force) and come back with its rollout timestamps.
func TestSettledCodexSessionIsReindexedForTimestamps(t *testing.T) {
	t.Parallel()
	const id = "019e7724-38f5-7660-b2d7-2eda237d4a7d"
	dir, configPath := harvestFixture(t, string(defaults.HarnessCodex), "codex-fixture", "sessions")
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	indexVersion := func() int {
		t.Helper()
		db, err := openPreparedStore(t, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		conn, err := db.Pool().Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Pool().Put(conn)
		got := -1
		if err := sqlitex.ExecuteTransient(conn, "SELECT index_version FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
			Args: []any{id}, ResultFunc: func(stmt *sqlite.Stmt) error { got = stmt.ColumnInt(0); return nil },
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	setIndexVersion := func(v int) {
		t.Helper()
		db, err := openPreparedStore(t, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		conn, err := db.Pool().Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Pool().Put(conn)
		if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{v, id}}); err != nil {
			t.Fatal(err)
		}
	}

	setIndexVersion(codexIndexerWithoutTimestamps)
	if out, err := executeHarvestCmd(t, dir, []string{"--config=" + configPath}); err != nil {
		t.Fatalf("second harvest: %v\n%s", err, out)
	}
	if got := indexVersion(); got <= codexIndexerWithoutTimestamps {
		t.Fatalf("index_version after an ordinary harvest = %d; a session indexed at %d must be re-indexed", got, codexIndexerWithoutTimestamps)
	}
	for i, turn := range fixtureDetail(t, dir, configPath, id).Turns {
		if turn.Timestamp.Year() != 2024 {
			t.Errorf("turn %d: timestamp %v after re-index, want the rollout's 2024 timestamp", i, turn.Timestamp)
		}
	}
}
