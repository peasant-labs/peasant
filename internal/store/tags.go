package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SQL constants for tag operations.
const (
	sqlGetTags = `SELECT tags FROM sessions WHERE session_id = ?`
	sqlSetTags = `UPDATE sessions SET tags = ? WHERE session_id = ?`
)

// GetTags returns the tags for a session as a string slice.
// Returns nil if the session has no tags (NULL) or does not exist.
func (s *Store) GetTags(ctx context.Context, sessionID ingest.SessionID) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var tags []string
	if err := sqlitex.ExecuteTransient(conn, sqlGetTags, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			raw := stmt.ColumnText(0)
			if raw == "" {
				return nil
			}
			return json.Unmarshal([]byte(raw), &tags)
		},
	}); err != nil {
		return nil, fmt.Errorf("store: get tags %s: %w", sessionID, err)
	}
	return tags, nil
}

// SetTags replaces all tags for a session with the given slice.
// Pass nil or empty slice to clear all tags.
func (s *Store) SetTags(ctx context.Context, sessionID ingest.SessionID, tags []string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var tagsJSON any
	if len(tags) > 0 {
		b, err := json.Marshal(tags)
		if err != nil {
			return fmt.Errorf("store: marshal tags: %w", err)
		}
		tagsJSON = string(b)
	}

	if err := sqlitex.ExecuteTransient(conn, sqlSetTags, &sqlitex.ExecOptions{
		Args: []any{tagsJSON, string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: set tags %s: %w", sessionID, err)
	}
	return nil
}

// AddTag appends a tag to the session's tags list. Idempotent — adding an
// existing tag is a no-op.
func (s *Store) AddTag(ctx context.Context, sessionID ingest.SessionID, tag string) error {
	existing, err := s.GetTags(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, t := range existing {
		if t == tag {
			return nil // already present
		}
	}
	return s.SetTags(ctx, sessionID, append(existing, tag))
}

// RemoveTag removes a tag from the session's tags list. Removing an absent
// tag is a no-op.
func (s *Store) RemoveTag(ctx context.Context, sessionID ingest.SessionID, tag string) error {
	existing, err := s.GetTags(ctx, sessionID)
	if err != nil {
		return err
	}
	filtered := make([]string, 0, len(existing))
	for _, t := range existing {
		if t != tag {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == len(existing) {
		return nil // tag was not present
	}
	return s.SetTags(ctx, sessionID, filtered)
}
