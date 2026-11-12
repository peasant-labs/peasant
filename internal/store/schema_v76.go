package store

import (
	"fmt"
	"io"
	"os"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// migrationV76 repairs the search-index triggers that migrationV75 re-pointed
// at the retired index, and rebuilds the union index when the v75 window
// left residue it cannot otherwise expunge.
//
// The defect: on a store whose search was consolidated
// (migrateConsolidateSearch, harmonized_migrate.go), the session_entries
// triggers session_entries_ai/ad/au wrote to the live union index
// session_search_fts and the retired session_entries_fts no longer existed.
// MigrationV75 rebuilds session_entries (to widen the entry_type CHECK with
// image) and along the way drops those triggers and both FTS tables, then
// RE-CREATES the retired session_entries_fts and points session_entries_ai/ad
// /au at it, while rebuilding session_search_fts over the union view. After
// v75 every store — consolidated or not, because v75's drop/recreate erases
// the distinction — keeps trigger-maintained postings in session_entries_fts
// while session_search_fts goes stale for every session_entries insert,
// update, and delete. The merged production read (SearchMergedOnConn,
// harmonized_search.go) still finds fresh rows through the legacy side, but
// the union side serves v75-time postings.
//
// The residue is worse than stale: external-content FTS5 removes postings by
// value, so the union keeps the old terms of every row updated or deleted
// while the triggers pointed at the retired index. session_entries has no
// AUTOINCREMENT, so SQLite reuses max(rowid)+1 — once a deleted row's rowid
// is reused by a new, unrelated row, the stale terms MATCH the new row and
// pass the unified read's session_entries join. Ghosts are NOT invisible
// after rowid reuse, which format-2 conversion (which deletes mirror rows)
// reaches on real v75 stores. Only a whole-index rebuild expunges them.
//
// The repair, forward-only in one transaction (the engine already wraps each
// migration):
//  1. Drop the v75 triggers session_entries_ai/ad/au.
//  2. Re-create them pointed at session_search_fts, reusing the exact
//     migrateSearchTriggerAI/AD/AU text (harmonized_migrate.go) both this
//     migration and migrateConsolidateSearch share — no second copy.
//  3. If the residue predicate below holds, rebuild session_search_fts whole
//     (over the union view, bodies included). The rebuild is gated in SQL by
//     INSERT ... SELECT 'rebuild' WHERE <predicate>: a false predicate
//     selects zero rows and the rebuild is skipped, so clean stores never
//     pay it. SQL cannot branch otherwise, and the engine runs
//     string-only migrations with no Go hook; the in-transaction gate keeps
//     the decision atomic with the re-pointing, with no durable decision bit
//     to strand on a crash (a rollback re-runs the gate on retry).
//  4. If the predicate does not hold, bring the union entries half current
//     with the cheap re-sync instead: a plain DELETE FROM session_search_fts
//     WHERE rowid < BodyRowIDBase removes every entries-half posting for a
//     docid whose live content values the delete can see, then one INSERT
//     re-indexes exactly the live session_entries rows with current values.
//     Bodies (rid at or above BodyRowIDBase) are untouched. The re-sync is
//     likewise gated on NOT <predicate>, so a rebuilt index is not churned.
//  5. Drop session_entries_fts if it exists.
//  6. Leave session_search_state.needs_rebuild unchanged: the flag still
//     owns untrusted body postings, and v76 must neither set nor clear it.
//
// The residue predicate compares the union entries-half docsize against the
// retired index, which the v75 window kept trigger-current: both indexes
// share indexed columns and tokenizer, so a clean store compares equal.
// docsize sz is the token count, so the predicate fires on deleted rows, on
// updates that change the token count, and on reused rowids whose new
// content differs in count — and stays silent on clean stores, insert-only
// windows, and stores that cross v75 and v76 in one run (v75 rebuilt both
// freshly with no window between). It is blind to token-count-preserving
// swaps, which no docsize comparison can see.
//
// The rowid literal below is BodyRowIDBase (schema_v62.go): SQLite DDL and
// migration SQL cannot reference a Go constant, so the split is spelled out
// where the v62 CHECK and allocation subquery spell it.
const searchResiduePredicate = `EXISTS (
  SELECT id, sz FROM session_search_fts_docsize WHERE id < 1125899906842624
  EXCEPT
  SELECT id, sz FROM session_entries_fts_docsize
)`

const migrationV76 = `
DROP TRIGGER IF EXISTS session_entries_ai;
DROP TRIGGER IF EXISTS session_entries_ad;
DROP TRIGGER IF EXISTS session_entries_au;
` + migrateSearchTriggerAI + `;
` + migrateSearchTriggerAD + `;
` + migrateSearchTriggerAU + `;
INSERT INTO session_search_fts(session_search_fts) SELECT 'rebuild' WHERE ` + searchResiduePredicate + `;
DELETE FROM session_search_fts WHERE rowid < 1125899906842624 AND NOT (` + searchResiduePredicate + `);
INSERT INTO session_search_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
  SELECT rowid, content_preview, tool_input, tool_output, session_id, entry_index FROM session_entries WHERE NOT (` + searchResiduePredicate + `);
DROP TABLE IF EXISTS session_entries_fts;
`

// searchIndexResiduePresent reports whether the union search index carries
// entries-half postings absent from the retired index: the v75 window's
// deletes, token-count-changing updates, and reused rowids. Both indexes must
// exist; a v75 store carries both (v75 creates them unconditionally), so a
// missing one fails the open with an actionable error instead of silently
// taking the cheap path on a tampered store.
func searchIndexResiduePresent(conn *sqlite.Conn) (bool, error) {
	for _, table := range []string{"session_search_fts", "session_entries_fts"} {
		if !tableExistsOnConn(conn, table) {
			return false, fmt.Errorf("store: check the search index for v75 residue with %s missing: this build expects the v75 pair of search indexes; restore the database from backup or re-ingest its transcripts", table)
		}
	}
	present := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT `+searchResiduePredicate, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			present = stmt.ColumnBool(0)
			return nil
		},
	}); err != nil {
		return false, fmt.Errorf("store: evaluate the v75 search residue predicate: %w; the migration cannot decide between rebuild and re-sync", err)
	}
	return present, nil
}

// searchIndexRebuildNoticeWriter carries the one-line v76 rebuild notice. It
// defaults to standard error so the warning reaches the user's terminal;
// tests swap it to capture the notice without touching the process streams.
var searchIndexRebuildNoticeWriter io.Writer = os.Stderr

// searchIndexRebuildNotice is the one line printed before a v76 whole-index
// rebuild runs: it names the cause, the cost, and the scope.
const searchIndexRebuildNotice = "rebuilding the search index once to remove stale results left by 0.16.1; this reads all stored text and can take several minutes"

// maybePrintSearchIndexRebuildNotice evaluates the residue predicate before
// the migration chain runs and warns when v76 is about to rebuild. It runs
// only when the pre-migration version is exactly 75: earlier versions cross
// v75 and v76 in the same run with no window between, and later versions
// already ran v76. The in-migration gate re-evaluates the same predicate at
// v76's transactional point; this call only warns, once per run.
func maybePrintSearchIndexRebuildNotice(conn *sqlite.Conn, versionBeforeMigration int64) error {
	if versionBeforeMigration != 75 {
		return nil
	}
	residue, err := searchIndexResiduePresent(conn)
	if err != nil {
		return err
	}
	if !residue {
		return nil
	}
	if _, err := fmt.Fprintln(searchIndexRebuildNoticeWriter, searchIndexRebuildNotice); err != nil {
		return fmt.Errorf("store: print the search rebuild notice: %w", err)
	}
	return nil
}
