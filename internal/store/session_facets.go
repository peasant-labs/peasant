package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The session workflow facets, derived at query time from rows the store
// already holds.
//
// sessionFacetSQL is the one spelling of each rule. The --has filter and the
// facets a list row reports both render it, so a row can never report a facet
// the filter would not select. Every rule correlates on the alias s.
//
// A session's entries live in one of two homes, chosen the way the routing shim
// chooses them: a session whose active generation exists reads its partition-0
// body rows, and every other session reads the session_entries mirror. Rules
// that read entries go through sessionEntryExistsSQL, which spells that
// dispatch once. Native sessions write nothing to session_commands either,
// because that table is filled by the mirror writer.

// sqlSessionIsNative renders the shim's dispatch: the session reads through
// the partition-0 body rows.
const sqlSessionIsNative = `(s.active_generation_id IS NOT NULL AND EXISTS (
  SELECT 1 FROM session_generations g
   WHERE g.session_id = s.session_id AND g.generation_id = s.active_generation_id
))`

// sqlSessionNativeEntries renders the FROM and WHERE of the active generation's
// partition-0 body rows, aliased e.
const sqlSessionNativeEntries = `session_generation_entries ge
  JOIN session_entry_bodies e
    ON e.session_id = ge.session_id AND e.body_digest = ge.body_digest
 WHERE ge.session_id = s.session_id
   AND ge.generation_id = s.active_generation_id
   AND ge.partition_id = 0`

// sessionEntryExistsSQL renders whether an entry of the session satisfies a
// predicate over the alias e. nativeOnly carries the clause that reads a column
// the mirror does not have; it is empty for a rule both homes answer alike.
func sessionEntryExistsSQL(predicate, nativeOnly string) string {
	native := predicate
	if nativeOnly != "" {
		native = "(" + predicate + " OR " + nativeOnly + ")"
	}
	return `(EXISTS (SELECT 1 FROM ` + sqlSessionNativeEntries + ` AND ` + native + `)
 OR (NOT ` + sqlSessionIsNative + ` AND EXISTS (
  SELECT 1 FROM session_entries e WHERE e.session_id = s.session_id AND ` + predicate + `)))`
}

