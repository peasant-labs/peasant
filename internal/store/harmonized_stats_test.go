package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// seedStatsSession inserts the host, project, and session rows one stats
// test needs. A non-nil active generation makes the session native.
func seedStatsSession(t *testing.T, s *store.Store, id string, active *string) {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.PoolForTest().Put(conn)
	exec := func(query string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('stats-host','stats-host')`)
	exec(`INSERT OR IGNORE INTO projects(project_hash, canonical_cwd) VALUES('stats-project','/synthetic/stats')`)
	var activeArg any
	if active != nil {
		activeArg = *active
	}
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id)
VALUES(?, 'opencode', 'stats-model', 'stats-host', 'stats-project', 1, 2, 3, '/synthetic/stats.jsonl', 'jsonl', 11, ?)`, id, activeArg)
}

func statsInt(v int) *int          { return &v }
func statsString(v string) *string { return &v }

func readStatsMirror(t *testing.T, s *store.Store, id string) any {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.PoolForTest().Put(conn)
	var mirror any
	if err := sqlitex.ExecuteTransient(conn, `SELECT input_submission_count FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{id}, ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				mirror = stmt.ColumnInt64(0)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return mirror
}

func TestSeedWriteAllowed(t *testing.T) {
	if ok, err := store.SeedWriteAllowed(store.StatsSourceHarness); !ok || err != nil {
		t.Fatalf("harness = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := store.SeedWriteAllowed(store.StatsSourceDerived); ok || err != nil {
		t.Fatalf("derived = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := store.SeedWriteAllowed(store.StatsSource("archived")); ok || err == nil {
		t.Fatalf("unknown origin = (%v, %v), want (false, error)", ok, err)
	}
}
