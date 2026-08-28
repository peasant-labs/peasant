package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SQL constants for session_entries write path.
const (
	sessionEntryColumnCount     = 25
	sessionEntryInsertChunkSize = 32

	sqlDeleteSessionEntries    = `DELETE FROM session_entries WHERE session_id = ?`
	sqlDeleteSessionEntriesExt = `DELETE FROM session_entries_ext WHERE session_id = ?`

	sqlInsertSessionEntryPrefix = `INSERT INTO session_entries (
    session_id, entry_index, provider, entry_type, role,
    timestamp_ms, content_preview, tokens_in, tokens_out,
    has_tool_use, tool_names_csv, has_thinking, is_error,
    raw_byte_length, tool_call_id, entry_id, parent_entry_id, extra,
    depth, parent_index, tool_input, tool_output,
    tool_kind, stop_reason, part_type
) VALUES `

	sqlSessionEntriesExist = `SELECT 1 FROM session_entries WHERE session_id = ? LIMIT 1`

	// sqlSelectTargetEntriesForSession reads the entry-annotation attachments a
	// re-index has to carry across its DELETE. Ordered so a restore is
	// deterministic and a failure names the same row every time.
	sqlSelectTargetEntriesForSession = `SELECT targets.annotation_id, targets.entry_index, targets.end_index, annotators.name
FROM annotation_target_entries targets
JOIN annotations ON annotations.id = targets.annotation_id
JOIN annotators ON annotators.id = annotations.annotator_id
WHERE targets.session_id = ?
ORDER BY targets.entry_index, targets.annotation_id`

	sqlInsertSessionEntryExt = `INSERT INTO session_entries_ext (
    session_id, entry_index, key, value_text, value_int, value_real
) VALUES (?, ?, ?, ?, ?, ?)`

	// sqlInsertSessionCommand inserts a row into session_commands.
	// session_commands has a FK to session_entries(session_id, entry_index)
	// with ON DELETE CASCADE. IndexSessionEntries also explicitly deletes
	// from session_commands during re-indexing for robustness, so callers
	// do not rely solely on the FK cascade behavior.
	sqlInsertSessionCommand = `INSERT OR REPLACE INTO session_commands (
    session_id, entry_index, command_name, command_args
) VALUES (?, ?, ?, ?)`
)

