package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SessionContentSnapshot is one committed SQL view of stored content and its
// context. Resources are released before returning owned rows. Capture states
// what the database can prove about Entries, so a caller can label a bounded
// preview without asking the store a second question.
type SessionContentSnapshot struct {
	Detail       *SessionDetailRow
	IndexState   *ingest.SessionIndexState
	Entries      []schema.SessionEntry
	Capture      ingest.SessionContentCapture
	Metrics      *ingest.SessionMetrics
	Associations []ingest.CurrentCommitAssociation
}

// ReadSessionContent reads the session and its verified COMPLETE content on one
// connection. Content is database-authoritative; no managed files are consulted.
// An incomplete capture is refused with ErrContentCaptureIncomplete: export and
// publication use this reader. Previewers use ReadSessionAvailable instead.
func (s *Store) ReadSessionContent(ctx context.Context, sessionID string) (*SessionContentSnapshot, error) {
	return s.readSessionSnapshot(ctx, sessionID, ingest.SessionEntryReadFullContent)
}

// ReadSessionAvailable reads the session and the content the database actually
// holds: the verified full text when the capture is complete, and the bounded
// stored projection otherwise. It is never gated on capture completeness,
// publication readiness, recovery or a native source, so every mounted
// previewer can show what was recorded. It still validates the stored index
// format, and a damaged complete capture still fails: a preview may show less
// than the whole session, never content the database cannot prove it stored.
// Reading it successfully does not certify the session for export or
// publication; those use ReadSessionContent.
func (s *Store) ReadSessionAvailable(ctx context.Context, sessionID string) (*SessionContentSnapshot, error) {
	return s.readSessionSnapshot(ctx, sessionID, ingest.SessionEntryReadAvailable)
}

func (s *Store) readSessionSnapshot(ctx context.Context, sessionID string, mode ingest.SessionEntryReadMode) (_ *SessionContentSnapshot, retErr error) {
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
	if err := s.ValidateIndexFormatsOnConn(conn, []schema.SessionID{sid}); err != nil {
		return nil, err
	}
	if mode == ingest.SessionEntryReadAvailable {
		snapshot.Entries, snapshot.Capture, err = loadAvailableSessionEntriesOnConn(ctx, conn, sid)
	} else {
		snapshot.Entries, snapshot.Capture, err = loadFullSessionEntriesOnConn(ctx, conn, sid, 0)
	}
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
