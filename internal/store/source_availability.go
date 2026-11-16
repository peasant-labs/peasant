package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const migrationV63 = `CREATE TABLE session_source_unavailability (
 session_id TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
 reason TEXT NOT NULL CHECK (reason IN ('original-source-unavailable-no-usable-saved-copy'))
) STRICT;`

var _ ingest.SourceAvailabilityStore = (*Store)(nil)

// RecordSourceUnavailable atomically inserts or upgrades one lane's cause.
// Repeats and less-specific observations do not change it, even when multiple
// workers report concurrently. An absent row is not proof of source health.
func (s *Store) RecordSourceUnavailable(ctx context.Context, sid ingest.SessionID, lane ingest.SourceUnavailabilityLane, reason ingest.SourceUnavailabilityReason) (bool, error) {
	validLane, err := ingest.NewSourceUnavailabilityLane(string(lane))
	if err != nil {
		return false, err
	}
	validated, err := ingest.NewSourceUnavailabilityReason(string(reason))
	if err != nil {
		return false, err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, err
	}
	defer s.pool.Put(conn)
	changed := false
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability (session_id, lane, reason) VALUES (?, ?, ?)
ON CONFLICT(session_id, lane) DO UPDATE SET reason = excluded.reason
WHERE session_source_unavailability.reason = ? RETURNING session_id`, &sqlitex.ExecOptions{
		Args:       []any{string(sid), string(validLane), string(validated), string(validated.SupersededReason())},
		ResultFunc: func(*sqlite.Stmt) error { changed = true; return nil },
	})
	if err != nil {
		return false, fmt.Errorf("record unavailable source for session %s in diagnostic lane %s: %w; prior content remains stored; restore database access and rerun harvest", sid, validLane, err)
	}
	return changed, nil
}

// ReadSourceUnavailabilityForLane reads exactly one diagnostic lane.
func (s *Store) ReadSourceUnavailabilityForLane(ctx context.Context, sid ingest.SessionID, lane ingest.SourceUnavailabilityLane) (*ingest.SourceUnavailabilityReason, error) {
	validLane, err := ingest.NewSourceUnavailabilityLane(string(lane))
	if err != nil {
		return nil, err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	var reason *ingest.SourceUnavailabilityReason
	err = sqlitex.ExecuteTransient(conn, `SELECT reason FROM session_source_unavailability WHERE session_id = ? AND lane = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), string(validLane)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			value, err := ingest.NewSourceUnavailabilityReason(stmt.ColumnText(0))
			if err == nil {
				reason = &value
			}
			return err
		},
	})
	return reason, err
}

// ReadSourceUnavailabilityDetail returns the most specific cause across
// diagnostic lanes and its owner. Equal causes use lexical lane order, making
// the answer deterministic regardless of insertion order.
func (s *Store) ReadSourceUnavailabilityDetail(ctx context.Context, sid ingest.SessionID) (*ingest.SourceUnavailability, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	var selected *ingest.SourceUnavailability
	err = sqlitex.ExecuteTransient(conn, `SELECT lane, reason FROM session_source_unavailability WHERE session_id = ? ORDER BY lane`, &sqlitex.ExecOptions{
		Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			lane, err := ingest.NewSourceUnavailabilityLane(stmt.ColumnText(0))
			if err != nil {
				return err
			}
			reason, err := ingest.NewSourceUnavailabilityReason(stmt.ColumnText(1))
			if err != nil {
				return err
			}
			if selected == nil || reason.SupersededReason() == selected.Reason {
				selected = &ingest.SourceUnavailability{Lane: lane, Reason: reason}
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return selected, nil
}

// ReadSourceUnavailability is the reason-only view of the most specific lane.
func (s *Store) ReadSourceUnavailability(ctx context.Context, sid ingest.SessionID) (*ingest.SourceUnavailabilityReason, error) {
	selected, err := s.ReadSourceUnavailabilityDetail(ctx, sid)
	if err != nil || selected == nil {
		return nil, err
	}
	return &selected.Reason, nil
}
