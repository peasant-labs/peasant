package storetest

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// SeedLegacySearchIndex reconstructs the pre-consolidation dual-index state
// on a store V76 already consolidated at open: the union entries triggers
// give way to the retired session_entries_fts index and its three sync
// triggers, so transition-window reads and migrateConsolidateSearch have the
// retired side to work with. The trigger text mirrors the retired triggers
// (store's shared migrateSearchTrigger definitions with only the table name
// swapped, which is exactly how they read); this package cannot reference
// those unexported definitions, so the SQL is spelled out here and kept in
// step with them by the transition-window tests that assert both indexes.
// Production can no longer reach this state (Open applies V76 first); only
// tests of the transition window rebuild it.
func SeedLegacySearchIndex(t *testing.T, s *store.Store) {
	t.Helper()
	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Put(conn)
	const script = `DROP TRIGGER IF EXISTS session_entries_ai;
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
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("seed the retired search index: %v", err)
	}
}
