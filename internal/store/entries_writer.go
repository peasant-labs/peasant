package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"golang.org/x/crypto/sha3"
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

	sqlSelectTargetEntrySpansForSession = `SELECT entry_index, end_index
FROM annotation_target_entries
WHERE session_id = ?
ORDER BY entry_index, annotation_id`

	sqlSelectSessionEntryAnchors = `SELECT entry_index, entry_id, tool_call_id, entry_type, role, part_type, content_preview
FROM session_entries
WHERE session_id = ?
ORDER BY entry_index`

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

	sqlSelectSessionCommandsForSession = `SELECT entry_index, command_name, command_args
FROM session_commands
WHERE session_id = ?
ORDER BY entry_index`

	sqlSelectSessionEntriesHash = `SELECT session_entries_hash FROM sessions WHERE session_id = ?`

	sqlSetSessionEntriesHash = `UPDATE sessions SET session_entries_hash = ? WHERE session_id = ?`
)

type sessionEntryWriteOutcome struct {
	skipped            bool
	sessionEntriesHash string
	stats              ingest.SessionEntryWriteStats
}

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

	stmts := newSessionEntryWriteStatements(conn)
	defer func() {
		err = errors.Join(err, stmts.Close())
	}()
	_, err = indexSessionEntriesOnConn(conn, sessionID, entries, stmts)
	return err
}

// IndexSessionEntryBatch writes multiple session entry replacements in one
// outer transaction. Each session runs under a savepoint, so one bad session can
// roll back without discarding later successful sessions in the same batch.
func (s *Store) IndexSessionEntryBatch(ctx context.Context, writes []ingest.SessionEntryWrite) []ingest.SessionEntryWriteResult {
	results := make([]ingest.SessionEntryWriteResult, len(writes))
	for i := range writes {
		results[i].SessionID = writes[i].SessionID
	}
	if len(writes) == 0 {
		return results
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		for i := range results {
			results[i].Err = fmt.Errorf("store: take connection for session entry batch: %w", err)
		}
		return results
	}
	defer s.pool.Put(conn)

	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	txnOpen := true
	stmts := newSessionEntryWriteStatements(conn)
	defer func() {
		if txnOpen {
			txnErr = errors.Join(txnErr, stmts.Close())
			endFn(&txnErr)
		}
	}()

	for i := range writes {
		if results[i].Err != nil {
			continue
		}
		outcome, err, fatal := indexSessionEntryWriteSavepoint(conn, writes[i], stmts)
		results[i].Stats = outcome.stats
		if err != nil {
			results[i].Err = err
			if fatal {
				txnErr = err
				break
			}
			continue
		}
		results[i].Written = true
		results[i].Skipped = outcome.skipped
	}

	txnErr = errors.Join(txnErr, stmts.Close())
	endFn(&txnErr)
	txnOpen = false
	if txnErr != nil {
		commitErr := fmt.Errorf("store: commit session entry batch: %w", txnErr)
		for i := range results {
			if results[i].Written {
				results[i].Written = false
				results[i].Err = commitErr
			}
			if results[i].Err == nil {
				results[i].Err = commitErr
			}
		}
	}
	return results
}

func indexSessionEntryWriteSavepoint(conn *sqlite.Conn, write ingest.SessionEntryWrite, stmts *sessionEntryWriteStatements) (sessionEntryWriteOutcome, error, bool) {
	const savepointName = "session_entry_batch_item"
	if err := sqlitex.ExecuteTransient(conn, "SAVEPOINT "+savepointName, nil); err != nil {
		return sessionEntryWriteOutcome{}, fmt.Errorf("store: start session entry savepoint for %s: %w", write.SessionID, err), true
	}

	outcome, err := indexSessionEntriesOnConn(conn, write.SessionID, write.Entries, stmts)
	if err != nil {
		rollbackErr, fatal := rollbackSessionEntrySavepoint(conn, savepointName, err, write.SessionID)
		return outcome, rollbackErr, fatal
	}
	if write.IndexVersion > 0 {
		if err := updateIndexStateWithSessionEntriesHashOnConn(conn, write.SessionID, write.IndexVersion, write.IndexedAtMs, outcome.sessionEntriesHash); err != nil {
			rollbackErr, fatal := rollbackSessionEntrySavepoint(conn, savepointName, fmt.Errorf("store: update index state for %s: %w", write.SessionID, err), write.SessionID)
			return outcome, rollbackErr, fatal
		}
	}
	if err := sqlitex.ExecuteTransient(conn, "RELEASE SAVEPOINT "+savepointName, nil); err != nil {
		return outcome, fmt.Errorf("store: release session entry savepoint for %s: %w", write.SessionID, err), true
	}
	return outcome, nil, false
}

