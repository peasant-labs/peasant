package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The rebuild is the store capability `harvest index --all` calls after it has
// enumerated every rollout. It re-derives both tables from their sources.
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
	// clause, and the clause keeps the statement safe on a real store. The
	// predicate matches the migration's backfill: one definition of which
	// stored spawn records are projectable.
	sqlRebuildClaudeTeammateSpawns = `INSERT OR IGNORE INTO claude_teammate_spawns (spawner_path, team, name)
SELECT e.source_path,
       json_extract(spawn.value, '$.team'),
       json_extract(spawn.value, '$.name')
FROM claude_transcript_evidence AS e
JOIN json_each(e.spawns_json) AS spawn
WHERE e.scope = 'root'
  AND json_extract(spawn.value, '$.team') IS NOT NULL
  AND json_extract(spawn.value, '$.name') IS NOT NULL`

	// One row per enumerated rollout PATH. A rollout the duplicate stage
	// refuses, or one that was never stored, still has a row, so the
	// session_id index can answer both the duplicate count and the parent
	// lookup. The path is the primary key, so a duplicate path in the
	// caller's walk collapses instead of aborting the rebuild.
	sqlRebuildCodexRolloutCatalog = `INSERT OR REPLACE INTO codex_rollout_catalog (source_path, session_id) VALUES (?, ?)`
)

// RebuildDerivedLookups rebuilds spawns and reconciles catalog rows in one transaction.
// The caller determines safe deletions from filesystem walk coverage.
func (s *Store) RebuildDerivedLookups(ctx context.Context, enumeration ingest.RolloutEnumeration) (err error) {
	switch enumeration.Status {
	case ingest.RolloutEnumerationNotWalked, ingest.RolloutEnumerationComplete, ingest.RolloutEnumerationPartial:
	default:
		return fmt.Errorf(
			"store.RebuildDerivedLookups: the Codex rollout enumeration carries no status, so nothing is known about its coverage: %w; "+
				"what: the caller passed a zero RolloutEnumeration; "+
				"why: reconciliation needs an explicit account of walk coverage; "+
				"where: store.RebuildDerivedLookups in internal/store/derived_lookup_rebuild.go, called by `harvest index --all`; "+
				"when: before any derived row was read or written; "+
				"impact: the derived lookup tables were left as they were and no saved pair was indexed; "+
				"fix: report RolloutEnumerationNotWalked when no Codex source was walked, or the status of the walk that ran",
			ingest.ErrIncompleteRolloutEnumeration)
	}

	// Validate the caller's files before either projection is changed.
	if enumeration.Status != ingest.RolloutEnumerationNotWalked {
		for index := range enumeration.Files {
			file := &enumeration.Files[index]
			if file.Path == "" {
				return fmt.Errorf("store.RebuildDerivedLookups: enumerated rollout %d carries an empty path; pass every rollout path the walk enumerated; nothing was written and the previous catalog is intact", index)
			}
			if file.SessionID == "" {
				return fmt.Errorf("store.RebuildDerivedLookups: enumerated rollout %q carries an empty session id; the catalog exists to answer duplicate and parent questions by id, so an id-less row is useless; nothing was written and the previous catalog is intact", file.Path)
			}
		}
	}

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

	if enumeration.Status == ingest.RolloutEnumerationNotWalked {
		// No Codex source was walked, so the catalog has no source to be
		// re-derived from. Leave it as the last full walk left it.
		return nil
	}

	for _, path := range enumeration.Deletes {
		if err = sqlitex.Execute(conn, sqlDeleteCodexRolloutCatalog, &sqlitex.ExecOptions{Args: []any{path.String()}}); err != nil {
			return fmt.Errorf("store.RebuildDerivedLookups: delete rollout %q: %w; the transaction rolled back", path, err)
		}
	}

	for index := range enumeration.Files {
		file := &enumeration.Files[index]
		if err = sqlitex.Execute(conn, sqlRebuildCodexRolloutCatalog, &sqlitex.ExecOptions{
			Args: []any{file.Path.String(), file.SessionID.String()},
		}); err != nil {
			return fmt.Errorf("store.RebuildDerivedLookups: catalog enumerated rollout %q: %w; the transaction rolled back and the previous catalog is intact", file.Path, err)
		}
	}
	return nil
}
