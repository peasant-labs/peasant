package store

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestMigrationV61AddsReverseLookupIndexes proves the reverse logical-target
// lookup indexes are additive and correctly shaped on the build's SQLite.
//
// A V60 database is migrated to V61 after seeding the two evidence stores the
// reconciliation reads. The migration must create the parentUuid expression
// index (partial on a retained value) and the relationship-evidence composite
// target index (kind first, then the target, then the child and state; not
// partial), leave every stored row untouched, and run on a SQLite that supports
// expression indexes (the migration itself is the functional check; the version
// assertion records the floor).
func TestMigrationV61AddsReverseLookupIndexes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "reverse-lookup-index.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	predecessor := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:60],
		MigrationOptions: dbSchema.MigrationOptions[:60],
	}
	if err := sqlitemigration.Migrate(ctx, conn, predecessor); err != nil {
		t.Fatalf("migrate to V60: %v", err)
	}

	requireSQLiteExpressionIndexSupport(t, conn)

	// Seed one legacy snapshot with a retained parentUuid and one active
	// generation relationship row so the migration runs over populated tables.
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('v61-host','v61-host');
INSERT INTO projects(project_hash, canonical_cwd) VALUES('v61-project','/synthetic/v61');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id)
VALUES('v61-child','codex','v61-model','v61-host','v61-project',1,2,3,'/synthetic/v61.jsonl','jsonl',11,'v61-gen');
INSERT INTO session_publication_metadata(session_id, capture_revision, schema_version, metadata_json, metadata_hash, content_hash, captured_at)
VALUES('v61-child',1,11,json('{"parentUuid":"v61-parent"}'),'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',5);
INSERT INTO session_relationship_evidence(session_id, generation_id, kind, target_state, target_local_id)
VALUES('v61-child','v61-gen','started_by','target_known','v61-parent');`, nil); err != nil {
		t.Fatalf("seed predecessor rows: %v", err)
	}

	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatalf("migrate to V61: %v", err)
	}

	legacySQL := indexDefinition(t, conn, "idx_session_publication_parent_uuid")
	if !strings.Contains(legacySQL, "json_extract") || !strings.Contains(legacySQL, "parentUuid") {
		t.Fatalf("legacy target index is not a parentUuid expression index: %s", legacySQL)
	}
	durableSQL := indexDefinition(t, conn, "idx_relationship_evidence_started_by_target")
	for _, want := range []string{"session_relationship_evidence", "kind", "target_local_id", "session_id", "target_state"} {
		if !strings.Contains(durableSQL, want) {
			t.Fatalf("durable target index is missing %q in its definition: %s", want, durableSQL)
		}
	}
	if partial := indexPartialFlag(t, conn, "session_publication_metadata", "idx_session_publication_parent_uuid"); partial != 1 {
		t.Errorf("legacy parentUuid index partial flag = %d, want 1", partial)
	}
	if partial := indexPartialFlag(t, conn, "session_relationship_evidence", "idx_relationship_evidence_started_by_target"); partial != 0 {
		t.Errorf("durable target index partial flag = %d, want 0 (a stable kind equality leads it)", partial)
	}
	if strings.Contains(durableSQL, "WHERE") {
		t.Errorf("durable target index must not be partial: %s", durableSQL)
	}

	// The migration is additive: the seeded evidence is unchanged.
	var parentUUID, targetLocalID string
	if err := sqlitex.ExecuteTransient(conn, `SELECT json_extract(metadata_json,'$.parentUuid') FROM session_publication_metadata WHERE session_id='v61-child'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { parentUUID = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `SELECT target_local_id FROM session_relationship_evidence WHERE session_id='v61-child'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { targetLocalID = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if parentUUID != "v61-parent" || targetLocalID != "v61-parent" {
		t.Fatalf("migration changed retained evidence: parentUuid=%q targetLocalID=%q", parentUUID, targetLocalID)
	}
	var userVersion int
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { userVersion = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if userVersion != CurrentSchemaVersion() {
		t.Fatalf("user_version after migration = %d, want the build schema %d", userVersion, CurrentSchemaVersion())
	}
}

// requireSQLiteExpressionIndexSupport records the SQLite floor for expression
// indexes and fails when the build cannot create them.
func requireSQLiteExpressionIndexSupport(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	var version string
	if err := sqlitex.ExecuteTransient(conn, "SELECT sqlite_version()", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { version = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		t.Fatalf("unexpected sqlite_version() %q", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatalf("parse sqlite_version() major from %q: %v", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("parse sqlite_version() minor from %q: %v", version, err)
	}
	if major < 3 || (major == 3 && minor < 9) {
		t.Fatalf("SQLite %s predates expression indexes (3.9.0); the reverse lookup requires an expression index", version)
	}
}

func indexDefinition(t *testing.T, conn *sqlite.Conn, name string) string {
	t.Helper()
	var sql string
	if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, &sqlitex.ExecOptions{
		Args:       []any{name},
		ResultFunc: func(stmt *sqlite.Stmt) error { sql = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if sql == "" {
		t.Fatalf("index %s is absent from the final schema", name)
	}
	return sql
}

func indexPartialFlag(t *testing.T, conn *sqlite.Conn, table, name string) int {
	t.Helper()
	partial := -1
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA index_list('`+table+`')`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			// index_list columns: seq, name, unique, origin, partial.
			if stmt.ColumnText(1) == name {
				partial = stmt.ColumnInt(4)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if partial < 0 {
		t.Fatalf("index %s not reported by PRAGMA index_list on its own name", name)
	}
	return partial
}