func rollbackSessionEntrySavepoint(conn *sqlite.Conn, savepointName string, cause error, sessionID ingest.SessionID) (error, bool) {
	rollbackErr := sqlitex.ExecuteTransient(conn, "ROLLBACK TO SAVEPOINT "+savepointName, nil)
	releaseErr := sqlitex.ExecuteTransient(conn, "RELEASE SAVEPOINT "+savepointName, nil)
	if rollbackErr != nil || releaseErr != nil {
		errs := []error{fmt.Errorf("store: session entry savepoint failed for %s: %w", sessionID, cause)}
		if rollbackErr != nil {
			errs = append(errs, fmt.Errorf("store: rollback session entry savepoint for %s: %w", sessionID, rollbackErr))
		}
		if releaseErr != nil {
			errs = append(errs, fmt.Errorf("store: release session entry savepoint for %s: %w", sessionID, releaseErr))
		}
		return errors.Join(errs...), true
	}
	return cause, false
}

func indexSessionEntriesOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, entries []schema.SessionEntry, stmts *sessionEntryWriteStatements) (sessionEntryWriteOutcome, error) {
	outcome := sessionEntryWriteOutcome{}
	sessionEntriesHash, err := computeSessionEntriesHash(entries)
	if err != nil {
		return outcome, fmt.Errorf("store: compute session_entries_hash for %s: %w", sessionID, err)
	}
	outcome.sessionEntriesHash = sessionEntriesHash

	storedHash, hasStoredHash, err := readStoredSessionEntriesHash(conn, string(sessionID))
	if err != nil {
		return outcome, fmt.Errorf("store: read session_entries_hash for %s: %w", sessionID, err)
	}
	if hasStoredHash && storedHash == sessionEntriesHash {
		outcome.stats.HashMatches++
		derivedMatch, err := sessionEntryDerivedTablesMatch(conn, string(sessionID), entries)
		if err != nil {
			return outcome, fmt.Errorf("store: compare existing session entry projections for %s: %w", sessionID, err)
		}
		if !derivedMatch {
			outcome.stats.ProjectionRepairRewrites++
		} else {
			annotationsValid, err := entryAnnotationTargetSpansMatchEntries(conn, string(sessionID), entries)
			if err != nil {
				return outcome, fmt.Errorf("store: compare existing session entry annotation spans for %s: %w", sessionID, err)
			}
			if annotationsValid {
				outcome.skipped = true
				outcome.stats.SkippedByHash++
				return outcome, nil
			}
		}
	} else if hasStoredHash {
		outcome.stats.HashMisses++
	}
	if !hasStoredHash {
		outcome.stats.FallbackCompares++
		storedEntries, err := readStoredSessionEntries(conn, string(sessionID))
		if err != nil {
			return outcome, fmt.Errorf("store: compare existing session entries for %s: %w", sessionID, err)
		}
		if sessionEntriesEqual(storedEntries, entries) {
			derivedMatch, err := sessionEntryDerivedTablesMatch(conn, string(sessionID), entries)
			if err != nil {
				return outcome, fmt.Errorf("store: compare existing session entry projections for %s: %w", sessionID, err)
			}
			if !derivedMatch {
				outcome.stats.ProjectionRepairRewrites++
			} else {
				annotationsValid, err := entryAnnotationTargetsMatchEntries(conn, string(sessionID), entries)
				if err != nil {
					return outcome, fmt.Errorf("store: compare existing session entry annotations for %s: %w", sessionID, err)
				}
				if annotationsValid {
					if err := setSessionEntriesHashOnConn(conn, string(sessionID), sessionEntriesHash); err != nil {
						return outcome, fmt.Errorf("store: set session_entries_hash for unchanged session %s: %w", sessionID, err)
					}
					outcome.skipped = true
					outcome.stats.SkippedByCompare++
					return outcome, nil
				}
			}
		}
	}

	carried, err := readEntryAnnotationTargets(conn, string(sessionID))
	if err != nil {
		return outcome, fmt.Errorf("store: read annotation_target_entries for %s: %w — "+
			"what: the entry-targeted annotations of this session could not be read before re-indexing; "+
			"why: a query against annotation_target_entries failed; "+
			"user impact: session %s was NOT re-indexed, because finishing would have detached its entry annotations from their targets and made them unpublishable; "+
			"how to fix: check the analytics store is readable, then re-run peasant ingest",
			sessionID, err, sessionID)
	}
	outcome.stats.AnnotationTargetsCarried += len(carried)

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
			return outcome, fmt.Errorf("store: delete %s for %s: %w — "+
				"what: failed to delete existing %s rows before re-indexing; "+
				"why: FK constraint or DB corruption; "+
				"user impact: session %s will not be re-indexed, stale data persists; "+
				"how to fix: re-run peasant ingest after upgrading; if persistent, delete the DB at ~/.local/share/peasant/peasant.db and re-ingest",
				q.table, sessionID, err, q.table, sessionID)
		}
	}

	// Insert all new entries.
	if err = insertSessionEntries(stmts, entries); err != nil {
		return outcome, fmt.Errorf("store: insert session_entries for %s: %w — "+
			"what: failed to insert indexed entries; "+
			"why: schema mismatch or constraint violation; "+
			"user impact: session %s indexing aborted, partial data may exist; "+
			"how to fix: check CHECK constraints on role/entry_type columns match current schema",
			sessionID, err, sessionID)
	}
	insertExtStmt, err := stmts.EntryExt()
	if err != nil {
		return outcome, fmt.Errorf("store: prepare session_entry_ext insert for %s: %w", sessionID, err)
	}
	insertCommandStmt, err := stmts.SessionCommand()
	if err != nil {
		return outcome, fmt.Errorf("store: prepare session_command insert for %s: %w", sessionID, err)
	}
	for i := range entries {
		e := &entries[i]
		// Write known ext keys and session_commands from Extra JSON.
		if e.Extra != nil {
			extra, ok := parseEntryExtra(*e.Extra)
			if !ok {
				continue
			}
			if err = writeExtKeys(insertExtStmt, string(e.SessionID), e.EntryIndex, extra); err != nil {
				return outcome, fmt.Errorf("store: write ext keys %s[%d]: %w", sessionID, e.EntryIndex, err)
			}
			if err = writeSessionCommand(insertCommandStmt, string(e.SessionID), e.EntryIndex, extra); err != nil {
				return outcome, fmt.Errorf("store: write session command %s[%d]: %w", sessionID, e.EntryIndex, err)
			}
		}
	}

	remappedTargets, err := restoreEntryAnnotationTargets(conn, string(sessionID), carried, entries)
	if err != nil {
		var refused *annotationRemapRefusedError
		if errors.As(err, &refused) {
			outcome.stats.AnnotationRollbackFailures++
		}
		return outcome, err
	}
	outcome.stats.AnnotationTargetsRemapped += remappedTargets
	if err = setSessionEntriesHashOnConn(conn, string(sessionID), sessionEntriesHash); err != nil {
		return outcome, fmt.Errorf("store: set session_entries_hash for %s: %w", sessionID, err)
	}

	outcome.stats.Rewrites++
	return outcome, nil
}

