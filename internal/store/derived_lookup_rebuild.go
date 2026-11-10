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
// catalog from the enumeration `harvest index --all` walked. Both replacements
// happen in one transaction, so a failure leaves the previous tables intact
// rather than a half-rebuilt store. The tables are replaced rather than merged
// so a corrupted row that no source still produces is removed instead of
// surviving.
//
// The catalog records paths that were never stored, so the database cannot
// derive it; the caller supplies the enumeration its walk produced. That
// enumeration is REPLACED into the table, which makes its completeness part of
// the input:
//
//   - Complete replaces the catalog, after the caller's set has been validated.
//   - NotWalked means this run configured no Codex source. There is nothing to
//     derive the catalog from, so it is left exactly as it is and only the
//     spawns are rebuilt; a run that never read a Codex tree must not be read as
//     "the tree is empty".
//   - Partial and the unset zero value are REFUSED before anything is written.
//     A walk that swallowed a directory-read or file-stat failure may have
//     missed rollouts that are still on disk, and clearing the table would
//     delete the rows that answer the duplicate question for them.
func (s *Store) RebuildDerivedLookups(ctx context.Context, enumeration ingest.RolloutEnumeration) (err error) {
	switch enumeration.Status {
	case ingest.RolloutEnumerationNotWalked, ingest.RolloutEnumerationComplete:
	case ingest.RolloutEnumerationPartial:
		return fmt.Errorf(
			"store.RebuildDerivedLookups: the Codex rollout enumeration is incomplete, so the rollout catalog cannot be replaced: %w; "+
				"what: a directory-read or file-stat failure during the discovery walk means the enumerated set may be missing rollouts that still exist; "+
				"why: replacing the catalog with a short set would delete the rows that answer the duplicate and parent questions for those rollouts; "+
				"where: store.RebuildDerivedLookups in internal/store/derived_lookup_rebuild.go, called by `harvest index --all`; "+
				"when: before any derived row was read or written; "+
				"impact: the derived lookup tables were left as they were and no saved pair was indexed; "+
				"fix: check every configured Codex source path and its read access (an unreadable directory is named in the run's discovery diagnostics), then rerun `harvest index --all`",
			ingest.ErrIncompleteRolloutEnumeration)
	default:
		return fmt.Errorf(
			"store.RebuildDerivedLookups: the Codex rollout enumeration carries no status, so nothing is known about its coverage: %w; "+
				"what: the caller passed a zero RolloutEnumeration; "+
				"why: a rebuild from a set whose coverage is unknown could clear rows no source produced; "+
				"where: store.RebuildDerivedLookups in internal/store/derived_lookup_rebuild.go, called by `harvest index --all`; "+
				"when: before any derived row was read or written; "+
				"impact: the derived lookup tables were left as they were and no saved pair was indexed; "+
				"fix: report RolloutEnumerationNotWalked when the run configured no Codex source, or the complete enumeration its walk produced",
			ingest.ErrIncompleteRolloutEnumeration)
	}

	// Validate the caller's set before anything is cleared. An empty path or id
	// is a caller error, and it must not be discovered after the rows it would
	// replace are already gone.
	if enumeration.Status == ingest.RolloutEnumerationComplete {
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

	if err = sqlitex.ExecuteTransient(conn, sqlClearCodexRolloutCatalog, nil); err != nil {
		return fmt.Errorf("store.RebuildDerivedLookups: clear the Codex rollout catalog: %w; the transaction rolled back and the previous catalog is intact", err)
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
