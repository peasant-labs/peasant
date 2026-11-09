package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The store is the DerivedLookupReader the teammate-link and Codex duplicate
// and parent decisions read through. Every statement below is an indexed read
// over one derived lookup table; none scans the stored population.
var _ ingest.DerivedLookupReader = (*Store)(nil)

const (
	// sqlCountClaudeIdentityDeclarers counts the root transcripts that declare a
	// teammate identity. It is written to match the partial index exactly:
	// scope = 'root' is part of the index predicate, and both identity columns
	// are the index key, so the count is a covering-index seek.
	sqlCountClaudeIdentityDeclarers = `SELECT COUNT(*) FROM claude_transcript_evidence
WHERE scope = 'root' AND identity_team = ? AND identity_name = ?`

	// sqlCountClaudeIdentitySpawners counts the distinct spawner transcripts
	// that name an identity. Both columns are the index key; DISTINCT on the
	// primary-key column is belt-and-braces, because the composite primary key
	// already makes one spawner path one row per pair.
	sqlCountClaudeIdentitySpawners = `SELECT COUNT(DISTINCT spawner_path) FROM claude_teammate_spawns
WHERE team = ? AND name = ?`

	// sqlCountCodexRolloutsBySession counts the catalogued rollout paths that
	// carry one id. The session_id index is deliberately non-unique: a count of
	// two is the duplicate the run must refuse.
	sqlCountCodexRolloutsBySession = `SELECT COUNT(*) FROM codex_rollout_catalog WHERE session_id = ?`

	// sqlCodexRolloutPathsBySession returns the catalogued paths for one id, in
	// path order, for a parent lookup through the same session_id index.
	sqlCodexRolloutPathsBySession = `SELECT source_path FROM codex_rollout_catalog WHERE session_id = ? ORDER BY source_path`
)

// CountClaudeIdentityDeclarers counts the stored root transcripts that declare
// id. The count is over distinct root paths because the evidence table is keyed
// by source path.
func (s *Store) CountClaudeIdentityDeclarers(ctx context.Context, id ingest.ClaudeTeammateIdentity) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: take connection to count Claude identity declarers: %w; the ambiguity answer is unavailable; restore database access and retry the link step", err)
	}
	defer s.pool.Put(conn)
	count := 0
	if err := sqlitex.ExecuteTransient(conn, sqlCountClaudeIdentityDeclarers, &sqlitex.ExecOptions{
		Args: []any{id.Team, id.Name},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: count Claude identity declarers for (%q, %q): %w; the ambiguity answer is unavailable; restore database access and retry the link step", id.Team, id.Name, err)
	}
	return count, nil
}

// CountClaudeIdentitySpawners counts the distinct stored spawner transcripts
// that name id.
func (s *Store) CountClaudeIdentitySpawners(ctx context.Context, id ingest.ClaudeTeammateIdentity) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: take connection to count Claude identity spawners: %w; the ambiguity answer is unavailable; restore database access and retry the link step", err)
	}
	defer s.pool.Put(conn)
	count := 0
	if err := sqlitex.ExecuteTransient(conn, sqlCountClaudeIdentitySpawners, &sqlitex.ExecOptions{
		Args: []any{id.Team, id.Name},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: count Claude identity spawners for (%q, %q): %w; the ambiguity answer is unavailable; restore database access and retry the link step", id.Team, id.Name, err)
	}
	return count, nil
}

// CountCodexRolloutsBySession counts the catalogued rollout paths that carry
// id. Two is the duplicate the run refuses.
func (s *Store) CountCodexRolloutsBySession(ctx context.Context, id ingest.SessionID) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: take connection to count Codex rollout paths for %s: %w; the duplicate answer is unavailable; restore database access and retry the lookup", id, err)
	}
	defer s.pool.Put(conn)
	count := 0
	if err := sqlitex.ExecuteTransient(conn, sqlCountCodexRolloutsBySession, &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: count Codex rollout paths for %s: %w; the duplicate answer is unavailable; restore database access and retry the lookup", id, err)
	}
	return count, nil
}

// CodexRolloutPaths returns the catalogued rollout paths that carry id, in path
// order. An id with no catalogued path returns an empty slice.
func (s *Store) CodexRolloutPaths(ctx context.Context, id ingest.SessionID) ([]ingest.ResolvedPath, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to read Codex rollout paths for %s: %w; the parent lookup is unavailable; restore database access and retry", id, err)
	}
	defer s.pool.Put(conn)
	var paths []ingest.ResolvedPath
	if err := sqlitex.ExecuteTransient(conn, sqlCodexRolloutPathsBySession, &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			path, err := ingest.NewResolvedPath(stmt.ColumnText(0))
			if err != nil {
				return fmt.Errorf("store: catalogued Codex rollout for %s carries an invalid path %q: %w; the row cannot be located; repair the catalog before retrying", id, stmt.ColumnText(0), err)
			}
			paths = append(paths, path)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read Codex rollout paths for %s: %w; the parent lookup is unavailable; restore database access and retry", id, err)
	}
	return paths, nil
}