type sessionEntryExtRow struct {
	entryIndex int
	key        string
	valueText  *string
	valueInt   *int
	valueReal  *float64
}

type sessionCommandRow struct {
	entryIndex  int
	commandName string
	commandArgs *string
}

func sessionEntryTablesMatch(conn *sqlite.Conn, sessionID string, entries []schema.SessionEntry) (bool, error) {
	storedEntries, err := readStoredSessionEntries(conn, sessionID)
	if err != nil {
		return false, err
	}
	if !sessionEntriesEqual(storedEntries, entries) {
		return false, nil
	}
	return sessionEntryDerivedTablesAndAnnotationsMatch(conn, sessionID, entries)
}

func sessionEntryHashDependenciesMatch(conn *sqlite.Conn, sessionID string, entries []schema.SessionEntry) (bool, error) {
	matched, err := sessionEntryDerivedTablesMatch(conn, sessionID, entries)
	if err != nil || !matched {
		return matched, err
	}
	return entryAnnotationTargetSpansMatchEntries(conn, sessionID, entries)
}

func sessionEntryDerivedTablesAndAnnotationsMatch(conn *sqlite.Conn, sessionID string, entries []schema.SessionEntry) (bool, error) {
	matched, err := sessionEntryDerivedTablesMatch(conn, sessionID, entries)
	if err != nil || !matched {
		return matched, err
	}

	return entryAnnotationTargetsMatchEntries(conn, sessionID, entries)
}

func sessionEntryDerivedTablesMatch(conn *sqlite.Conn, sessionID string, entries []schema.SessionEntry) (bool, error) {
	storedExt, err := readStoredSessionEntryExtRows(conn, sessionID)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(storedExt, expectedSessionEntryExtRows(entries)) {
		return false, nil
	}

	storedCommands, err := readStoredSessionCommandRows(conn, sessionID)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(storedCommands, expectedSessionCommandRows(entries)) {
		return false, nil
	}

	return true, nil
}

func readStoredSessionEntriesHash(conn *sqlite.Conn, sessionID string) (string, bool, error) {
	var hash string
	var hasHash bool
	err := sqlitex.ExecuteTransient(conn, sqlSelectSessionEntriesHash, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				hash = stmt.ColumnText(0)
				hasHash = hash != ""
			}
			return nil
		},
	})
	if err != nil {
		return "", false, err
	}
	return hash, hasHash, nil
}