// IndexSessionEntries atomically replaces all session_entries for a session.
// Uses DELETE + INSERT within a single transaction for idempotent re-indexing.
//
// Entry-targeted annotations are carried ACROSS the replacement. They live in
// annotation_target_entries, which has no ON DELETE CASCADE and therefore has to
// be cleared by hand before the entries it references go — but clearing it and
// stopping there detaches every entry annotation from its target while the
// annotations row survives. That orphan is invisible to the existing ingest
// verification and fails the wire target-arm validation. Legacy orphans are now
// isolated and reported by push, but a re-index must not create new ones, so the
// rows are read first and re-attached to the entries that still exist afterwards.
func (s *Store) IndexSessionEntries(ctx context.Context, sessionID ingest.SessionID, entries []schema.SessionEntry) (err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	carried, err := readEntryAnnotationTargets(conn, string(sessionID))
	if err != nil {
		return fmt.Errorf("store: read annotation_target_entries for %s: %w — "+
			"what: the entry-targeted annotations of this session could not be read before re-indexing; "+
			"why: a query against annotation_target_entries failed; "+
			"user impact: session %s was NOT re-indexed, because finishing would have detached its entry annotations from their targets and made them unpublishable; "+
			"how to fix: check the analytics store is readable, then re-run peasant ingest",
			sessionID, err, sessionID)
	}

	// Delete existing entries for this session in FK-safe order.
	// annotation_target_entries lacks ON DELETE CASCADE, so delete explicitly.
	// session_commands and session_entries_ext have CASCADE but delete explicitly for robustness.
	for _, q := range []struct {
		table, sql string
	}{
		{"annotation_target_entries", `DELETE FROM annotation_target_entries WHERE session_id = ?`},
		{"session_commands", `DELETE FROM session_commands WHERE session_id = ?`},
		{"session_entries_ext", sqlDeleteSessionEntriesExt},
		{"session_entries", sqlDeleteSessionEntries},
	} {
		if err = sqlitex.ExecuteTransient(conn, q.sql, &sqlitex.ExecOptions{
			Args: []any{string(sessionID)},
		}); err != nil {
			return fmt.Errorf("store: delete %s for %s: %w — "+
				"what: failed to delete existing %s rows before re-indexing; "+
				"why: FK constraint or DB corruption; "+
				"user impact: session %s will not be re-indexed, stale data persists; "+
				"how to fix: re-run peasant ingest after upgrading; if persistent, delete the DB at ~/.local/share/peasant/peasant.db and re-ingest",
				q.table, sessionID, err, q.table, sessionID)
		}
	}

	// Insert all new entries.
	if err = insertSessionEntries(conn, entries); err != nil {
		return fmt.Errorf("store: insert session_entries for %s: %w — "+
			"what: failed to insert indexed entries; "+
			"why: schema mismatch or constraint violation; "+
			"user impact: session %s indexing aborted, partial data may exist; "+
			"how to fix: check CHECK constraints on role/entry_type columns match current schema",
			sessionID, err, sessionID)
	}
	insertExtStmt, _, err := conn.PrepareTransient(sqlInsertSessionEntryExt)
	if err != nil {
		return fmt.Errorf("store: prepare session_entry_ext insert for %s: %w", sessionID, err)
	}
	defer insertExtStmt.Finalize()
	insertCommandStmt, _, err := conn.PrepareTransient(sqlInsertSessionCommand)
	if err != nil {
		return fmt.Errorf("store: prepare session_command insert for %s: %w", sessionID, err)
	}
	defer insertCommandStmt.Finalize()
	for i := range entries {
		e := &entries[i]
		// Write known ext keys and session_commands from Extra JSON.
		if e.Extra != nil {
			extra, ok := parseEntryExtra(*e.Extra)
			if !ok {
				continue
			}
			if err = writeExtKeys(insertExtStmt, string(e.SessionID), e.EntryIndex, extra); err != nil {
				return fmt.Errorf("store: write ext keys %s[%d]: %w", sessionID, e.EntryIndex, err)
			}
			if err = writeSessionCommand(insertCommandStmt, string(e.SessionID), e.EntryIndex, extra); err != nil {
				return fmt.Errorf("store: write session command %s[%d]: %w", sessionID, e.EntryIndex, err)
			}
		}
	}

	if err = restoreEntryAnnotationTargets(conn, string(sessionID), carried, entries); err != nil {
		return err
	}

	return nil
}

