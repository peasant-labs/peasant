package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	sqlDeleteSessionCommits = `DELETE FROM session_commits WHERE session_id = ?`

	sqlInsertSessionCommit = `INSERT OR REPLACE INTO session_commits (
    session_id, commit_hash, author_name, author_email,
    message, commit_time, author_time
) VALUES (?, ?, ?, ?, ?, ?, ?)`
)

// UpsertSessionCommits atomically replaces all commit rows for a session.
// Executes DELETE + INSERT within a single transaction.
// An empty commits slice deletes existing rows without inserting new ones.
func (s *Store) UpsertSessionCommits(ctx context.Context, sessionID ingest.SessionID, commits []ingest.CommitInfo) (err error) {
	return s.writeSessionCommits(ctx, sessionID, commits, false)
}

var _ ingest.SessionCommitMergeStore = (*Store)(nil)

// MergeSessionCommits adds recovered observations without deleting historical
// bindings merely because current Git inspection could not rediscover them.
func (s *Store) MergeSessionCommits(ctx context.Context, sessionID ingest.SessionID, commits []ingest.CommitInfo) error {
	return s.writeSessionCommits(ctx, sessionID, commits, true)
}

func (s *Store) writeSessionCommits(ctx context.Context, sessionID ingest.SessionID, commits []ingest.CommitInfo, preserveExisting bool) (err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store.UpsertSessionCommits: take connection for session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)
	if err := upsertSessionCommitsOnConn(conn, sessionID, commits, preserveExisting); err != nil {
		return err
	}
	// A standalone commit update has no validated file manifest. The managed
	// artifact mirror uses the private helper and restores its exact digest in
	// its enclosing transaction after metadata and association writes succeed.
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET artifact_hash = NULL WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sessionID)}}); err != nil {
		return fmt.Errorf("store.UpsertSessionCommits: clear unverified artifact identity for session %s: %w; commit replacement was rolled back; restore database access and retry", sessionID, err)
	}
	return nil
}

func upsertSessionCommitsOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, commits []ingest.CommitInfo, preserve ...bool) (err error) {
	preserveExisting := len(preserve) > 0 && preserve[0]
	// Allocate or replay every durable association before replacing the current
	// session_commits projection. The association ledger is intentionally
	// append-only: a later re-ingest may remove a current binding, but it must
	// not erase the observed hash that the rewrite timeline needs to explain.
	for i := range commits {
		commit := &commits[i]
		var authorTime *int64
		if commit.AuthorTime != 0 {
			authorTime = &commit.AuthorTime
		}
		request := associationLookupRequest{
			SessionID:          sessionID,
			ObservedCommitHash: commit.Hash,
			Subject:            commit.Message,
			AuthorTime:         authorTime,
		}
		if err = validateAssociationLookupRequest(request); err != nil {
			return fmt.Errorf("store.UpsertSessionCommits: validate durable association for commit %q and session %s before replacing current commits: %w", commit.Hash, sessionID, err)
		}
		if _, err = lookupOrCreateSessionCommitAssociation(conn, request); err != nil {
			return fmt.Errorf("store.UpsertSessionCommits: ensure durable association for commit %s and session %s: %w", commit.Hash, sessionID, err)
		}
	}

	// Delete all current rows for this session to allow clean re-ingest. This
	// does not delete the durable association ledger, which records original
	// observations independently of the mutable current projection.
	if !preserveExisting {
		if err = sqlitex.ExecuteTransient(conn, sqlDeleteSessionCommits, &sqlitex.ExecOptions{
			Args: []any{string(sessionID)},
		}); err != nil {
			return fmt.Errorf("store.UpsertSessionCommits: delete existing commits for session %s: %w", sessionID, err)
		}
	}

	for i := range commits {
		c := &commits[i]
		authorName := nullableString(c.AuthorName)
		authorEmail := nullableString(c.AuthorEmail)
		message := nullableString(c.Message)
		commitTime := nullableInt64(c.CommitTime)
		authorTime := nullableInt64(c.AuthorTime)

		if err = sqlitex.ExecuteTransient(conn, sqlInsertSessionCommit, &sqlitex.ExecOptions{
			Args: []any{
				string(sessionID),
				c.Hash,
				authorName,
				authorEmail,
				message,
				commitTime,
				authorTime,
			},
		}); err != nil {
			return fmt.Errorf("store.UpsertSessionCommits: insert commit %s for session %s: %w", c.Hash, sessionID, err)
		}
	}

	return nil
}

// nullableString returns nil (SQL NULL) for empty strings, or the string value.
// Empty strings are stored as NULL to distinguish "unknown" from "empty".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableInt64 returns nil (SQL NULL) for zero values, or the int64 value.
// Zero timestamps are stored as NULL to distinguish "unknown" from epoch.
func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