func setSessionEntriesHashOnConn(conn *sqlite.Conn, sessionID string, hash string) error {
	return sqlitex.ExecuteTransient(conn, sqlSetSessionEntriesHash, &sqlitex.ExecOptions{Args: []any{hash, sessionID}})
}

type sessionEntriesHashDocument struct {
	Domain   string                         `json:"domain"`
	Entries  []sessionEntriesHashEntry      `json:"entries"`
	Ext      []sessionEntriesHashExtRow     `json:"ext"`
	Commands []sessionEntriesHashCommandRow `json:"commands"`
}

type sessionEntriesHashEntry struct {
	SessionID      string               `json:"session_id"`
	EntryIndex     int                  `json:"entry_index"`
	Harness        string               `json:"provider"`
	EntryType      string               `json:"entry_type"`
	Role           string               `json:"role"`
	TimestampMs    *int64               `json:"timestamp_ms"`
	ContentPreview *string              `json:"content_preview"`
	TokensIn       *int                 `json:"tokens_in"`
	TokensOut      *int                 `json:"tokens_out"`
	HasToolUse     bool                 `json:"has_tool_use"`
	ToolNamesCSV   *string              `json:"tool_names_csv"`
	HasThinking    bool                 `json:"has_thinking"`
	IsError        bool                 `json:"is_error"`
	RawByteLength  *int                 `json:"raw_byte_length"`
	ToolCallID     *string              `json:"tool_call_id"`
	EntryID        *string              `json:"entry_id"`
	ParentEntryID  *string              `json:"parent_entry_id"`
	Extra          *string              `json:"extra"`
	Depth          int                  `json:"depth"`
	ParentIndex    *int                 `json:"parent_index"`
	ToolInput      *string              `json:"tool_input"`
	ToolOutput     *string              `json:"tool_output"`
	ToolKind       *schema.ToolCallKind `json:"tool_kind"`
	StopReason     *schema.StopReason   `json:"stop_reason"`
	PartType       *string              `json:"part_type"`
}

type sessionEntriesHashExtRow struct {
	EntryIndex int      `json:"entry_index"`
	Key        string   `json:"key"`
	ValueText  *string  `json:"value_text"`
	ValueInt   *int     `json:"value_int"`
	ValueReal  *float64 `json:"value_real"`
}

type sessionEntriesHashCommandRow struct {
	EntryIndex  int     `json:"entry_index"`
	CommandName string  `json:"command_name"`
	CommandArgs *string `json:"command_args"`
}

func computeSessionEntriesHash(entries []schema.SessionEntry) (string, error) {
	ordered := orderedSessionEntries(entries)
	doc := sessionEntriesHashDocument{
		Domain:   "peasant.session_entries.v1",
		Entries:  make([]sessionEntriesHashEntry, 0, len(ordered)),
		Ext:      hashExtRows(expectedSessionEntryExtRows(ordered)),
		Commands: hashCommandRows(expectedSessionCommandRows(ordered)),
	}
	for i := range ordered {
		doc.Entries = append(doc.Entries, sessionEntriesHashEntryFromEntry(&ordered[i]))
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	h := sha3.New256()
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orderedSessionEntries(entries []schema.SessionEntry) []schema.SessionEntry {
	ordered := append([]schema.SessionEntry(nil), entries...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].EntryIndex < ordered[j].EntryIndex
	})
	return ordered
}

func sessionEntriesHashEntryFromEntry(entry *schema.SessionEntry) sessionEntriesHashEntry {
	return sessionEntriesHashEntry{
		SessionID:      string(entry.SessionID),
		EntryIndex:     entry.EntryIndex,
		Harness:        entry.Harness.String(),
		EntryType:      entry.EntryType.String(),
		Role:           entry.Role.String(),
		TimestampMs:    entry.TimestampMs,
		ContentPreview: entry.ContentPreview,
		TokensIn:       entry.TokensIn,
		TokensOut:      entry.TokensOut,
		HasToolUse:     entry.HasToolUse,
		ToolNamesCSV:   entry.ToolNamesCSV,
		HasThinking:    entry.HasThinking,
		IsError:        entry.IsError,
		RawByteLength:  entry.RawByteLength,
		ToolCallID:     entry.ToolCallID,
		EntryID:        entry.EntryID,
		ParentEntryID:  entry.ParentEntryID,
		Extra:          entry.Extra,
		Depth:          entry.Depth,
		ParentIndex:    entry.ParentIndex,
		ToolInput:      entry.ToolInput,
		ToolOutput:     entry.ToolOutput,
		ToolKind:       entry.ToolKind,
		StopReason:     entry.StopReason,
		PartType:       entry.PartType,
	}
}

func hashExtRows(rows []sessionEntryExtRow) []sessionEntriesHashExtRow {
	out := make([]sessionEntriesHashExtRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionEntriesHashExtRow{
			EntryIndex: row.entryIndex,
			Key:        row.key,
			ValueText:  row.valueText,
			ValueInt:   row.valueInt,
			ValueReal:  row.valueReal,
		})
	}
	return out
}

