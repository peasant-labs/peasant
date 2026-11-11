package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const (
	v79FixtureVillageURL = "https://village.example/team"
	v79FixtureUserID     = "11111111-1111-4111-8111-111111111111"
)

// TestMigrationV79AddsLocalOwnerAndClaimLog proves a populated V75 database
// becomes V79 without losing or rewriting any row, that the new singleton and
// append-only constraints hold at the schema boundary, and that pulled rows stay
// byte-identical.
func TestMigrationV79AddsLocalOwnerAndClaimLog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v75.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateFrozenSchema(t, conn, 78); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	seedV79Predecessor(t, conn)

	tables := []struct {
		name string
		key  string
	}{
		{"sessions", "session_id"},
		{"annotations", "id"},
		{"pulled_transcripts", "village_host || ':' || transcript_id"},
		{"pulled_annotations", "village_host || ':' || content_hash"},
	}
	preColumns := make(map[string][]string, len(tables))
	before := make(map[string]map[string]string, len(tables))
	for _, table := range tables {
		preColumns[table.name] = v79Columns(t, conn, table.name)
		before[table.name] = dumpV79Rows(t, conn, table.name, table.key, preColumns[table.name])
	}
	beforeVersion := v79UserVersion(t, conn)
	if beforeVersion != 78 {
		conn.Close()
		t.Fatalf("predecessor user_version = %d, want 78", beforeVersion)
	}

	if err := migrateFrozenSchema(t, conn, CurrentSchemaVersion()); err != nil {
		conn.Close()
		t.Fatalf("migrate populated V75 database to V79: %v", err)
	}
	if got := v79UserVersion(t, conn); got != 79 {
		conn.Close()
		t.Fatalf("migrated user_version = %d, want 79", got)
	}
	for _, table := range tables {
		after := dumpV79Rows(t, conn, table.name, table.key, preColumns[table.name])
		if !reflect.DeepEqual(before[table.name], after) {
			conn.Close()
			t.Fatalf("%s rows changed during the V79 migration:\nbefore %v\nafter  %v", table.name, before[table.name], after)
		}
	}

	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = ON`, nil); err != nil {
		conn.Close()
		t.Fatalf("enable foreign keys for the schema-boundary checks: %v", err)
	}
	assertV79Count(t, conn, `SELECT COUNT(*) FROM local_owner`, 0)
	assertV79Count(t, conn, `SELECT COUNT(*) FROM owner_claims`, 0)
	assertV79Count(t, conn, `SELECT COUNT(*) FROM sessions WHERE owner_claim_id IS NOT NULL`, 0)
	assertV79Count(t, conn, `SELECT COUNT(*) FROM annotations WHERE owner_claim_id IS NOT NULL`, 0)

	// local_owner holds at most one row, enforced in SQL rather than by callers.
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO local_owner(id, singleton, created_at) VALUES('lo_fixture', 1, 1000)`, nil); err != nil {
		conn.Close()
		t.Fatalf("insert the first local owner: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO local_owner(id, singleton, created_at) VALUES('lo_second', 1, 2000)`, nil); err == nil {
		conn.Close()
		t.Fatal("a second local_owner row was accepted")
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO local_owner(id, singleton, created_at) VALUES('lo_bad_singleton', 2, 3000)`, nil); err == nil {
		conn.Close()
		t.Fatal("a local_owner row with singleton != 1 was accepted")
	}

	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO collective_observation_scopes(village_url, user_id, incarnation) VALUES(?, ?, 'v79-fixture-incarnation')`, &sqlitex.ExecOptions{Args: []any{v79FixtureVillageURL, v79FixtureUserID}}); err != nil {
		conn.Close()
		t.Fatalf("seed the claim account scope: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES('oc_fixture', 'lo_fixture', ?, ?, 'fixture-user', 4000, 'manual')`, &sqlitex.ExecOptions{Args: []any{v79FixtureVillageURL, v79FixtureUserID}}); err != nil {
		conn.Close()
		t.Fatalf("insert a valid claim entry: %v", err)
	}

	if err := sqlitex.ExecuteTransient(conn, `UPDATE owner_claims SET username_snapshot = 'rewritten' WHERE id = 'oc_fixture'`, nil); err == nil || !strings.Contains(err.Error(), "append-only") {
		conn.Close()
		t.Fatalf("UPDATE owner_claims error = %v, want an append-only refusal", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM owner_claims WHERE id = 'oc_fixture'`, nil); err == nil || !strings.Contains(err.Error(), "append-only") {
		conn.Close()
		t.Fatalf("DELETE owner_claims error = %v, want an append-only refusal", err)
	}

	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES('oc_missing_owner', 'lo_missing', ?, ?, 'fixture-user', 5000, 'manual')`, &sqlitex.ExecOptions{Args: []any{v79FixtureVillageURL, v79FixtureUserID}}); err == nil {
		conn.Close()
		t.Fatal("owner_claims accepted a local_owner_id that does not exist")
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES('oc_missing_scope', 'lo_fixture', ?, '22222222-2222-4222-8222-222222222222', 'fixture-user', 6000, 'manual')`, &sqlitex.ExecOptions{Args: []any{v79FixtureVillageURL}}); err == nil {
		conn.Close()
		t.Fatal("owner_claims accepted a (village_url, user_id) without a collective observation scope")
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES('oc_bad_source', 'lo_fixture', ?, ?, 'fixture-user', 7000, 'not-a-source')`, &sqlitex.ExecOptions{Args: []any{v79FixtureVillageURL, v79FixtureUserID}}); err == nil {
		conn.Close()
		t.Fatal("owner_claims accepted a source outside the closed set")
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	// The chain is forward-only: a database stamped by a newer build is refused
	// loudly before any migration or write instead of being silently served.
	conn, err = sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version = 80`, nil); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("opening a database from a newer schema version succeeded")
	} else if !strings.Contains(err.Error(), "newer Peasant") {
		t.Fatalf("opening a newer-schema database error = %v, want a forward-only refusal", err)
	}
}

func seedV79Predecessor(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('v79-host', 'v79-host');
INSERT INTO projects(project_hash) VALUES('v79-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('v79-session-a', 'claude-code', 'fixture-model', 'v79-host', 'v79-project', 1, 2, 3, '/synthetic/v79-a.jsonl', 'jsonl');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('v79-session-b', 'claude-code', 'fixture-model', 'v79-host', 'v79-project', 4, 5, 6, '/synthetic/v79-b.jsonl', 'jsonl');
INSERT INTO annotations(id, target_kind_id, annotation_type_id, annotator_id, value, created_at)
VALUES('v79-annotation-a', (SELECT id FROM target_kinds WHERE name = 'session'), (SELECT id FROM annotation_types LIMIT 1), (SELECT id FROM annotators WHERE name = 'human-web'), 'keep-me', 7);
INSERT INTO annotation_target_sessions(annotation_id, session_id) VALUES('v79-annotation-a', 'v79-session-a');
INSERT INTO annotations(id, target_kind_id, annotation_type_id, annotator_id, value, created_at)
VALUES('v79-annotation-b', (SELECT id FROM target_kinds WHERE name = 'session'), (SELECT id FROM annotation_types LIMIT 1), (SELECT id FROM annotators WHERE name = 'human-web'), 'keep-me-too', 8);
INSERT INTO annotation_target_sessions(annotation_id, session_id) VALUES('v79-annotation-b', 'v79-session-b');
INSERT INTO pulled_transcripts(village_host, transcript_id, owner_user_id, owner_username, local_session_id, title, harness, project_name, content_hash, visibility, pull_dir, first_pulled_at, last_pulled_at, annotation_count)
VALUES('village.example', 'pulled-a', 'remote-user', 'remote-user', 'v79-session-a', 'Remote A', 'claude-code', 'remote-project', 'hash-a', 'private', '/pulls/a', 10, 11, 1);
INSERT INTO pulled_transcripts(village_host, transcript_id, owner_user_id, owner_username, content_hash, visibility, pull_dir, first_pulled_at, last_pulled_at)
VALUES('village.example', 'pulled-b', '', '', 'hash-b', 'public', '/pulls/b', 12, 13);
INSERT INTO pulled_annotations(village_host, content_hash, transcript_id, local_session_id, author_user_id, author_username, payload, pulled_at)
VALUES('village.example', 'ann-hash-a', 'pulled-a', 'v79-session-a', 'remote-author', 'remote-author', '{"value":"foreign"}', 14);
`, nil); err != nil {
		t.Fatal(err)
	}
}

func v79UserVersion(t *testing.T, conn *sqlite.Conn) int {
	t.Helper()
	var version int
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { version = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	return version
}

func assertV79Count(t *testing.T, conn *sqlite.Conn, sql string, want int64) {
	t.Helper()
	var got int64
	if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { got = stmt.ColumnInt64(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", sql, got, want)
	}
}

// dumpV79Rows encodes every column by name and SQLite literal representation,
// including NULLs and blobs, so a changed byte or a missing row cannot hide in a
// sampled column.
func v79Columns(t *testing.T, conn *sqlite.Conn, table string) []string {
	t.Helper()
	var columns []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, &sqlitex.ExecOptions{
		Args: []any{table},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			columns = append(columns, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(columns) == 0 {
		t.Fatalf("table %q has no columns", table)
	}
	return columns
}

// dumpV79Rows encodes every predecessor column by name and SQLite literal
// representation, including NULLs and blobs, so a changed byte or a missing row
// cannot hide in a sampled column. Columns added by V79 are intentionally
// omitted: the caller passes the predecessor column list for both reads.
func dumpV79Rows(t *testing.T, conn *sqlite.Conn, table, key string, columnNames []string) map[string]string {
	t.Helper()
	columns := make([]string, 0, len(columnNames))
	for _, name := range columnNames {
		columns = append(columns, `hex(quote("`+strings.ReplaceAll(name, `"`, `""`)+`"))`)
	}
	out := map[string]string{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT `+key+`, `+strings.Join(columns, ` || ':' || `)+` FROM `+table+` ORDER BY `+key, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			out[stmt.ColumnText(0)] = stmt.ColumnText(1)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
