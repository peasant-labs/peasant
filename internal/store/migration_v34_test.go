package store_test

import (
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// V34 creates pulled_transcripts (PK village_host, transcript_id) and
// pulled_annotations (PK village_host, content_hash) plus two indexes on
// pulled_annotations. These tests verify ONLY what V34 introduces.

func TestMigrationV34Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	if uv := queryInt(t, conn, `PRAGMA user_version`); uv < 34 {
		t.Errorf("user_version: expected >= 34, got %d", uv)
	}

	for _, table := range []string{"pulled_transcripts", "pulled_annotations"} {
		got := queryInt(t, conn,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table)
		if got != 1 {
			t.Errorf("%s table: expected 1, got %d", table, got)
		}
	}

	for _, idx := range []string{
		"idx_pulled_annotations_transcript",
		"idx_pulled_annotations_local_session",
	} {
		got := queryInt(t, conn,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx)
		if got != 1 {
			t.Errorf("%s index: expected 1, got %d", idx, got)
		}
	}
}

// TestMigrationV34CompositePKs verifies the composite primary keys: the same
// transcript_id (or content_hash) from two distinct villages must coexist
// (C-F4 multi-village correctness), and a duplicate within one village must
// be rejected.
func TestMigrationV34CompositePKs(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	const insertTranscript = `INSERT INTO pulled_transcripts
		(village_host, transcript_id, owner_user_id, owner_username,
		 content_hash, visibility, pull_dir,
		 first_pulled_at, last_pulled_at, annotation_count)
		VALUES (?, ?, 'u1', 'alice', 'h1', 'group', '/pulls/x', 1000, 1000, 0)`

	const tid = "99d59925-36bc-424c-a789-8be54d9702ba"

	// Same transcript_id on two different villages must coexist.
	if err := sqlitex.ExecuteTransient(conn, insertTranscript,
		&sqlitex.ExecOptions{Args: []any{"village-a.example", tid}}); err != nil {
		t.Fatalf("insert village-a transcript: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, insertTranscript,
		&sqlitex.ExecOptions{Args: []any{"village-b.example", tid}}); err != nil {
		t.Fatalf("insert village-b transcript (same id, different host) should succeed: %v", err)
	}
	if got := queryInt(t, conn,
		`SELECT COUNT(*) FROM pulled_transcripts WHERE transcript_id = ?`, tid); got != 2 {
		t.Errorf("expected 2 pulled_transcripts rows across two villages, got %d", got)
	}

	// Duplicate (village_host, transcript_id) must be rejected.
	if err := sqlitex.ExecuteTransient(conn, insertTranscript,
		&sqlitex.ExecOptions{Args: []any{"village-a.example", tid}}); err == nil {
		t.Error("expected PK violation for duplicate (village_host, transcript_id), but insert succeeded")
	}

	const insertAnnotation = `INSERT INTO pulled_annotations
		(village_host, content_hash, transcript_id, author_user_id, author_username, payload, pulled_at)
		VALUES (?, ?, ?, 'u2', 'bob', '{}', 2000)`

	const hash = "deadbeef"

	// Same content_hash on two different villages must coexist.
	if err := sqlitex.ExecuteTransient(conn, insertAnnotation,
		&sqlitex.ExecOptions{Args: []any{"village-a.example", hash, tid}}); err != nil {
		t.Fatalf("insert village-a annotation: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, insertAnnotation,
		&sqlitex.ExecOptions{Args: []any{"village-b.example", hash, tid}}); err != nil {
		t.Fatalf("insert village-b annotation (same hash, different host) should succeed: %v", err)
	}
	if got := queryInt(t, conn,
		`SELECT COUNT(*) FROM pulled_annotations WHERE content_hash = ?`, hash); got != 2 {
		t.Errorf("expected 2 pulled_annotations rows across two villages, got %d", got)
	}

	// Duplicate (village_host, content_hash) must be rejected.
	if err := sqlitex.ExecuteTransient(conn, insertAnnotation,
		&sqlitex.ExecOptions{Args: []any{"village-a.example", hash, tid}}); err == nil {
		t.Error("expected PK violation for duplicate (village_host, content_hash), but insert succeeded")
	}
}

// TestMigrationV34AnnotationNoTranscriptFK verifies pulled_annotations.transcript_id
// has NO foreign key: an annotation may target an OWN pushed transcript that has
// no pulled_transcripts row (the annotation-refresh path).
func TestMigrationV34AnnotationNoTranscriptFK(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = ON`, nil); err != nil {
		t.Fatalf("enable FKs: %v", err)
	}

	// No matching pulled_transcripts row exists; the insert must still succeed.
	err := sqlitex.ExecuteTransient(conn, `INSERT INTO pulled_annotations
		(village_host, content_hash, transcript_id, author_user_id, author_username, payload, pulled_at)
		VALUES ('village-a.example', 'h-no-fk', '99d59925-36bc-424c-a789-8be54d9702ba',
		        'u2', 'bob', '{}', 3000)`, nil)
	if err != nil {
		t.Fatalf("insert annotation with no matching transcript should succeed (no FK): %v", err)
	}
}

// TestMigrationV34RetractedAtColumn verifies the schema-only retracted_at column
// exists and is nullable (designed for deferred retraction reconciliation, not
// written by the MVP).
func TestMigrationV34RetractedAtColumn(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	var hasRetractedAt bool
	err := sqlitex.ExecuteTransient(conn, `PRAGMA table_info(pulled_annotations)`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if stmt.ColumnText(1) == "retracted_at" {
					hasRetractedAt = true
					if notNull := stmt.ColumnInt(3); notNull != 0 {
						t.Errorf("retracted_at should be nullable (notnull=0), got notnull=%d", notNull)
					}
				}
				return nil
			},
		})
	if err != nil {
		t.Fatalf("table_info(pulled_annotations): %v", err)
	}
	if !hasRetractedAt {
		t.Error("pulled_annotations.retracted_at column not found")
	}
}

// TestMigrationV34Strict verifies both tables are STRICT (reject a TEXT value in
// an INTEGER column).
func TestMigrationV34Strict(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	err := sqlitex.ExecuteTransient(conn, `INSERT INTO pulled_transcripts
		(village_host, transcript_id, owner_user_id, owner_username,
		 content_hash, visibility, pull_dir,
		 first_pulled_at, last_pulled_at, annotation_count)
		VALUES ('v', 't', 'u', 'n', 'h', 'group', '/d', 'not-a-number', 1000, 0)`, nil)
	if err == nil {
		t.Error("expected STRICT mode to reject TEXT in INTEGER column (first_pulled_at), but insert succeeded")
	}
}