func hashCommandRows(rows []sessionCommandRow) []sessionEntriesHashCommandRow {
	out := make([]sessionEntriesHashCommandRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionEntriesHashCommandRow{
			EntryIndex:  row.entryIndex,
			CommandName: row.commandName,
			CommandArgs: row.commandArgs,
		})
	}
	return out
}

func entryAnnotationTargetsMatchEntries(conn *sqlite.Conn, sessionID string, entries []schema.SessionEntry) (bool, error) {
	targets, err := readEntryAnnotationTargets(conn, sessionID)
	if err != nil {
		return false, err
	}
	if len(targets) == 0 {
		return true, nil
	}
	present := make(map[int]bool, len(entries))
	newAnchors := make([]entryTargetAnchor, 0, len(entries))
	for i := range entries {
		present[entries[i].EntryIndex] = true
		newAnchors = append(newAnchors, entryTargetAnchorFromEntry(&entries[i]))
	}
	for _, target := range targets {
		if missingSpanIndex(target, present) >= 0 {
			return false, nil
		}
		if !entryAnnotationSpanStillMatches(target, newAnchors) {
			return false, nil
		}
	}
	return true, nil
}

func entryAnnotationTargetSpansMatchEntries(conn *sqlite.Conn, sessionID string, entries []schema.SessionEntry) (bool, error) {
	targets, err := readEntryAnnotationTargetSpans(conn, sessionID)
	if err != nil {
		return false, err
	}
	if len(targets) == 0 {
		return true, nil
	}
	present := make(map[int]bool, len(entries))
	for i := range entries {
		present[entries[i].EntryIndex] = true
	}
	for _, target := range targets {
		if missingSpanIndex(target, present) >= 0 {
			return false, nil
		}
	}
	return true, nil
}

func readStoredSessionEntries(conn *sqlite.Conn, sessionID string) ([]schema.SessionEntry, error) {
	entries := []schema.SessionEntry(nil)
	err := sqlitex.ExecuteTransient(conn, sqlListEntries, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entries = append(entries, scanSessionEntry(stmt))
			return nil
		},
	})
	return entries, err
}

func sessionEntriesEqual(stored []schema.SessionEntry, entries []schema.SessionEntry) bool {
	if len(stored) != len(entries) {
		return false
	}
	expected := append([]schema.SessionEntry(nil), entries...)
	sort.SliceStable(expected, func(i, j int) bool {
		return expected[i].EntryIndex < expected[j].EntryIndex
	})
	return reflect.DeepEqual(stored, expected)
}

func readStoredSessionEntryExtRows(conn *sqlite.Conn, sessionID string) ([]sessionEntryExtRow, error) {
	rows := []sessionEntryExtRow(nil)
	err := sqlitex.ExecuteTransient(conn, sqlListEntriesExt, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, sessionEntryExtRow{
				entryIndex: stmt.ColumnInt(0),
				key:        stmt.ColumnText(1),
				valueText:  nullableColumnText(stmt, 2),
				valueInt:   nullableColumnInt(stmt, 3),
				valueReal:  nullableColumnFloat(stmt, 4),
			})
			return nil
		},
	})
	return rows, err
}

func expectedSessionEntryExtRows(entries []schema.SessionEntry) []sessionEntryExtRow {
	rows := []sessionEntryExtRow(nil)
	for i := range entries {
		extra := parsedEntryExtra(&entries[i])
		if extra == nil {
			continue
		}
		for _, key := range knownExtIntKeys {
			v, ok := extra[key]
			if !ok {
				continue
			}
			fv, ok := v.(float64)
			if !ok || fv == 0 {
				continue
			}
			iv := int(fv)
			rows = append(rows, sessionEntryExtRow{entryIndex: entries[i].EntryIndex, key: key, valueInt: &iv})
		}
		if v, ok := extra[knownExtTextKey]; ok {
			sv, ok := v.(string)
			if ok && sv != "" {
				rows = append(rows, sessionEntryExtRow{entryIndex: entries[i].EntryIndex, key: knownExtTextKey, valueText: &sv})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].entryIndex != rows[j].entryIndex {
			return rows[i].entryIndex < rows[j].entryIndex
		}
		return rows[i].key < rows[j].key
	})
	return rows
}

func readStoredSessionCommandRows(conn *sqlite.Conn, sessionID string) ([]sessionCommandRow, error) {
	rows := []sessionCommandRow(nil)
	err := sqlitex.ExecuteTransient(conn, sqlSelectSessionCommandsForSession, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, sessionCommandRow{
				entryIndex:  stmt.ColumnInt(0),
				commandName: stmt.ColumnText(1),
				commandArgs: nullableColumnText(stmt, 2),
			})
			return nil
		},
	})
	return rows, err
}

