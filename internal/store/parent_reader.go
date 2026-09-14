package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Logical-parent readers for independently admitted children.
//
// sessions.parent_id is a nullable FK availability cache. A nil cache never
// proves a root session and never hides a known relationship: the durable
// logical evidence lives in session_relationship_evidence for V2 generations
// and in the managed metadata ParentUUID for V1 pairs. Readers that answer
// "who is the parent" consult the logical evidence; readers that answer
// "which cache row can join" use the cache.

// ParentCacheForSession returns the FK availability cache for one session.
// A nil return means the cache is NULL: the parent is missing, unselected,
// unavailable, cyclic, failed, or explicitly none. It never proves a root.
func (s *Store) ParentCacheForSession(ctx context.Context, sessionID ingest.SessionID) (*string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for parent cache of session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	var cache *string
	if err := sqlitex.ExecuteTransient(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				v := stmt.ColumnText(0)
				cache = &v
			}
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read parent cache for session %s: %w", sessionID, err)
	}
	return cache, nil
}

// RelationshipEvidenceForSession returns the durable relationship rows for the
// active V2 generation of one session. V1 pairs carry their logical parent in
// the managed metadata ParentUUID instead; they have no rows here.
func (s *Store) RelationshipEvidenceForSession(ctx context.Context, sessionID ingest.SessionID) ([]RelationshipEvidenceRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for relationship evidence of session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	return relationshipEvidenceForSessionOnConn(conn, sessionID)
}

// RelationshipEvidenceRow is one durable relationship evidence row.
type RelationshipEvidenceRow struct {
	Kind          string
	TargetState   string
	TargetLocalID *string
}

// relationshipEvidenceForSessionOnConn reads the active generation's evidence rows.
func relationshipEvidenceForSessionOnConn(conn *sqlite.Conn, sessionID ingest.SessionID) ([]RelationshipEvidenceRow, error) {
	var rows []RelationshipEvidenceRow
	if err := sqlitex.ExecuteTransient(conn, `SELECT r.kind, r.target_state, r.target_local_id FROM session_relationship_evidence r JOIN sessions s ON s.session_id = r.session_id AND s.active_generation_id = r.generation_id WHERE r.session_id = ? ORDER BY r.kind`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row := RelationshipEvidenceRow{Kind: stmt.ColumnText(0), TargetState: stmt.ColumnText(1)}
			if stmt.ColumnType(2) != sqlite.TypeNull && stmt.ColumnText(2) != "" {
				v := stmt.ColumnText(2)
				row.TargetLocalID = &v
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read relationship evidence for session %s: %w", sessionID, err)
	}
	return rows, nil
}

// LogicalParentForSession returns the durable logical parent for one session:
// the active generation's known or retained started_by target when present,
// otherwise the parent cache. A nil return means no known logical parent; it
// never proves a root when relationship evidence names an unavailable target.
// V1 pairs without V2 evidence fall back to the cache; their full logical
// ParentUUID remains in the managed metadata file.
func (s *Store) LogicalParentForSession(ctx context.Context, sessionID ingest.SessionID) (*string, error) {
	evidence, err := s.RelationshipEvidenceForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, row := range evidence {
		if row.Kind != "started_by" {
			continue
		}
		if row.TargetState != "target_known" && row.TargetState != "target_known_retained" {
			continue
		}
		if row.TargetLocalID != nil && *row.TargetLocalID != "" {
			return row.TargetLocalID, nil
		}
	}
	return s.ParentCacheForSession(ctx, sessionID)
}
