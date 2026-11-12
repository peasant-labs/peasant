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

// BuildCleanV75Store writes a real, clean version-75 store at path: it opens
// the current schema (so every table, view, and trigger is the shipped
// shape), seeds one session entry through the live triggers, then undoes
// exactly what migration v76 changes - it re-creates the retired
// session_entries_fts index with the v75 definition, points the three
// session_entries triggers back at it, rebuilds it from session_entries so
// both search indexes agree, and stamps user_version 75. Migration v76 only
// re-points those triggers and drops that index, so the result is the v75
// schema with no residue: opening it runs v76 on the targeted-insert path.
// It also drops pending_recounts, the one table a later migration (v77)
// creates, so the chain from 75 creates it again instead of colliding.
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
	if err := sqlitex.ExecuteScript(conn, legacySearchIndexScript+`
INSERT INTO session_entries_fts(session_entries_fts) VALUES('rebuild');
DROP TABLE pending_recounts;
PRAGMA user_version = 75;`, nil); err != nil {
		t.Fatalf("restore the v75 search index shape: %v", err)
	}
}