func expectedSessionCommandRows(entries []schema.SessionEntry) []sessionCommandRow {
	rows := []sessionCommandRow(nil)
	for i := range entries {
		extra := parsedEntryExtra(&entries[i])
		if extra == nil {
			continue
		}
		cmdNameVal, ok := extra["command_name"]
		if !ok {
			continue
		}
		cmdName, ok := cmdNameVal.(string)
		if !ok || cmdName == "" {
			continue
		}
		row := sessionCommandRow{entryIndex: entries[i].EntryIndex, commandName: cmdName}
		if v, ok := extra["command_args"]; ok {
			if sv, ok := v.(string); ok && sv != "" {
				row.commandArgs = &sv
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].entryIndex < rows[j].entryIndex
	})
	return rows
}

func parsedEntryExtra(entry *schema.SessionEntry) map[string]any {
	if entry.Extra == nil {
		return nil
	}
	extra, ok := parseEntryExtra(*entry.Extra)
	if !ok {
		return nil
	}
	return extra
}

func nullableColumnText(stmt *sqlite.Stmt, col int) *string {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return nil
	}
	v := stmt.ColumnText(col)
	return &v
}

func nullableColumnInt(stmt *sqlite.Stmt, col int) *int {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return nil
	}
	v := stmt.ColumnInt(col)
	return &v
}

func nullableColumnFloat(stmt *sqlite.Stmt, col int) *float64 {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return nil
	}
	v := stmt.ColumnFloat(col)
	return &v
}

type sessionEntryWriteStatements struct {
	conn             *sqlite.Conn
	entryInsertStmts map[int]*sqlite.Stmt
	extStmt          *sqlite.Stmt
	commandStmt      *sqlite.Stmt
}

func newSessionEntryWriteStatements(conn *sqlite.Conn) *sessionEntryWriteStatements {
	return &sessionEntryWriteStatements{conn: conn, entryInsertStmts: map[int]*sqlite.Stmt{}}
}

func (stmts *sessionEntryWriteStatements) Close() error {
	if stmts == nil {
		return nil
	}
	var err error
	for rowCount, stmt := range stmts.entryInsertStmts {
		if stmt == nil {
			continue
		}
		if closeErr := stmt.Finalize(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("finalize session_entries insert statement for %d row(s): %w", rowCount, closeErr))
		}
		delete(stmts.entryInsertStmts, rowCount)
	}
	if stmts.extStmt != nil {
		if closeErr := stmts.extStmt.Finalize(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("finalize session_entries_ext insert statement: %w", closeErr))
		}
		stmts.extStmt = nil
	}
	if stmts.commandStmt != nil {
		if closeErr := stmts.commandStmt.Finalize(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("finalize session_commands insert statement: %w", closeErr))
		}
		stmts.commandStmt = nil
	}
	return err
}

func (stmts *sessionEntryWriteStatements) EntryInsert(rowCount int) (*sqlite.Stmt, error) {
	if stmt := stmts.entryInsertStmts[rowCount]; stmt != nil {
		return stmt, nil
	}
	stmt, _, err := stmts.conn.PrepareTransient(buildSessionEntryInsertSQL(rowCount))
	if err != nil {
		return nil, err
	}
	stmts.entryInsertStmts[rowCount] = stmt
	return stmt, nil
}

func (stmts *sessionEntryWriteStatements) EntryExt() (*sqlite.Stmt, error) {
	if stmts.extStmt != nil {
		return stmts.extStmt, nil
	}
	stmt, _, err := stmts.conn.PrepareTransient(sqlInsertSessionEntryExt)
	if err != nil {
		return nil, err
	}
	stmts.extStmt = stmt
	return stmt, nil
}

func (stmts *sessionEntryWriteStatements) SessionCommand() (*sqlite.Stmt, error) {
	if stmts.commandStmt != nil {
		return stmts.commandStmt, nil
	}
	stmt, _, err := stmts.conn.PrepareTransient(sqlInsertSessionCommand)
	if err != nil {
		return nil, err
	}
	stmts.commandStmt = stmt
	return stmt, nil
}

