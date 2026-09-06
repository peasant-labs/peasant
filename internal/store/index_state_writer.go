package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const sqlUpdateIndexState = `UPDATE sessions SET index_version = ?, indexed_at = ?, session_entries_hash = NULL WHERE session_id = ?`

const sqlUpdateIndexStateWithSessionEntriesHash = `UPDATE sessions SET index_version = ?, indexed_at = ?, session_entries_hash = ? WHERE session_id = ?`

// validateIndexerRevisionOnConn runs under the same transaction as entry writes.
// A force or source refresh must not replace output produced by a newer parser.
func validateIndexerRevisionOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, version int) error {
	found := false
	if err := sqlitex.ExecuteTransient(conn, "SELECT index_version FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			if stored := stmt.ColumnInt(0); stored > version {
				return fmt.Errorf("session %s was indexed by revision %d, newer than this writer's revision %d; use a newer Peasant build to refresh it without losing parser output", sessionID, stored, version)
			}
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: validate indexer revision before replacing entries: %w", err)
	}
	if !found {
		return fmt.Errorf("store: cannot index session %s before its metadata is stored; import the session and retry", sessionID)
	}
	return nil
}

// UpdateIndexState sets the index_version and indexed_at for a session after
// successful indexing.
func (s *Store) UpdateIndexState(ctx context.Context, sessionID ingest.SessionID, version int, indexedAtMs int64) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlUpdateIndexState, &sqlitex.ExecOptions{
		Args: []any{version, indexedAtMs, string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: update index state for %s: %w", sessionID, err)
	}
	return nil
}

// UpdateIndexStateWithSessionEntriesHash sets the index state and the parsed
// entries digest together. Use it only after the same transaction has stored the
// matching session_entries rows.
func (s *Store) UpdateIndexStateWithSessionEntriesHash(ctx context.Context, sessionID ingest.SessionID, version int, indexedAtMs int64, sessionEntriesHash string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := updateIndexStateWithSessionEntriesHashOnConn(conn, sessionID, version, indexedAtMs, sessionEntriesHash); err != nil {
		return err
	}
	return nil
}

func updateIndexStateWithSessionEntriesHashOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, version int, indexedAtMs int64, sessionEntriesHash string) error {
	if err := sqlitex.ExecuteTransient(conn, sqlUpdateIndexStateWithSessionEntriesHash, &sqlitex.ExecOptions{
		Args: []any{version, indexedAtMs, sessionEntriesHash, string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: update index state and session_entries_hash for %s: %w", sessionID, err)
	}
	return nil
}

// ListStaleIndexSessions selects only registered harness targets. The legacy SQL
// index_version column records the actual indexer revision, not an index format.
func (s *Store) ListStaleIndexSessions(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions) ([]ingest.SessionID, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	conditions := make([]string, 0, len(targets))
	args := make([]any, 0, len(targets)*2)
	for _, harness := range slices.Sorted(maps.Keys(targets)) {
		version := targets[harness].IndexerVersion
		if !harness.IsKnown() || version < 1 {
			return nil, fmt.Errorf("store: select stale indexes: harness %q has invalid indexer target %d; supply registered positive harvester versions", harness, version)
		}
		conditions = append(conditions, "(model_harness = ? AND index_version < ?)")
		args = append(args, string(harness), version)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var sessions []ingest.SessionID
	if err := sqlitex.ExecuteTransient(conn, "SELECT session_id FROM sessions WHERE "+strings.Join(conditions, " OR ")+" ORDER BY session_id", &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			raw := stmt.ColumnText(0)
			sid, err := ingest.NewSessionID(raw)
			if err != nil {
				// Skip invalid session IDs (should not happen in practice).
				return nil
			}
			sessions = append(sessions, sid)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list stale index sessions: %w", err)
	}
	return sessions, nil
}
