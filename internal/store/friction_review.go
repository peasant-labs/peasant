package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// FrictionReviewSession is one row of the local review queue: a session with
// at least one agent friction episode, its conversation turn count, and the
// agent/human episode totals the reader marks as pending or done.
type FrictionReviewSession struct {
	SessionID     string
	TurnCount     int
	Project       string
	AgentEpisodes int
	HumanEpisodes int
}

// ListFrictionReviewSessions returns the sessions the local review queue shows,
// with the one shared turn count definition spliced in. It is the store-owned
// read behind the REST handler so the queue cannot run a second count query.
func (s *Store) ListFrictionReviewSessions(ctx context.Context) ([]FrictionReviewSession, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for the friction review sessions: %w", err)
	}
	defer s.pool.Put(conn)

	rows := make([]FrictionReviewSession, 0)
	if err := sqlitex.ExecuteTransient(conn, sqlFrictionReviewSessions, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, FrictionReviewSession{
				SessionID:     stmt.ColumnText(0),
				TurnCount:     stmt.ColumnInt(1),
				Project:       stmt.ColumnText(2),
				AgentEpisodes: stmt.ColumnInt(3),
				HumanEpisodes: stmt.ColumnInt(4),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list the friction review sessions: %w; the queue was not read", err)
	}
	return rows, nil
}

// sqlFrictionReviewSessions groups the agent friction annotations by session
// and reads the conversation turn count through the shared expression, so the
// review queue reports the same count as the session list and the transcript.
const sqlFrictionReviewSessions = `SELECT
	COALESCE(ats.session_id, ate.session_id) as sid,
	COALESCE(` + sqlSessionConversationTurnCount + `, 0),
	COALESCE(p.canonical_cwd, p.project_hash, ''),
	SUM(CASE WHEN ak.name = 'agent' THEN 1 ELSE 0 END) as agent_count,
	SUM(CASE WHEN ak.name = 'human' THEN 1 ELSE 0 END) as human_count
FROM annotations a
JOIN annotation_types t ON t.id = a.annotation_type_id
JOIN annotators ann ON ann.id = a.annotator_id
JOIN annotator_kinds ak ON ak.id = ann.kind_id
LEFT JOIN annotation_target_sessions ats ON ats.annotation_id = a.id
LEFT JOIN annotation_target_entries ate ON ate.annotation_id = a.id
LEFT JOIN sessions s ON s.session_id = COALESCE(ats.session_id, ate.session_id)
LEFT JOIN session_metrics m ON m.session_id = s.session_id
LEFT JOIN session_captured_stats c ON c.session_id = s.session_id
LEFT JOIN projects p ON s.project_hash = p.project_hash
WHERE t.type_id = 'research.friction_episode'
AND a.superseded_by IS NULL
GROUP BY sid
HAVING agent_count > 0
ORDER BY sid`
