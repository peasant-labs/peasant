package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestMigrateSessionConvertsClean proves the basic Phase 2 conversion: a
// file-backed native session converts, and afterwards its active
// generation is harmonized with no old rows, no mirror rows, no native
// full-content rows, no owned generation files, and a clear flag.
func TestMigrateSessionConvertsClean(t *testing.T) {
	sid, err := schema.NewSessionID("c1c1c1c1-c1c1-41c1-81c1-c1c1c1c1c1c1")
	if err != nil {
		t.Fatal(err)
	}
	s, root := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	v2, blobs := buildTestGeneration(t, sid, "gen_migrate_clean", "migrate text", "migrate input", "migrate output")
	stampSeedMetadataHash(&v2)
	seedFileBackedGeneration(t, s, root, sid, v2, blobs, true)

	outcome, err := s.MigrateSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("MigrateSession: %v", err)
	}
	if outcome != MigrateOutcomeConverted {
		t.Fatalf("MigrateSession outcome = %q, want converted", outcome)
	}
	assertSessionConverted(t, s, root, sid, "gen_migrate_clean")
}

// stampSeedMetadataHash stamps the seed candidate's metadata hash the way
// the production pipeline stamps it: stats-inclusive, after every
// content-bearing field is set. Without it the metadata dimension
// compares against an empty anchor and refuses.
func stampSeedMetadataHash(v2 *indexformat.V2) {
	v2.Generation.Metadata.MetadataHash = schema.ComputeMetadataHash(&v2.Generation.Metadata)
}

// assertSessionConverted proves the converted-session shape (design §7.2):
// the active generation row is harmonized; no projection, mirror, or
// native full-content rows remain; the owned generation files are gone;
// and the sweep flag is clear.
func assertSessionConverted(t *testing.T, s *Store, root string, sid schema.SessionID, generationID string) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	active, harmonized, err := harmonizedActiveOnConn(conn, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !harmonized || active != generationID {
		t.Fatalf("active generation = %q harmonized=%v, want %q harmonized", active, harmonized, generationID)
	}
	for _, table := range []string{"session_projection_generations", "session_projection_entries", "session_projection_content"} {
		var count int64
		if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM `+table+` WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sid)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				count = stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s still holds %d row(s) for converted session %s", table, count, sid)
		}
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM session_entries WHERE session_id = '` + string(sid) + `'`,
		`SELECT COUNT(*) FROM session_entry_full_content WHERE session_id = '` + string(sid) + `'`,
		`SELECT content_sweep_pending FROM sessions WHERE session_id = '` + string(sid) + `'`,
	} {
		var count int64
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				count = stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("converted session %s leaves residue: %s = %d", sid, query, count)
		}
	}
	generations, err := os.ReadDir(filepath.Join(root, string(sid), "generations"))
	if err == nil && len(generations) != 0 {
		t.Fatalf("converted session %s keeps %d owned generation director(ies)", sid, len(generations))
	}
}