func insertSessionEntries(conn *sqlite.Conn, entries []schema.SessionEntry) error {
	for start := 0; start < len(entries); start += sessionEntryInsertChunkSize {
		end := start + sessionEntryInsertChunkSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := insertSessionEntryChunk(conn, entries[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func insertSessionEntryChunk(conn *sqlite.Conn, entries []schema.SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	stmt, _, err := conn.PrepareTransient(buildSessionEntryInsertSQL(len(entries)))
	if err != nil {
		return err
	}
	defer stmt.Finalize()

	param := 1
	for i := range entries {
		bindSessionEntryParams(stmt, &entries[i], &param)
	}

	_, err = stmt.Step()
	if resetErr := stmt.Reset(); err == nil {
		err = resetErr
	}
	if clearErr := stmt.ClearBindings(); err == nil {
		err = clearErr
	}
	if err != nil {
		return fmt.Errorf("session_entry chunk [%d,%d]: %w", entries[0].EntryIndex, entries[len(entries)-1].EntryIndex, err)
	}
	return nil
}

func buildSessionEntryInsertSQL(rowCount int) string {
	var b strings.Builder
	b.Grow(len(sqlInsertSessionEntryPrefix) + rowCount*sessionEntryColumnCount*3)
	b.WriteString(sqlInsertSessionEntryPrefix)
	for row := 0; row < rowCount; row++ {
		if row > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for col := 0; col < sessionEntryColumnCount; col++ {
			if col > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('?')
		}
		b.WriteByte(')')
	}
	return b.String()
}

func bindSessionEntryParams(stmt *sqlite.Stmt, e *schema.SessionEntry, param *int) {
	stmt.BindText(nextParam(param), string(e.SessionID))
	stmt.BindInt64(nextParam(param), int64(e.EntryIndex))
	stmt.BindText(nextParam(param), e.Harness.String())
	stmt.BindText(nextParam(param), e.EntryType.String())
	stmt.BindText(nextParam(param), e.Role.String())
	bindNullableInt64(stmt, nextParam(param), e.TimestampMs)
	bindNullableString(stmt, nextParam(param), e.ContentPreview)
	bindNullableInt(stmt, nextParam(param), e.TokensIn)
	bindNullableInt(stmt, nextParam(param), e.TokensOut)
	stmt.BindInt64(nextParam(param), int64(boolToInt(e.HasToolUse)))
	bindNullableString(stmt, nextParam(param), e.ToolNamesCSV)
	stmt.BindInt64(nextParam(param), int64(boolToInt(e.HasThinking)))
	stmt.BindInt64(nextParam(param), int64(boolToInt(e.IsError)))
	bindNullableInt(stmt, nextParam(param), e.RawByteLength)
	bindNullableString(stmt, nextParam(param), e.ToolCallID)
	bindNullableString(stmt, nextParam(param), e.EntryID)
	bindNullableString(stmt, nextParam(param), e.ParentEntryID)
	bindNullableString(stmt, nextParam(param), e.Extra)
	stmt.BindInt64(nextParam(param), int64(e.Depth))
	bindNullableInt(stmt, nextParam(param), e.ParentIndex)
	bindNullableString(stmt, nextParam(param), e.ToolInput)
	bindNullableString(stmt, nextParam(param), e.ToolOutput)
	bindNullableToolCallKind(stmt, nextParam(param), e.ToolKind)
	bindNullableStopReason(stmt, nextParam(param), e.StopReason)
	bindNullableString(stmt, nextParam(param), e.PartType)
}

func nextParam(param *int) int {
	current := *param
	*param = *param + 1
	return current
}

func bindNullableString(stmt *sqlite.Stmt, param int, value *string) {
	if value == nil {
		stmt.BindNull(param)
		return
	}
	stmt.BindText(param, *value)
}

func bindNullableInt(stmt *sqlite.Stmt, param int, value *int) {
	if value == nil {
		stmt.BindNull(param)
		return
	}
	stmt.BindInt64(param, int64(*value))
}

func bindNullableInt64(stmt *sqlite.Stmt, param int, value *int64) {
	if value == nil {
		stmt.BindNull(param)
		return
	}
	stmt.BindInt64(param, *value)
}

func bindNullableToolCallKind(stmt *sqlite.Stmt, param int, value *schema.ToolCallKind) {
	if value == nil {
		stmt.BindNull(param)
		return
	}
	stmt.BindText(param, value.String())
}

func bindNullableStopReason(stmt *sqlite.Stmt, param int, value *schema.StopReason) {
	if value == nil {
		stmt.BindNull(param)
		return
	}
	stmt.BindText(param, value.String())
}

// entryAnnotationTarget is one annotation's attachment to a span of a session's
// entries. It is the only thing standing between an entry annotation and the
// orphan state that fails every push.
type entryAnnotationTarget struct {
	annotationID  string
	annotatorName string
	entryIndex    int
	endIndex      int
}

// readEntryAnnotationTargets reads the entry-annotation attachments of one
// session, so a re-index can put them back.
func readEntryAnnotationTargets(conn *sqlite.Conn, sessionID string) ([]entryAnnotationTarget, error) {
	var targets []entryAnnotationTarget
	err := sqlitex.ExecuteTransient(conn, sqlSelectTargetEntriesForSession, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			targets = append(targets, entryAnnotationTarget{
				annotationID:  stmt.ColumnText(0),
				annotatorName: stmt.ColumnText(3),
				entryIndex:    stmt.ColumnInt(1),
				endIndex:      stmt.ColumnInt(2),
			})
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return targets, nil
}

// restoreEntryAnnotationTargets re-attaches the annotations that were carried
// across the replacement, to the entries that are still there.
//
// If any entry in an attachment's half-open span no longer exists, the entire
// transaction fails. Forcing the row back would retain an annotation over
// missing content; dropping it would orphan the annotation. Rolling back
// preserves both the prior index and target until the user deliberately removes
// or recreates that annotation.
func restoreEntryAnnotationTargets(
	conn *sqlite.Conn,
	sessionID string,
	carried []entryAnnotationTarget,
	entries []schema.SessionEntry,
) error {
	if len(carried) == 0 {
		return nil
	}
	present := make(map[int]bool, len(entries))
	for i := range entries {
		present[entries[i].EntryIndex] = true
	}
	for _, target := range carried {
		missingIndex := -1
		for index := target.entryIndex; index < target.endIndex; index++ {
			if !present[index] {
				missingIndex = index
				break
			}
		}
		if missingIndex >= 0 {
			dryRun := fmt.Sprintf("peasant annotate prune %s --session %s --dry-run",
				githooks.ShellQuote(target.annotatorName), githooks.ShellQuote(sessionID))
			return fmt.Errorf("store: preserve annotation_target_entries for %s[%d,%d): the re-index no longer contains the complete span targeted by annotation %s — "+
				"what: an entry annotation could not be carried onto the replacement index; "+
				"why: entry %d disappeared from the newly indexed transcript; "+
				"user impact: the re-index was rolled back, so the previous entries and annotation target remain intact and later village pushes are not poisoned by an orphan; "+
				"how to fix: using the same global --config-dir/--data-dir overrides as this ingest, inspect the annotation with 'peasant annotate list %s'. If it no longer applies, preview the annotator-and-session-scoped cleanup with '%s', remove or recreate the affected annotation, then re-run the same peasant ingest command",
				sessionID, target.entryIndex, target.endIndex, target.annotationID, missingIndex,
				githooks.ShellQuote(sessionID), dryRun)
		}
		if err := sqlitex.ExecuteTransient(conn, sqlInsertTargetEntry, &sqlitex.ExecOptions{
			Args: []any{target.annotationID, sessionID, target.entryIndex, target.endIndex},
		}); err != nil {
			return fmt.Errorf("store: restore annotation_target_entries for %s[%d]: %w — "+
				"what: an entry annotation could not be re-attached to the entry it targets after re-indexing; "+
				"why: the insert into annotation_target_entries failed; "+
				"user impact: session %s was NOT re-indexed (the whole re-index is rolled back), because completing it would have left annotation %s with no publishable target; "+
				"how to fix: check the analytics store for constraint or corruption errors, then re-run the same peasant ingest command with the same global --config-dir/--data-dir overrides",
				sessionID, target.entryIndex, err, sessionID, target.annotationID)
		}
	}
	return nil
}

// SessionEntriesExist returns true if session_entries rows exist for the session.
func (s *Store) SessionEntriesExist(ctx context.Context, sessionID ingest.SessionID) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var exists bool
	err = sqlitex.ExecuteTransient(conn, sqlSessionEntriesExist, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID)},
		ResultFunc: func(_ *sqlite.Stmt) error { exists = true; return nil },
	})
	if err != nil {
		return false, fmt.Errorf("store: check session_entries for %s: %w", sessionID, err)
	}
	return exists, nil
}

// --- helpers ---

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// derefBoolToInt converts *bool to any: nil → nil, false → 0, true → 1.
func derefBoolToInt(p *bool) any {
	if p == nil {
		return nil
	}
	if *p {
		return 1
	}
	return 0
}

func derefString2(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// knownExtIntKeys are Extra JSON keys promoted to session_entries_ext as value_int.
var knownExtIntKeys = []string{"tokens_reasoning", "cache_read", "cache_write"}

// knownExtTextKey is the Extra JSON key promoted to session_entries_ext as value_text.
const knownExtTextKey = "model_id"

func parseEntryExtra(extraJSON string) (map[string]any, bool) {
	var extra map[string]any
	if err := json.Unmarshal([]byte(extraJSON), &extra); err != nil {
		return nil, false // malformed JSON — skip silently
	}
	return extra, true
}

// writeExtKeys writes known Extra JSON keys to session_entries_ext.
func writeExtKeys(stmt *sqlite.Stmt, sessionID string, entryIndex int, extra map[string]any) error {
	// Integer keys.
	for _, key := range knownExtIntKeys {
		v, ok := extra[key]
		if !ok {
			continue
		}
		// JSON numbers unmarshal as float64.
		fv, ok := v.(float64)
		if !ok || fv == 0 {
			continue
		}
		if err := execPreparedSessionEntryExt(stmt, sessionID, entryIndex, key, nil, int(fv)); err != nil {
			return err
		}
	}

	// Text key.
	if v, ok := extra[knownExtTextKey]; ok {
		sv, ok := v.(string)
		if ok && sv != "" {
			if err := execPreparedSessionEntryExt(stmt, sessionID, entryIndex, knownExtTextKey, &sv, 0); err != nil {
				return err
			}
		}
	}

	return nil
}

func execPreparedSessionEntryExt(stmt *sqlite.Stmt, sessionID string, entryIndex int, key string, valueText *string, valueInt int) error {
	stmt.BindText(1, sessionID)
	stmt.BindInt64(2, int64(entryIndex))
	stmt.BindText(3, key)
	bindNullableString(stmt, 4, valueText)
	if valueText == nil {
		stmt.BindInt64(5, int64(valueInt))
	} else {
		stmt.BindNull(5)
	}
	stmt.BindNull(6)

	_, err := stmt.Step()
	if resetErr := stmt.Reset(); err == nil {
		err = resetErr
	}
	if clearErr := stmt.ClearBindings(); err == nil {
		err = clearErr
	}
	return err
}

// writeSessionCommand checks the Extra JSON for a "command_name" key and, if found,
// inserts a row into session_commands. The session_entries row must already be
// inserted (FK constraint) when this is called.
// "command_args" is optional — nil is stored when absent.
func writeSessionCommand(stmt *sqlite.Stmt, sessionID string, entryIndex int, extra map[string]any) error {
	cmdNameVal, ok := extra["command_name"]
	if !ok {
		return nil // no command_name key — not a skill invocation entry
	}
	cmdName, ok := cmdNameVal.(string)
	if !ok || cmdName == "" {
		return nil
	}

	var cmdArgs *string
	if v, ok := extra["command_args"]; ok {
		if sv, ok := v.(string); ok && sv != "" {
			cmdArgs = &sv
		}
	}

	stmt.BindText(1, sessionID)
	stmt.BindInt64(2, int64(entryIndex))
	stmt.BindText(3, cmdName)
	bindNullableString(stmt, 4, cmdArgs)
	_, err := stmt.Step()
	if resetErr := stmt.Reset(); err == nil {
		err = resetErr
	}
	if clearErr := stmt.ClearBindings(); err == nil {
		err = clearErr
	}
	return err
}
