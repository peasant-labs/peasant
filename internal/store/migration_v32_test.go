package store_test

import (
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// V32 migration creates the lesson_sources provenance table and two indexes.

func TestMigrationV32Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 32 {
		t.Errorf("user_version: expected >= 32, got %d", uv)
	}

	// The lesson_sources table must exist.
	tableCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lesson_sources'`)
	if tableCount != 1 {
		t.Errorf("lesson_sources table: expected 1, got %d", tableCount)
	}

	// Both indexes must exist.
	lessonIdx := queryInt(t, conn,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_lesson_sources_lesson'`)
	if lessonIdx != 1 {
		t.Errorf("idx_lesson_sources_lesson: expected 1, got %d", lessonIdx)
	}

	sessionIdx := queryInt(t, conn,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_lesson_sources_session'`)
	if sessionIdx != 1 {
		t.Errorf("idx_lesson_sources_session: expected 1, got %d", sessionIdx)
	}
}

func TestMigrationV32LessonSourcesFK(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Enable FK enforcement for this connection.
	err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = ON`, nil)
	if err != nil {
		t.Fatalf("enable FKs: %v", err)
	}

	// Insert with a non-existent lesson_id must fail due to FK constraint.
	err = sqlitex.ExecuteTransient(conn, `
		INSERT INTO lesson_sources (id, lesson_id, episode_annotation_id, session_id, created_at)
		VALUES ('src-1', 'nonexistent-lesson-id', 'ann-1', 'sess-1', 1000)
	`, nil)
	if err == nil {
		t.Error("expected FK violation for non-existent lesson_id, but insert succeeded")
	}
}

func TestMigrationV32LessonSourcesInsert(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Insert a lesson first (required by the FK constraint).
	err := sqlitex.ExecuteTransient(conn, `
		INSERT INTO lessons (id, episode_annotation_id, session_id, topic, rule, failure_mode, created_at)
		VALUES ('lesson-1', 'ann-1', 'sess-1', 'testing/topic', 'A rule', 'A failure mode', 1000)
	`, nil)
	if err != nil {
		t.Fatalf("insert lesson: %v", err)
	}

	// Insert a lesson_sources row referencing the lesson.
	err = sqlitex.ExecuteTransient(conn, `
		INSERT INTO lesson_sources (id, lesson_id, episode_annotation_id, session_id, created_at)
		VALUES ('src-1', 'lesson-1', 'ann-1', 'sess-1', 1000)
	`, nil)
	if err != nil {
		t.Fatalf("insert lesson_source: %v", err)
	}

	// Verify the row round-trips.
	count := queryInt(t, conn, `SELECT COUNT(*) FROM lesson_sources WHERE lesson_id = 'lesson-1'`)
	if count != 1 {
		t.Errorf("expected 1 lesson_sources row, got %d", count)
	}

	// Verify all columns can be read back.
	var gotID, gotLessonID, gotAnnotationID, gotSessionID string
	var gotCreatedAt int64
	err = sqlitex.ExecuteTransient(conn, `
		SELECT id, lesson_id, episode_annotation_id, session_id, created_at
		FROM lesson_sources WHERE id = 'src-1'
	`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			gotID = stmt.ColumnText(0)
			gotLessonID = stmt.ColumnText(1)
			gotAnnotationID = stmt.ColumnText(2)
			gotSessionID = stmt.ColumnText(3)
			gotCreatedAt = stmt.ColumnInt64(4)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("select lesson_source: %v", err)
	}
	if gotID != "src-1" {
		t.Errorf("id: got %q, want %q", gotID, "src-1")
	}
	if gotLessonID != "lesson-1" {
		t.Errorf("lesson_id: got %q, want %q", gotLessonID, "lesson-1")
	}
	if gotAnnotationID != "ann-1" {
		t.Errorf("episode_annotation_id: got %q, want %q", gotAnnotationID, "ann-1")
	}
	if gotSessionID != "sess-1" {
		t.Errorf("session_id: got %q, want %q", gotSessionID, "sess-1")
	}
	if gotCreatedAt != 1000 {
		t.Errorf("created_at: got %d, want %d", gotCreatedAt, 1000)
	}
}

func TestMigrationV32LessonSourcesUnique(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Insert a lesson first.
	err := sqlitex.ExecuteTransient(conn, `
		INSERT INTO lessons (id, episode_annotation_id, session_id, topic, rule, failure_mode, created_at)
		VALUES ('lesson-uniq', 'ann-uniq', 'sess-uniq', 'testing/unique', 'Unique rule', 'Unique fail', 1000)
	`, nil)
	if err != nil {
		t.Fatalf("insert lesson: %v", err)
	}

	// First provenance insert succeeds.
	err = sqlitex.ExecuteTransient(conn, `
		INSERT INTO lesson_sources (id, lesson_id, episode_annotation_id, session_id, created_at)
		VALUES ('src-uniq-1', 'lesson-uniq', 'ann-uniq', 'sess-uniq', 1000)
	`, nil)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Duplicate (same lesson_id, annotation_id, session_id) must be rejected.
	err = sqlitex.ExecuteTransient(conn, `
		INSERT INTO lesson_sources (id, lesson_id, episode_annotation_id, session_id, created_at)
		VALUES ('src-uniq-2', 'lesson-uniq', 'ann-uniq', 'sess-uniq', 2000)
	`, nil)
	if err == nil {
		t.Error("expected UNIQUE constraint violation for duplicate (lesson_id, annotation_id, session_id), but insert succeeded")
	}

	// Different annotation_id should succeed (different episode contributed).
	err = sqlitex.ExecuteTransient(conn, `
		INSERT INTO lesson_sources (id, lesson_id, episode_annotation_id, session_id, created_at)
		VALUES ('src-uniq-3', 'lesson-uniq', 'ann-other', 'sess-uniq', 3000)
	`, nil)
	if err != nil {
		t.Fatalf("different annotation insert should succeed: %v", err)
	}

	// Verify 2 rows total (first + different annotation).
	count := queryInt(t, conn, `SELECT COUNT(*) FROM lesson_sources WHERE lesson_id = 'lesson-uniq'`)
	if count != 2 {
		t.Errorf("expected 2 lesson_sources rows, got %d", count)
	}
}
