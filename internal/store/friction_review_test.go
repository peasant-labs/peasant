package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestListFrictionReviewSessionsReportsConversationTurns proves the review
// queue reads the same conversation count as every other surface. It also pins
// the agent/human episode split that decides pending versus done.
func TestListFrictionReviewSessionsReportsConversationTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.OpenWith(t, store.WithPoolSize(1))
	const sessionID = "11111111-1111-4111-8111-111111111111"
	storetest.SeedSession(t, s, sessionID)

	turns := 3
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID:             schema.SessionID(sessionID),
		ConversationTurnCount: &turns,
		Source:                store.StatsSourceHarness,
		UpdatedAtMs:           1,
	}); err != nil {
		t.Fatal(err)
	}

	model, provider := "fixture-model", "fixture-provider"
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO models(model_id, provider_key, display_name, last_synced) VALUES(?, ?, 'Fixture Model', '2026-01-01')`,
		&sqlitex.ExecOptions{Args: []any{model, provider}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatal(err)
	}
	s.PoolForTest().Put(conn)
	agentID, err := s.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorAgent,
		Name:        "friction-review-agent",
		DisplayName: "Friction Review Agent",
		ModelID:     &model,
		ProviderKey: &provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	humanID, err := s.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorHuman,
		Name:        "friction-review-human",
		DisplayName: "Friction Review Human",
	})
	if err != nil {
		t.Fatal(err)
	}

	conn, err = s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var typeID string
	if err := sqlitex.ExecuteTransient(conn, `SELECT id FROM annotation_types WHERE type_id = 'research.friction_episode'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			typeID = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if typeID == "" {
		t.Fatal("friction episode annotation type is not seeded")
	}
	insert := func(id, annotatorID, value string) {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotations (
	id, target_kind_id, annotator_id, annotation_type_id, value,
	confidence, reason, provenance, is_primary, created_at
) VALUES (?, (SELECT id FROM target_kinds WHERE name = 'session'), ?, ?, ?, NULL, NULL, NULL, 0, 1)`,
			&sqlitex.ExecOptions{Args: []any{id, annotatorID, typeID, value}}); err != nil {
			t.Fatal(err)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`,
			&sqlitex.ExecOptions{Args: []any{id, sessionID}}); err != nil {
			t.Fatal(err)
		}
	}
	insert("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", agentID, "bad_handoff")
	insert("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", agentID, "bad_output")
	insert("cccccccc-cccc-4ccc-8ccc-cccccccccccc", humanID, "bad_process")
	s.PoolForTest().Put(conn)

	rows, err := s.ListFrictionReviewSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("review rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.SessionID != sessionID || row.TurnCount != turns {
		t.Fatalf("review row = %+v, want session %s with %d conversation turns", row, sessionID, turns)
	}
	if row.AgentEpisodes != 2 || row.HumanEpisodes != 1 {
		t.Fatalf("episode split = %d agent/%d human, want 2/1", row.AgentEpisodes, row.HumanEpisodes)
	}
}
