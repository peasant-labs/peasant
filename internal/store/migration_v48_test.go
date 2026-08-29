package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestMigrationV48AnnotationRunStateTable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 48 {
		t.Errorf("user_version: expected >= 48, got %d", uv)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='annotation_run_state'`); got != 1 {
		t.Fatalf("annotation_run_state table count = %d, want 1", got)
	}
}

func TestStore_AnnotationRunStateRoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := ingest.SessionID("99999999-4444-4444-8444-999999999999")
	seedSession(t, s, string(sid))
	hash := strings.Repeat("a", 64)
	annotatedAt := time.UnixMilli(1700000000123)

	if err := s.SaveAnnotationRunState(ctx, ingest.AnnotationRunState{SessionID: sid, SessionEntriesHash: hash, ComputeVersion: 12, ClassifierVersion: 3, AnnotatedAt: annotatedAt}); err != nil {
		t.Fatalf("SaveAnnotationRunState: %v", err)
	}
	state, err := s.GetAnnotationRunState(ctx, sid)
	if err != nil {
		t.Fatalf("GetAnnotationRunState: %v", err)
	}
	if state == nil {
		t.Fatal("GetAnnotationRunState returned nil")
	}
	if state.SessionID != sid || state.SessionEntriesHash != hash || state.ComputeVersion != 12 || state.ClassifierVersion != 3 || !state.AnnotatedAt.Equal(annotatedAt) {
		t.Fatalf("state round trip mismatch: %+v", state)
	}
}

func TestStore_AnnotationRunStateCascadesWithSessionDelete(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := ingest.SessionID("99999999-5555-4555-8555-999999999999")
	seedSession(t, s, string(sid))
	if err := s.SaveAnnotationRunState(ctx, ingest.AnnotationRunState{SessionID: sid, SessionEntriesHash: strings.Repeat("b", 64), ComputeVersion: 1, ClassifierVersion: 1, AnnotatedAt: time.UnixMilli(1700000000000)}); err != nil {
		t.Fatalf("SaveAnnotationRunState: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_metrics WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
		t.Fatalf("delete session_metrics: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_run_state WHERE session_id = ?`, string(sid)); got != 0 {
		t.Fatalf("annotation_run_state rows after session delete = %d, want 0", got)
	}
}

func TestMigrationV48AnnotationRunStateRejectsInvalidHash(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sid := "99999999-6666-4666-8666-999999999999"
	seedSession(t, s, sid)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	for _, invalid := range []string{strings.Repeat("c", 63), strings.Repeat("A", 64)} {
		err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotation_run_state (session_id, session_entries_hash, compute_version, classifier_version, annotated_at) VALUES (?, ?, 1, 1, 1700000000000)`, &sqlitex.ExecOptions{Args: []any{sid, invalid}})
		if err == nil {
			t.Fatalf("invalid annotation_run_state hash %q was accepted", invalid)
		}
	}
}

func TestStore_GetCurrentSessionEntriesHash(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := ingest.SessionID("99999999-7777-4777-8777-999999999999")
	seedSession(t, s, string(sid))
	hash := strings.Repeat("d", 64)

	conn := takeConn(t, s.PoolForTest())
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_entries_hash = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{hash, string(sid)}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("update session_entries_hash: %v", err)
	}
	s.PoolForTest().Put(conn)

	got, ok, err := s.GetCurrentSessionEntriesHash(ctx, sid)
	if err != nil {
		t.Fatalf("GetCurrentSessionEntriesHash: %v", err)
	}
	if !ok || got != hash {
		t.Fatalf("GetCurrentSessionEntriesHash = %q, %v; want %q, true", got, ok, hash)
	}
}