func insertSessionEntries(stmts *sessionEntryWriteStatements, entries []schema.SessionEntry) error {
	for start := 0; start < len(entries); start += sessionEntryInsertChunkSize {
		end := start + sessionEntryInsertChunkSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := insertSessionEntryChunk(stmts, entries[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func insertSessionEntryChunk(stmts *sessionEntryWriteStatements, entries []schema.SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	stmt, err := stmts.EntryInsert(len(entries))
	if err != nil {
		return err
	}

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
	anchors       []entryTargetAnchor
}

type entryTargetAnchor struct {
	entryIndex     int
	entryID        string
	toolCallID     string
	entryType      string
	role           string
	partType       string
	contentPreview string
}

type annotationRemapRefusedError struct {
	err error
}

func (e *annotationRemapRefusedError) Error() string { return e.err.Error() }

func (e *annotationRemapRefusedError) Unwrap() error { return e.err }

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
	if len(targets) == 0 {
		return targets, nil
	}

	anchors, err := readSessionEntryAnchors(conn, sessionID)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		for index := targets[i].entryIndex; index < targets[i].endIndex; index++ {
			if anchor, ok := anchors[index]; ok {
				targets[i].anchors = append(targets[i].anchors, anchor)
			}
		}
	}
	return targets, nil
}

func readEntryAnnotationTargetSpans(conn *sqlite.Conn, sessionID string) ([]entryAnnotationTarget, error) {
	var targets []entryAnnotationTarget
	err := sqlitex.ExecuteTransient(conn, sqlSelectTargetEntrySpansForSession, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			targets = append(targets, entryAnnotationTarget{
				entryIndex: stmt.ColumnInt(0),
				endIndex:   stmt.ColumnInt(1),
			})
			return nil
		},
	})
	return targets, err
}

