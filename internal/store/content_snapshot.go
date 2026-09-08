package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SessionContentSnapshot is one committed SQL view, not proof that retained
// files still match it. Resources are released before returning owned rows.
type SessionContentSnapshot struct {
	Detail       *SessionDetailRow
	IndexState   *ingest.SessionIndexState
	Entries      []schema.SessionEntry
	Metrics      *ingest.SessionMetrics
	Associations []ingest.CurrentCommitAssociation
}

// ReadSessionContent reads the session and all content context on one connection.
// Managed-file callers acquire file ownership before entering this method.
func (s *Store) ReadSessionContent(ctx context.Context, sessionID string) (_ *SessionContentSnapshot, retErr error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take session content connection: %w", err)
	}
	defer s.pool.Put(conn)
	endSnapshot := sqlitex.Save(conn)
	defer endSnapshot(&retErr)
	detail, err := sessionDetailByIDOnConn(conn, sessionID)
	if err != nil || detail == nil {
		return nil, err
	}
	sid := ingest.SessionID(sessionID)
	snapshot := &SessionContentSnapshot{Detail: detail}
	snapshot.IndexState, err = readIndexStateOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	snapshot.Entries, err = s.listEntriesOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	snapshot.Metrics, err = getMetricsOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	snapshot.Associations, err = listCurrentSessionCommitAssociationsOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}