// toolNamedSQL renders whether an entry's tool list names one of the exact
// tools. GLOB rather than LIKE: it is case-sensitive and an underscore is
// literal. The comma wrap keeps Task from matching TaskCreate.
func toolNamedSQL(names ...string) string {
	clauses := make([]string, len(names))
	for i, name := range names {
		clauses[i] = `',' || e.tool_names_csv || ',' GLOB '*,` + name + `,*'`
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

// sqlSessionHasChild reads the one parent answer in reverse. The subquery is
// uncorrelated, so SQLite runs it once however many sessions probe it.
var sqlSessionHasChild = `s.session_id IN (SELECT ` + LogicalParentSQL("child") + ` FROM sessions child)`

// sqlEntryRunsInBackground reads run_in_background from a tool call's input.
// A stored input need not be valid JSON, and json_type raises on one that is
// not, so the guard must run first: CASE fixes the order an AND would not.
const sqlEntryRunsInBackground = `CASE WHEN e.tool_input IS NOT NULL
   AND instr(e.tool_input, '"run_in_background"') > 0
   AND json_valid(e.tool_input)
  THEN json_type(e.tool_input, '$.run_in_background') = 'true' ELSE 0 END`

// sqlEntryIsClearCommand matches the system entry /clear is stored as. The
// indexer files built-in commands as system entries with no command row, so
// the entry text is the only record of one. It also files any user message that
// quotes the tag as system, so the tag must lead the text, alone or after its
// <command-message> sibling.
const sqlEntryIsClearCommand = `(e.role = 'system' AND (
     ltrim(e.content_preview, char(32, 9, 10, 13)) GLOB '<command-name>/clear</command-name>*'
  OR ltrim(e.content_preview, char(32, 9, 10, 13)) GLOB '<command-message>*</command-message>*<command-name>/clear</command-name>*'))`

// sqlEntryIsCompaction matches the compaction markers: Claude's boundary, a
// Codex control record, and an OpenCode part.
const sqlEntryIsCompaction = `e.part_type IN ('compact-boundary', 'compacted', 'compaction')`

// sqlEntryIsGeneratedSummary matches the compaction summary of a native Codex
// or OpenCode session, which carries no part type. Nothing else writes this
// origin, and a failed compaction is stored as a system control instead.
const sqlEntryIsGeneratedSummary = `e.prov_origin = 'generated_summary'`

// sqlEntryIsCommandInvocation matches a native entry carrying the command
// metadata the indexer writes for a custom command. It is the native record of
// what a mirror session keeps in session_commands.
const sqlEntryIsCommandInvocation = `CASE WHEN instr(COALESCE(e.extra, e.extra_verbatim), '"command_name"') > 0
   AND json_valid(COALESCE(e.extra, e.extra_verbatim))
  THEN json_type(COALESCE(e.extra, e.extra_verbatim), '$.command_name') = 'text' ELSE 0 END`

// sqlSessionSawSeveralModels compares the assistant model observations of the
// session's home. Claude's synthetic marker is left out: no model produced
// that record. An OpenCode model switch needs no rule of its own, because the
// indexer lands it on the next assistant turn as an observation, and a native
// OpenCode switch is stored with no marker that tells it from an agent switch.
var sqlSessionSawSeveralModels = `((SELECT COUNT(DISTINCT e.model_id) FROM ` + sqlSessionNativeEntries + `
   AND e.role = 'assistant' AND e.model_id IS NOT NULL AND e.model_id <> '` + defaults.ClaudeSyntheticModelID + `') > 1
 OR (NOT ` + sqlSessionIsNative + ` AND (SELECT COUNT(DISTINCT x.value_text) FROM session_entries_ext x
   WHERE x.session_id = s.session_id AND x.key = '` + knownExtTextKey + `'
     AND x.value_text <> '` + defaults.ClaudeSyntheticModelID + `') > 1))`

// sqlSessionIsPartial reads the active generation's own diagnostics, so an
// older partial generation does not mark a session whose active one is whole.
const sqlSessionIsPartial = `EXISTS (
  SELECT 1 FROM session_generations g
   WHERE g.session_id = s.session_id AND g.generation_id = s.active_generation_id
     AND g.diagnostics_partial = 1
)`

// sessionFacetSQL renders one facet as a boolean expression. A value outside
// the closed set holds for no session.
func sessionFacetSQL(facet defaults.SessionWorkflowFacet) string {
	switch facet {
	case defaults.SessionFacetSubagent:
		return `(` + sqlSessionHasChild + ` OR ` + sessionEntryExistsSQL(toolNamedSQL("Task", "Agent"), "") + `)`
	case defaults.SessionFacetSkill:
		return `((NOT ` + sqlSessionIsNative + ` AND EXISTS (SELECT 1 FROM session_commands sc WHERE sc.session_id = s.session_id))
 OR ` + sessionEntryExistsSQL(toolNamedSQL("Skill", "skill"), sqlEntryIsCommandInvocation) + `)`
	case defaults.SessionFacetMCP:
		return sessionEntryExistsSQL(`',' || e.tool_names_csv GLOB '*,mcp__*'`, "")
	case defaults.SessionFacetCompact:
		return sessionEntryExistsSQL(sqlEntryIsCompaction, sqlEntryIsGeneratedSummary)
	case defaults.SessionFacetClear:
		return sessionEntryExistsSQL(sqlEntryIsClearCommand, "")
	case defaults.SessionFacetModelSwitch:
		return sqlSessionSawSeveralModels
	case defaults.SessionFacetBackground:
		return sessionEntryExistsSQL(sqlEntryRunsInBackground, "")
	case defaults.SessionFacetPartial:
		return sqlSessionIsPartial
	default:
		return "0"
	}
}

// SessionWorkflowFacets answers which facets hold for each named session,
// through the same rendered rules the Facets filter applies. Each list is in
// defaults.AllSessionWorkflowFacets order. A session no facet holds for, or one
// the store does not hold, has no entry.
func (s *Store) SessionWorkflowFacets(ctx context.Context, sessionIDs []string) (map[string][]defaults.SessionWorkflowFacet, error) {
	facets := make(map[string][]defaults.SessionWorkflowFacet, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return facets, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to read session facets: %w; no facets were read; retry the command", err)
	}
	defer s.pool.Put(conn)

	columns := make([]string, len(defaults.AllSessionWorkflowFacets))
	for i, facet := range defaults.AllSessionWorkflowFacets {
		columns[i] = sessionFacetSQL(facet)
	}
	selectFacets := `SELECT s.session_id, ` + strings.Join(columns, ", ") + ` FROM sessions s WHERE s.session_id IN (?`
	for start := 0; start < len(sessionIDs); start += indexFormatReadBatchSize {
		batch := sessionIDs[start:min(start+indexFormatReadBatchSize, len(sessionIDs))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		err := sqlitex.ExecuteTransient(conn, selectFacets+strings.Repeat(",?", len(batch)-1)+`)`, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				var held []defaults.SessionWorkflowFacet
				for i, facet := range defaults.AllSessionWorkflowFacets {
					if stmt.ColumnInt(i+1) != 0 {
						held = append(held, facet)
					}
				}
				if held != nil {
					facets[stmt.ColumnText(0)] = held
				}
				return nil
			},
		})
		if err != nil {
			return nil, fmt.Errorf("store: read session facets: %w; no facets were read; run peasant harvest verify and retry", err)
		}
	}
	return facets, nil
}
