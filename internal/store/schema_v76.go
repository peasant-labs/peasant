package store

import (
	"fmt"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// searchBodyRowIDBaseSQL is BodyRowIDBase (schema_v62.go) spelled as SQL:
// migration SQL cannot reference a Go constant, so the one literal lives
// here and is spliced into every v76 statement that splits the entries half
// from the bodies half. A test pins it to BodyRowIDBase.
const searchBodyRowIDBaseSQL = "1125899906842624"

// searchConsolidatedPredicate is the one definition of "a 0.16.1 store
// consolidated after reaching v75": the retired index is absent AND the ai
// trigger already targets the union index. migrationV76 records it in its
// latch and searchConsolidatedOnConn evaluates it for the upgrade plan, so
// the migration and the plan cannot disagree.
const searchConsolidatedPredicate = `NOT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_entries_fts')
     AND EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'trigger' AND name = 'session_entries_ai' AND instr(sql, 'session_search_fts') > 0)`

// searchResiduePredicate holds when the union entries-half docsize carries a
// (rowid, token count) pair the retired index lacks: the residue the v75
// window leaves. migrationV76's doc explains what it can and cannot see.
const searchResiduePredicate = `EXISTS (
  SELECT id, sz FROM session_search_fts_docsize WHERE id < ` + searchBodyRowIDBaseSQL + `
  EXCEPT
  SELECT id, sz FROM session_entries_fts_docsize
)`

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
//  1. Record the decision in a TEMP latch BEFORE touching triggers:
//     consolidated = searchConsolidatedPredicate (the retired index is
//     absent AND the ai trigger already targets the union index: a 0.16.1
//     store consolidated after reaching v75). The latch holds one row; every later statement gates on it, never
//     on a fresh predicate evaluation, so exactly one branch runs.
//  2. CREATE the retired index IF NOT EXISTS with the exact v75 definition,
//     so the residue predicate below always prepares — even on a consolidated
//     store whose legacy side no longer exists.
//  3. Set residue = NOT consolidated AND <predicate> into the latch.
//  4. Drop the v75 triggers session_entries_ai/ad/au.
//  5. Re-create them pointed at session_search_fts, reusing the exact
//     migrateSearchTriggerAI/AD/AU text (harmonized_migrate.go) both this
//     migration and migrateConsolidateSearch share — no second copy.
//  6. If the latch says residue, rebuild session_search_fts whole
//     (over the union view, bodies included). The rebuild is gated in SQL by
//     INSERT ... SELECT 'rebuild' WHERE (SELECT residue FROM latch): a false
//     latch selects zero rows and the rebuild is skipped, so clean and
//     consolidated stores never pay it. SQL cannot branch otherwise, and the
//     engine runs string-only migrations with no Go hook; the in-transaction
//     latch keeps the decision atomic with the re-pointing, with no durable
//     decision bit to strand on a crash (a rollback re-runs the gate on
//     retry; the TEMP latch vanishes with the connection).
//  7. If the latch says no residue, bring the union entries half current
//     with a targeted insert: rows live in session_entries whose rowid is
//     missing from the union entries-half docsize. When the predicate is
//     false the union entries half already covers the trigger-current legacy
//     side, so only window inserts are missing; bodies (rid at or above
//     BodyRowIDBase) are untouched. Gated on (SELECT NOT residue FROM latch),
//     so the latch picks exactly one branch — a rebuilt index is never
//     re-synced, and the re-sync never runs after a rebuild within the same
//     migration.
//  8. Drop session_entries_fts if it exists, then drop the TEMP latch.
//  9. On the rebuild branch only, clear session_search_state.needs_rebuild
//     in the same latch-gated way: a whole-index rebuild re-tokenizes the
//     union view, bodies included, so a pending rebuild flag is moot. The
//     targeted-insert branch leaves the flag unchanged: it touches only the
//     entries half, and the flag still owns untrusted body postings.
//
// The residue predicate compares the union entries-half docsize against the
// retired index, which the v75 window kept trigger-current: both indexes
// share indexed columns and tokenizer, so a clean store compares equal.
// docsize sz is the token count, so the predicate fires on deleted rows, on
// updates that change the token count, and on reused rowids whose new
// content differs in count — and stays silent on clean stores, insert-only
// windows, and stores that cross v75 and v76 in one run (v75 rebuilt both
// freshly with no window between). It is blind to token-count-preserving
// swaps, which no docsize comparison can see. Every literal rowid split is
// searchBodyRowIDBaseSQL.
const migrationV76 = `
CREATE TEMP TABLE IF NOT EXISTS v76_search_latch(consolidated INTEGER NOT NULL, residue INTEGER NOT NULL);
DELETE FROM v76_search_latch;
INSERT INTO v76_search_latch(consolidated, residue)
  SELECT CASE WHEN ` + searchConsolidatedPredicate + ` THEN 1 ELSE 0 END, 0;
CREATE VIRTUAL TABLE IF NOT EXISTS session_entries_fts USING fts5(
    content_preview,
    tool_input,
    tool_output,
    session_id UNINDEXED,
    entry_index UNINDEXED,
    content='session_entries',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);
UPDATE v76_search_latch SET residue = CASE WHEN consolidated THEN 0 WHEN (` + searchResiduePredicate + `) THEN 1 ELSE 0 END;
DROP TRIGGER IF EXISTS session_entries_ai;
DROP TRIGGER IF EXISTS session_entries_ad;
DROP TRIGGER IF EXISTS session_entries_au;
` + migrateSearchTriggerAI + `;
` + migrateSearchTriggerAD + `;
` + migrateSearchTriggerAU + `;
INSERT INTO session_search_fts(session_search_fts) SELECT 'rebuild' WHERE (SELECT residue FROM v76_search_latch);
UPDATE session_search_state SET needs_rebuild = 0 WHERE (SELECT residue FROM v76_search_latch);
INSERT INTO session_search_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
  SELECT rowid, content_preview, tool_input, tool_output, session_id, entry_index FROM session_entries WHERE rowid NOT IN (SELECT id FROM session_search_fts_docsize WHERE id < ` + searchBodyRowIDBaseSQL + `) AND (SELECT NOT residue FROM v76_search_latch);
DROP TABLE IF EXISTS session_entries_fts;
DROP TABLE IF EXISTS temp.v76_search_latch;
`

// searchConsolidatedOnConn reports whether the store is a 0.16.1 store
// consolidated after reaching v75. It evaluates searchConsolidatedPredicate,
// the same text v76 records in its TEMP latch before touching triggers, so
// the upgrade plan's cost for this step agrees with the migration.
func searchConsolidatedOnConn(conn *sqlite.Conn) (bool, error) {
	consolidated := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT CASE WHEN `+searchConsolidatedPredicate+` THEN 1 ELSE 0 END`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			consolidated = stmt.ColumnInt64(0) != 0
			return nil
		},
	}); err != nil {
		return false, fmt.Errorf("store: check whether the search index was consolidated after v75: %w", err)
	}
	return consolidated, nil
}

// searchIndexResiduePresent reports whether the union search index carries
// entries-half postings absent from the retired index: the v75 window's
// deletes, token-count-changing updates, and reused rowids. A store
// consolidated after v75 carries no residue by definition: the union index
// was rebuilt at consolidation and later writes went through union triggers,
// so it reports false with no error. Otherwise both indexes must exist; a
// v75 store carries both (v75 creates them unconditionally), so a missing
// one is a tampered store and reports an actionable error instead of a
// verdict. The upgrade plan handles the two missing-table cases itself
// before it asks (searchRepairStep).
func searchIndexResiduePresent(conn *sqlite.Conn) (bool, error) {
	consolidated, err := searchConsolidatedOnConn(conn)
	if err != nil {
		return false, err
	}
	if consolidated {
		return false, nil
	}
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
		return false, fmt.Errorf("store: evaluate the v75 search residue predicate: %w; the plan cannot say whether the repair rebuilds the index", err)
	}
	return present, nil
}
