package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestMigrationV60ManagedGenerationCatalog pins the closed sets and nullable
// count checks the V2 activation catalog depends on: the relationship target
// state CHECK admits exactly the schema-owned set, and both the generation's
// and the session's input submission counts are NULL (unknown) or an integer
// in the schema-owned range.
func TestMigrationV60ManagedGenerationCatalog(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)

	tables := map[string]bool{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'session_%'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { tables[stmt.ColumnText(0)] = true; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"session_projection_generations", "session_relationship_evidence", "session_projection_sections",
		"session_projection_entries", "session_context_segments", "session_projection_content", "session_projection_aliases",
	} {
		if !tables[name] {
			t.Errorf("migration V60 did not create table %s", name)
		}
	}

	sid := "33333333-3333-4333-8333-333333333333"
	seedGenerationSession(t, s, sid)
	digest := strings.Repeat("0", 64)

	insertGeneration := func(count any) error {
		return sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_generations
 (session_id, generation_id, metadata_json, source_evidence_digest, completeness, index_format_version, installed_at_ms, input_submission_count)
 VALUES (?, 'gen-check', '{}', ?, 'complete', 2, 1, ?)`, &sqlitex.ExecOptions{Args: []any{sid, digest, count}})
	}
	for _, accepted := range []any{nil, int64(0), int64(1), int64(9007199254740991)} {
		if err := insertGeneration(accepted); err != nil {
			t.Fatalf("input_submission_count %v was rejected: %v", accepted, err)
		}
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_projection_generations WHERE generation_id='gen-check'`, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, rejected := range []any{int64(-1), int64(9007199254740992), "not-a-number"} {
		if err := insertGeneration(rejected); err == nil {
			t.Errorf("input_submission_count %v was accepted; the nullable count check must reject it", rejected)
		}
	}

	// The sessions mirror carries the same nullable count so ordinary reads
	// agree with the snapshot without a separate metadata upsert.
	updateSessionCount := func(count any) error {
		return sqlitex.ExecuteTransient(conn, `UPDATE sessions SET input_submission_count = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{count, sid}})
	}
	for _, accepted := range []any{nil, int64(0), int64(1), int64(9007199254740991)} {
		if err := updateSessionCount(accepted); err != nil {
			t.Fatalf("sessions input_submission_count %v was rejected: %v", accepted, err)
		}
	}
	for _, rejected := range []any{int64(-1), int64(9007199254740992)} {
		if err := updateSessionCount(rejected); err == nil {
			t.Errorf("sessions input_submission_count %v was accepted; the nullable count check must reject it", rejected)
		}
	}

	validStates := map[schema.RelationshipTargetState]bool{}
	for _, state := range schema.AllRelationshipTargetStates {
		validStates[state] = true
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence (session_id, generation_id, kind, target_state) VALUES (?, 'gen-check', 'started_by', ?)`, &sqlitex.ExecOptions{Args: []any{sid, string(state)}}); err != nil {
			t.Errorf("relationship target state %q was rejected: %v", state, err)
		}
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_relationship_evidence WHERE generation_id='gen-check'`, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{"made_up_state", "TARGET_KNOWN", ""} {
		if validStates[schema.RelationshipTargetState(state)] {
			continue
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence (session_id, generation_id, kind, target_state) VALUES (?, 'gen-check', 'started_by', ?)`, &sqlitex.ExecOptions{Args: []any{sid, state}}); err == nil {
			t.Errorf("relationship target state %q was accepted; the CHECK must admit exactly the schema set", state)
		}
	}
}
