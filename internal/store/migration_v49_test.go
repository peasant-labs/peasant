package store_test

import (
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestMigrationV49AnnotationTargetAnchorsTable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	if got := queryInt(t, conn, `PRAGMA user_version`); got < 49 {
		t.Fatalf("user_version: expected >= 49, got %d", got)
	}
	var tableExists int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='annotation_target_anchors'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			tableExists = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("query annotation_target_anchors table: %v", err)
	}
	if tableExists != 1 {
		t.Fatalf("annotation_target_anchors table missing")
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotation_target_anchors (annotation_id, session_id, state, updated_at) VALUES ('missing', 'missing', 'guessed', 1)`, nil); err == nil {
		t.Fatal("annotation_target_anchors accepted unknown state; want CHECK failure")
	}
}
