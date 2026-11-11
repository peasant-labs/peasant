package store

// migrationV76 repairs the search-index triggers that migrationV75 re-pointed
// at the retired index.
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
// the union side serves v75-time postings: a re-ranked stale posting can win
// the merge dedup and a future consolidation would need a full rebuild.
//
// The repair, forward-only in one transaction (the engine already wraps each
// migration), cost proportional to session_entries only:
//  1. Drop the v75 triggers session_entries_ai/ad/au.
//  2. Re-create them pointed at session_search_fts, reusing the exact
//     migrateSearchTriggerAI/AD/AU text (harmonized_migrate.go) both this
//     migration and migrateConsolidateSearch share — no second copy.
//  3. Bring session_search_fts current for the entries half only: a plain
//     DELETE FROM session_search_fts WHERE rowid < BodyRowIDBase removes every
//     entries-half posting for a docid whose live content values the delete
//     can see, then one INSERT re-indexes exactly the live session_entries
//     rows with current values. The plain rowid DELETE un-indexes exactly
//     the live content values — the same token set the external-content
//     'delete' command with current values would carry — with no per-row
//     value plumbing; bodies (rid at or above BodyRowIDBase) are untouched
//     and no 'rebuild' ever runs.
//     Residual (proven, not assumed): FTS5 un-indexes by value, so postings
//     whose indexed values no longer exist in the table survive any
//     non-rebuild resync — removed terms of rows updated after V75, and
//     ghosts of rows deleted after V75. The INSERT restores findability for
//     every live row (new, updated, and untouched terms all match), the
//     repaired triggers stop new residue from accruing, ghosts stay invisible
//     to production search (the unified read joins session_entries and
//     requires e.session_id IS NOT NULL, harmonized_search.go), and only a
//     future whole-index rebuild expunges the residue. See the V76 test,
//     which pins the healed and the residual postings separately.
//  4. Drop session_entries_fts if it exists.
//  5. Leave session_search_state.needs_rebuild unchanged: the bodies half was
//     never broken, and a flag set for untrusted body postings still needs its
//     whole-index rebuild.
//
// The rowid literal below is BodyRowIDBase (schema_v62.go): SQLite DDL and
// migration SQL cannot reference a Go constant, so the split is spelled out
// where the v62 CHECK and allocation subquery spell it.
const migrationV76 = `
DROP TRIGGER IF EXISTS session_entries_ai;
DROP TRIGGER IF EXISTS session_entries_ad;
DROP TRIGGER IF EXISTS session_entries_au;
` + migrateSearchTriggerAI + `;
` + migrateSearchTriggerAD + `;
` + migrateSearchTriggerAU + `;
DELETE FROM session_search_fts WHERE rowid < 1125899906842624;
INSERT INTO session_search_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
  SELECT rowid, content_preview, tool_input, tool_output, session_id, entry_index FROM session_entries;
DROP TABLE IF EXISTS session_entries_fts;
`
