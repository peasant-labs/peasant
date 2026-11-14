package storetest

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// legacySearchIndexScript re-creates the retired session_entries_fts index
// and points the three session_entries triggers at it: the shape migration
// v75 left behind. The trigger text mirrors the retired triggers (store's
// shared migrateSearchTrigger definitions with only the table name
// swapped, which is exactly how they read); this package cannot reference
// those unexported definitions, so the SQL is spelled out here and kept in
// step with them by the transition-window tests that assert both indexes.
const legacySearchIndexScript = `DROP TRIGGER IF EXISTS session_entries_ai;
DROP TRIGGER IF EXISTS session_entries_ad;
DROP TRIGGER IF EXISTS session_entries_au;
CREATE VIRTUAL TABLE session_entries_fts USING fts5(
    content_preview,
    tool_input,
    tool_output,
    session_id UNINDEXED,
    entry_index UNINDEXED,
    content='session_entries',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER session_entries_ai AFTER INSERT ON session_entries BEGIN
    INSERT INTO session_entries_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES (new.rowid, new.content_preview, new.tool_input, new.tool_output, new.session_id, new.entry_index);
END;
CREATE TRIGGER session_entries_ad AFTER DELETE ON session_entries BEGIN
    INSERT INTO session_entries_fts(session_entries_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES ('delete', old.rowid, old.content_preview, old.tool_input, old.tool_output, old.session_id, old.entry_index);
END;
CREATE TRIGGER session_entries_au AFTER UPDATE ON session_entries BEGIN
    INSERT INTO session_entries_fts(session_entries_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES ('delete', old.rowid, old.content_preview, old.tool_input, old.tool_output, old.session_id, old.entry_index);
    INSERT INTO session_entries_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES (new.rowid, new.content_preview, new.tool_input, new.tool_output, new.session_id, new.entry_index);
END;`

// SeedLegacySearchIndex reconstructs the pre-consolidation dual-index state
// on a store V76 already consolidated at open: the union entries triggers
// give way to the retired session_entries_fts index and its three sync
// triggers, so transition-window reads and migrateConsolidateSearch have the
// retired side to work with.
// Production can no longer reach this state (Open applies V76 first); only
// tests of the transition window rebuild it.
func SeedLegacySearchIndex(t *testing.T, s *store.Store) {
	t.Helper()
	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Put(conn)
	if err := sqlitex.ExecuteScript(conn, legacySearchIndexScript, nil); err != nil {
		t.Fatalf("seed the retired search index: %v", err)
	}
}

// undoImportProfileScript undoes what migration v79 adds, so a store
// built from the current schema can be stamped as an earlier version and have
// that migration applied to it again: the index_log timing columns, the stage
// table, the outcome lookup tables, and the reason-id shape of
// ingest_run_outcomes, which returns to its kind and reason text columns.
const undoImportProfileScript = `
ALTER TABLE index_log DROP COLUMN parse_ms;
ALTER TABLE index_log DROP COLUMN write_ms;
ALTER TABLE index_log DROP COLUMN bytes;
DROP TABLE ingest_run_stages;
DROP INDEX idx_ingest_run_outcomes_run;
DROP TABLE ingest_run_outcomes;
DROP TABLE ingest_outcome_reasons;
DROP TABLE ingest_outcome_kinds;
CREATE TABLE ingest_run_outcomes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES ingest_log(id) ON DELETE CASCADE,
    session_id TEXT,
    kind TEXT NOT NULL CHECK(kind IN ('worker_error', 'diagnostic')),
    reason_code TEXT NOT NULL CHECK(length(reason_code) > 0),
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_ingest_run_outcomes_run ON ingest_run_outcomes(run_id, id);
`

// BuildCleanV75Store writes a real, clean version-75 store at path: it opens
// the current schema (so every table, view, and trigger is the shipped
// shape), seeds one session entry through the live triggers, then undoes
// exactly what migration v76 changes - it re-creates the retired
// session_entries_fts index with the v75 definition, points the three
// session_entries triggers back at it, rebuilds it from session_entries so
// both search indexes agree, undoes the two later migrations, and stamps
// user_version 75. Migration v76 only re-points those triggers and drops that
// index, so the result is the v75 schema with no residue: upgrading it runs
// v76 on the targeted-insert path. Undoing the later migrations drops
// pending_recounts (v77) and the import profile tables and columns (v79), so
// the chain from 75 creates them again instead of colliding.
func BuildCleanV75Store(t *testing.T, path string) {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open the current schema to build a v75 store: %v", err)
	}
	const sessionID = "cccccccc-3333-4333-8333-cccccccccccc"
	SeedSession(t, s, sessionID)
	SeedSessionEntry(t, s, sessionID, 0)
	if err := s.Close(); err != nil {
		t.Fatalf("close the seeded store: %v", err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("reopen the seeded store raw: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close the v75 store: %v", err)
		}
	}()
	if err := sqlitex.ExecuteScript(conn, legacySearchIndexScript+undoImportProfileScript+`
INSERT INTO session_entries_fts(session_entries_fts) VALUES('rebuild');
DROP TABLE pending_recounts;
PRAGMA user_version = 75;`, nil); err != nil {
		t.Fatalf("restore the v75 search index shape: %v", err)
	}
}

// BuildRecountOwedHeadStore writes a real store at the head schema that
// holds one session and still owes the turn count recount: the marker row
// migration 77 writes ('turn_counts' in pending_recounts, see
// internal/store/schema_v77.go) is present, as if the recount that follows
// that migration never committed. Open refuses it as behind, and its upgrade
// plan is the one recount step.
func BuildRecountOwedHeadStore(t *testing.T, path string) {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open the current schema to build a store that owes its recount: %v", err)
	}
	const sessionID = "dddddddd-4444-4444-8444-dddddddddddd"
	SeedSession(t, s, sessionID)
	SeedSessionEntry(t, s, sessionID, 0)
	if err := s.Close(); err != nil {
		t.Fatalf("close the seeded store: %v", err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("reopen the seeded store raw: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close the store that owes its recount: %v", err)
		}
	}()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO pending_recounts(name) VALUES('turn_counts')`, nil); err != nil {
		t.Fatalf("mark the turn count recount as owed: %v", err)
	}
}

// BuildQuickV78Store writes a real version-78 store that holds no session:
// the current schema with what migration v79 adds undone, stamped 78. Its
// upgrade plan is the one quick step, 79, and a real upgrade finishes it.
// Every store below 78 plans step 78, which copies the session tables and is
// long, so this is the quick behind store an entry-point policy test needs.
func BuildQuickV78Store(t *testing.T, path string) {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open the current schema to build a v78 store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close the new store: %v", err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("reopen the new store raw: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close the v78 store: %v", err)
		}
	}()
	if err := sqlitex.ExecuteScript(conn, undoImportProfileScript+`
PRAGMA user_version = 78;`, nil); err != nil {
		t.Fatalf("restore the v78 shape: %v", err)
	}
}
