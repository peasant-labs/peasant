package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// sqlRecordedDirectory is the directory a session was recorded in: its git
// worktree when ingest captured one, else its project's canonical working
// directory. Every read below derives it the same way.
const sqlRecordedDirectory = `COALESCE(NULLIF(s.git_worktree, ''), p.canonical_cwd, '')`

// RecordedDirectories returns every directory a stored session was recorded in,
// each once. It is raw evidence: which repository a directory belongs to, if
// any, is the caller's question.
func (s *Store) RecordedDirectories(ctx context.Context) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: recorded directories take connection: %w", err)
	}
	defer s.pool.Put(conn)
	var dirs []string
	err = sqlitex.ExecuteTransient(conn, `SELECT DISTINCT `+sqlRecordedDirectory+` AS dir
FROM sessions s LEFT JOIN projects p ON p.project_hash = s.project_hash
WHERE dir <> '' ORDER BY dir`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		dirs = append(dirs, stmt.ColumnText(0))
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store: recorded directories query: %w", err)
	}
	return dirs, nil
}

// SessionDirectories returns the directory each named session was recorded in.
// A session that is not stored, or that recorded no directory, is absent.
func (s *Store) SessionDirectories(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: session directories take connection: %w", err)
	}
	defer s.pool.Put(conn)
	for start := 0; start < len(sessionIDs); start += indexFormatReadBatchSize {
		batch := sessionIDs[start:min(start+indexFormatReadBatchSize, len(sessionIDs))]
		err = sqlitex.ExecuteTransient(conn, `SELECT s.session_id, `+sqlRecordedDirectory+`
FROM sessions s LEFT JOIN projects p ON p.project_hash = s.project_hash
WHERE s.session_id IN (`+sqlPlaceholders(len(batch))+`)`, &sqlitex.ExecOptions{Args: sessionIDArgs(batch), ResultFunc: func(stmt *sqlite.Stmt) error {
			if dir := stmt.ColumnText(1); dir != "" {
				out[stmt.ColumnText(0)] = dir
			}
			return nil
		}})
		if err != nil {
			return nil, fmt.Errorf("store: session directories query: %w", err)
		}
	}
	return out, nil
}

// LatestPublication returns the receipt this account on this Village updated
// last, for any session, or nil when the account has published nothing from
// this computer.
func (s *Store) LatestPublication(ctx context.Context, origin, owner string) (*PublicationRecord, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the latest publication receipt: acquire SQLite connection: %w", err)
	}
	defer s.pool.Put(conn)
	var out *PublicationRecord
	err = sqlitex.ExecuteTransient(conn, `SELECT session_id, project_hash, receipt_json FROM session_publications
WHERE village_origin=? AND owner_user_id=? ORDER BY remote_updated_at DESC, published_at DESC, session_id LIMIT 1`, &sqlitex.ExecOptions{Args: []any{origin, owner}, ResultFunc: func(stmt *sqlite.Stmt) error {
		record, scanErr := scanPublicationRecord(stmt, origin, owner)
		if scanErr != nil {
			return scanErr
		}
		out = &record
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("read the latest publication receipt: %w", err)
	}
	return out, nil
}

// scanPublicationRecord reads one receipt row selected as session_id,
// project_hash, receipt_json.
func scanPublicationRecord(stmt *sqlite.Stmt, origin, owner string) (PublicationRecord, error) {
	sessionID := stmt.ColumnText(0)
	projectHash, err := schema.NewProjectHash(stmt.ColumnText(1))
	if err != nil {
		return PublicationRecord{}, fmt.Errorf("session %q: stored project hash: %w", sessionID, err)
	}
	record := PublicationRecord{VillageOrigin: origin, OwnerUserID: owner, SessionID: sessionID, ProjectHash: projectHash}
	if err := json.Unmarshal([]byte(stmt.ColumnText(2)), &record.Receipt); err != nil {
		return PublicationRecord{}, fmt.Errorf("session %q: %w", sessionID, err)
	}
	return record, nil
}
