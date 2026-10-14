package codemap

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestSessionListParentAnswersThroughTheRule proves the Map/Review project
// listing's parent column answers the logical-parent rule, not the raw
// availability cache. The store holds a true root (no evidence, NULL cache) and
// an evidenced child whose cache is NULL but whose generation names the root.
// The shipped fragment must answer the child's parent as the root, which the
// raw `parent_id` predicate cannot: it would see the NULL cache and report the
// child as a root too.
func TestSessionListParentAnswersThroughTheRule(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()

	const rootID = "a1a1a1a1-0000-4000-8000-000000000001"
	const childID = "a1a1a1a1-0000-4000-8000-000000000002"
	const generationID = "gen-session-parent-rule"
	projectHash := schema.ProjectHash(testutil.TestProjectHash)

	seed := func(sessionID string, startMs int64) {
		t.Helper()
		ingested := startMs + 1
		meta := &schema.UnifiedMetadata{
			SessionID:    schema.SessionID(sessionID),
			ModelHarness: ingest.HarnessClaudeCode,
			Model:        testutil.TestModel,
			HostSlug:     schema.HostSlug(testutil.TestHostSlug),
			Project:      schema.ProjectContext{Hash: projectHash, Name: "repo", FilePath: "/repo"},
			Timestamp:    schema.TimestampInfo{Start: startMs, End: startMs + 100, Ingested: &ingested},
			Source:       schema.SourceInfo{FilePath: "/session-parent-rule.jsonl", Format: schema.SourceFormatJSONL},
		}
		if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
			t.Fatalf("seed session %s for the project listing: %v", sessionID, err)
		}
	}
	seed(rootID, testutil.TestSessionStartTime.UnixMilli())
	seed(childID, testutil.TestSessionStartTime.UnixMilli()+1000)

	conn, err := database.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take a connection to arrange the evidenced child: %v", err)
	}
	defer database.Pool().Put(conn)
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("arrange the evidenced child: %v", err)
		}
	}
	// The child's generation carries the started_by evidence that names the
	// root; the availability cache is deliberately left NULL.
	exec(`UPDATE sessions SET active_generation_id = ? WHERE session_id = ?`, generationID, childID)
	exec(`INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
VALUES (?, ?, 0, 'started_by', 'target_known', ?, 'native_typed', '', '', '')`, childID, generationID, rootID)

	// The raw cache stays NULL, which is exactly why the raw predicate would
	// mis-answer the child as a root.
	var rawCache *string
	if err := sqlitex.ExecuteTransient(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{childID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				value := stmt.ColumnText(0)
				rawCache = &value
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read the child's raw availability cache: %v", err)
	}
	if rawCache != nil {
		t.Fatalf("the child's availability cache is %q, want NULL; the arrangement must leave the cache empty so the test proves the rule answers, not the cache", *rawCache)
	}

	parents := map[string]string{}
	if err := sqlitex.ExecuteTransient(conn, sqlSessionsForProject, &sqlitex.ExecOptions{
		Args: []any{projectHash.String()},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			parents[stmt.ColumnText(0)] = stmt.ColumnText(8)
			return nil
		},
	}); err != nil {
		t.Fatalf("run the shipped project-listing fragment: %v", err)
	}
	if got := parents[childID]; got != rootID {
		t.Fatalf("the project listing answers the evidenced child's parent as %q, want %q: the parent column must render the logical-parent rule, so a NULL availability cache cannot hide a known parent", got, rootID)
	}
	if got := parents[rootID]; got != "" {
		t.Fatalf("the project listing answers the true root's parent as %q, want empty: a session with no evidence and a NULL cache is a root", got)
	}
}
