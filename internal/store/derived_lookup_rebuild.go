package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The rebuild is the store capability the full discovery walk calls after it
// has enumerated every rollout. It re-derives both tables from their sources.
var _ ingest.DerivedLookupRebuilder = (*Store)(nil)

// The rebuild's statements are its own, deliberately not the maintenance SQL
// the evidence transaction uses. The derived tables are a projection of the
// evidence rows and the enumerated rollout files, so both paths must be able
// to disagree: a maintenance write that is dropped has to show up as a
// difference the rebuild repairs, not pass because both paths call one
// function.
const (
	sqlClearClaudeTeammateSpawns = `DELETE FROM claude_teammate_spawns`

	// The spawns projection is re-derived from the root evidence rows alone.
	// A root row's spawns_json holds the records mining appended without
	// deduplicating them, so a transcript may repeat a (team, name); the
	// composite primary key collapses the repeat even without the ignore
	// clause, and the clause keeps the statement safe on a real store.
	sqlRebuildClaudeTeammateSpawns = `INSERT OR IGNORE INTO claude_teammate_spawns (spawner_path, team, name)
SELECT e.source_path,
       json_extract(spawn.value, '$.team'),
       json_extract(spawn.value, '$.name')
FROM claude_transcript_evidence AS e
JOIN json_each(e.spawns_json) AS spawn
WHERE e.scope = 'root'
  AND json_type(spawn.value, '$.team') = 'text'
  AND json_type(spawn.value, '$.name') = 'text'`

	sqlClearCodexRolloutCatalog = `DELETE FROM codex_rollout_catalog`

	// One row per enumerated rollout PATH. A rollout the duplicate stage
	// refuses, or one that was never stored, still has a row, so the
	// session_id index can answer both the duplicate count and the parent
	// lookup. The path is the primary key, so a duplicate path in the
	// caller's walk collapses instead of aborting the rebuild.
	sqlRebuildCodexRolloutCatalog = `INSERT OR IGNORE INTO codex_rollout_catalog (source_path, session_id) VALUES (?, ?)`
)

// RebuildDerivedLookups replaces the derived lookup tables from their sources:
// the teammate spawns from claude_transcript_evidence, and the Codex rollout
// catalog from the caller's enumerated walk. Both replacements happen in one
// transaction, so a failure leaves the previous tables intact rather than a
// half-rebuilt store. The tables are replaced rather than merged so a
// corrupted row that no source still produces is removed instead of surviving.
//
// The caller supplies the rollouts it enumerated because the catalog records
// paths that were never stored; the database cannot derive them.
func (s *Store) RebuildDerivedLookups(ctx context.Context, catalog []ingest.RolloutFile) (err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store.RebuildDerivedLookups: take connection: %w; the derived lookup tables were not rebuilt; restore database access and rerun the full discovery walk", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	if err = sqlitex.ExecuteTransient(conn, sqlClearClaudeTeammateSpawns, nil); err != nil {
		return fmt.Errorf("store.RebuildDerivedLookups: clear the teammate spawns projection: %w; the transaction rolled back and the previous table is intact", err)
	}
	if err = sqlitex.ExecuteTransient(conn, sqlRebuildClaudeTeammateSpawns, nil); err != nil {
		return fmt.Errorf("store.RebuildDerivedLookups: re-derive the teammate spawns from the stored evidence: %w; the transaction rolled back and the previous table is intact", err)
	}

	if err = sqlitex.ExecuteTransient(conn, sqlClearCodexRolloutCatalog, nil); err != nil {
		return fmt.Errorf("store.RebuildDerivedLookups: clear the Codex rollout catalog: %w; the transaction rolled back and the previous catalog is intact", err)
	}
	for index := range catalog {
		file := &catalog[index]
		if file.Path == "" {
			return fmt.Errorf("store.RebuildDerivedLookups: enumerated rollout %d carries an empty path; pass every rollout path the walk enumerated; the transaction rolled back and the previous catalog is intact", index)
		}
		if file.SessionID == "" {
			return fmt.Errorf("store.RebuildDerivedLookups: enumerated rollout %q carries an empty session id; the catalog exists to answer duplicate and parent questions by id, so an id-less row is useless; the transaction rolled back and the previous catalog is intact", file.Path)
		}
		if err = sqlitex.Execute(conn, sqlRebuildCodexRolloutCatalog, &sqlitex.ExecOptions{
			Args: []any{file.Path.String(), file.SessionID.String()},
		}); err != nil {
			return fmt.Errorf("store.RebuildDerivedLookups: catalog enumerated rollout %q: %w; the transaction rolled back and the previous catalog is intact", file.Path, err)
		}
	}
	return nil
}