func readSessionEntryAnchors(conn *sqlite.Conn, sessionID string) (map[int]entryTargetAnchor, error) {
	anchors := map[int]entryTargetAnchor{}
	err := sqlitex.ExecuteTransient(conn, sqlSelectSessionEntryAnchors, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			anchor := entryTargetAnchor{
				entryIndex:     stmt.ColumnInt(0),
				entryID:        stmt.ColumnText(1),
				toolCallID:     stmt.ColumnText(2),
				entryType:      stmt.ColumnText(3),
				role:           stmt.ColumnText(4),
				partType:       stmt.ColumnText(5),
				contentPreview: stmt.ColumnText(6),
			}
			anchors[anchor.entryIndex] = anchor
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return anchors, nil
}

// restoreEntryAnnotationTargets re-attaches the annotations that were carried
// across the replacement, to the entries that are still there.
//
// If the old numeric span no longer points at the same anchored entries, the
// target moves only when every target entry has one unique contiguous match in
// the replacement index. Otherwise the transaction fails. Forcing the old row
// back would retain an annotation over different content; dropping it would
// orphan the annotation. Rolling back preserves both the prior index and target
// until the user deliberately removes or recreates that annotation.
func restoreEntryAnnotationTargets(
	conn *sqlite.Conn,
	sessionID string,
	carried []entryAnnotationTarget,
	entries []schema.SessionEntry,
) (int, error) {
	if len(carried) == 0 {
		return 0, nil
	}
	remappedTargets := 0
	present := make(map[int]bool, len(entries))
	for i := range entries {
		present[entries[i].EntryIndex] = true
	}
	newAnchors := make([]entryTargetAnchor, 0, len(entries))
	for i := range entries {
		newAnchors = append(newAnchors, entryTargetAnchorFromEntry(&entries[i]))
	}
	for _, target := range carried {
		start, end := target.entryIndex, target.endIndex
		missingIndex := missingSpanIndex(target, present)
		if missingIndex < 0 && entryAnnotationSpanStillMatches(target, newAnchors) {
			if err := insertEntryAnnotationTarget(conn, sessionID, target, start, end); err != nil {
				return remappedTargets, err
			}
			continue
		}
		if missingIndex < 0 {
			missingIndex = target.entryIndex
		}
		{
			var remapped bool
			if start, end, remapped = remapEntryAnnotationTarget(target, newAnchors); !remapped {
				dryRun := fmt.Sprintf("peasant annotate prune %s --session %s --dry-run",
					githooks.ShellQuote(target.annotatorName), githooks.ShellQuote(sessionID))
				return remappedTargets, &annotationRemapRefusedError{err: fmt.Errorf("store: preserve annotation_target_entries for %s[%d,%d): the re-index no longer contains the complete span targeted by annotation %s — "+
					"what: an entry annotation could not be carried onto the replacement index; "+
					"why: entry %d disappeared from the newly indexed transcript and no unique contiguous anchor match was found; "+
					"user impact: the re-index was rolled back, so the previous entries and annotation target remain intact and later village pushes are not poisoned by an orphan; "+
					"how to fix: using the same global --config-dir/--data-dir overrides as this ingest, inspect the annotation with 'peasant annotate list %s'. If it no longer applies, preview the annotator-and-session-scoped cleanup with '%s', remove or recreate the affected annotation, then re-run the same peasant ingest command",
					sessionID, target.entryIndex, target.endIndex, target.annotationID, missingIndex,
					githooks.ShellQuote(sessionID), dryRun)}
			}
		}
		if start != target.entryIndex || end != target.endIndex {
			remappedTargets++
		}
		if err := insertEntryAnnotationTarget(conn, sessionID, target, start, end); err != nil {
			return remappedTargets, err
		}
	}
	return remappedTargets, nil
}

func missingSpanIndex(target entryAnnotationTarget, present map[int]bool) int {
	for index := target.entryIndex; index < target.endIndex; index++ {
		if !present[index] {
			return index
		}
	}
	return -1
}

func entryAnnotationSpanStillMatches(target entryAnnotationTarget, newAnchors []entryTargetAnchor) bool {
	spanLen := target.endIndex - target.entryIndex
	if spanLen <= 0 || len(target.anchors) != spanLen {
		return true
	}
	newByIndex := map[int]entryTargetAnchor{}
	for _, anchor := range newAnchors {
		newByIndex[anchor.entryIndex] = anchor
	}
	for _, oldAnchor := range target.anchors {
		if len(oldAnchor.matchKeys()) == 0 {
			continue
		}
		newAnchor, ok := newByIndex[oldAnchor.entryIndex]
		if !ok || !anchorsShareKey(oldAnchor, newAnchor) {
			return false
		}
	}
	return true
}

func anchorsShareKey(a, b entryTargetAnchor) bool {
	keys := map[string]bool{}
	for _, key := range a.matchKeys() {
		keys[key] = true
	}
	for _, key := range b.matchKeys() {
		if keys[key] {
			return true
		}
	}
	return false
}

func insertEntryAnnotationTarget(conn *sqlite.Conn, sessionID string, target entryAnnotationTarget, start, end int) error {
	if err := sqlitex.ExecuteTransient(conn, sqlInsertTargetEntry, &sqlitex.ExecOptions{
		Args: []any{target.annotationID, sessionID, start, end},
	}); err != nil {
		return fmt.Errorf("store: restore annotation_target_entries for %s[%d]: %w — "+
			"what: an entry annotation could not be re-attached to the entry it targets after re-indexing; "+
			"why: the insert into annotation_target_entries failed; "+
			"user impact: session %s was NOT re-indexed (the whole re-index is rolled back), because completing it would have left annotation %s with no publishable target; "+
			"how to fix: check the analytics store for constraint or corruption errors, then re-run the same peasant ingest command with the same global --config-dir/--data-dir overrides",
			sessionID, start, err, sessionID, target.annotationID)
	}
	return nil
}

func remapEntryAnnotationTarget(target entryAnnotationTarget, newAnchors []entryTargetAnchor) (int, int, bool) {
	spanLen := target.endIndex - target.entryIndex
	if spanLen <= 0 || len(target.anchors) != spanLen {
		return 0, 0, false
	}
	indexByKey := map[string][]int{}
	for _, anchor := range newAnchors {
		for _, key := range anchor.matchKeys() {
			indexByKey[key] = append(indexByKey[key], anchor.entryIndex)
		}
	}

	start := -1
	previous := -1
	for i, anchor := range target.anchors {
		mapped, ok := uniqueAnchorMatch(anchor, indexByKey)
		if !ok {
			return 0, 0, false
		}
		if i == 0 {
			start = mapped
		} else if mapped != previous+1 {
			return 0, 0, false
		}
		previous = mapped
	}
	return start, previous + 1, true
}

func uniqueAnchorMatch(anchor entryTargetAnchor, indexByKey map[string][]int) (int, bool) {
	for _, key := range anchor.matchKeys() {
		matches := indexByKey[key]
		if len(matches) == 1 {
			return matches[0], true
		}
	}
	return 0, false
}

func entryTargetAnchorFromEntry(entry *schema.SessionEntry) entryTargetAnchor {
	anchor := entryTargetAnchor{
		entryIndex: entry.EntryIndex,
		entryType:  entry.EntryType.String(),
		role:       entry.Role.String(),
	}
	if entry.EntryID != nil {
		anchor.entryID = *entry.EntryID
	}
	if entry.ToolCallID != nil {
		anchor.toolCallID = *entry.ToolCallID
	}
	if entry.PartType != nil {
		anchor.partType = *entry.PartType
	}
	if entry.ContentPreview != nil {
		anchor.contentPreview = *entry.ContentPreview
	}
	return anchor
}

func (anchor entryTargetAnchor) matchKeys() []string {
	keys := make([]string, 0, 3)
	if anchor.entryID != "" {
		keys = append(keys, "entry_id\x00"+anchor.entryID)
	}
	if anchor.toolCallID != "" {
		keys = append(keys, "tool_call_id\x00"+anchor.role+"\x00"+anchor.entryType+"\x00"+anchor.partType+"\x00"+anchor.toolCallID)
	}
	if anchor.contentPreview != "" {
		keys = append(keys, "content\x00"+anchor.role+"\x00"+anchor.entryType+"\x00"+anchor.partType+"\x00"+anchor.contentPreview)
	}
	return keys
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
