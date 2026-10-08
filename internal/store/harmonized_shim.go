package store

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The routing shim's dispatch side (design section 6.2 readers 3, 6, 8, 9):
// every bounded legacy-entry read serves harmonized sessions from the
// partition-0 body rows through legacyShape and every other session from the
// mirror, with the callers unchanged.
//
// The mirror holds main-partition entries only (its primary key is
// (session_id, entry_index)), so the shim reads partition 0 as well. Earlier
// partitions serve only the snapshot builders.

// shimmedOnConn reports whether a session's entries read through the shim: a
// harmonized active generation exists, so the mirror rows are gone and the
// body rows serve through legacyShape.
func shimmedOnConn(conn *sqlite.Conn, sessionID string) (bool, error) {
	_, harmonized, err := harmonizedActiveOnConn(conn, schema.SessionID(sessionID))
	if err != nil {
		return false, err
	}
	return harmonized, nil
}

// shimmedIDsOnConn partitions one batch of session IDs by representation in
// a single query, so bulk reads issue one dispatch instead of one per
// session.
func shimmedIDsOnConn(conn *sqlite.Conn, sessionIDs []string) (map[string]bool, error) {
	shimmed := make(map[string]bool, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return shimmed, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT s.session_id FROM sessions s
WHERE s.session_id IN (` + strings.Join(placeholders, ",") + `)
AND s.active_generation_id IS NOT NULL
AND EXISTS (SELECT 1 FROM session_generations g
WHERE g.session_id = s.session_id AND g.generation_id = s.active_generation_id)`
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			shimmed[stmt.ColumnText(0)] = true
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: partition sessions by representation: %w", err)
	}
	return shimmed, nil
}

// shimBoundedOnConn reports whether the shim bounds previews for a session:
// exactly when its capture is a full-content one. Preview-only captures stay
// unbounded, the way the mirror stored them. A session with no capture row
// never went through the full writer, so it reads unbounded too.
func shimBoundedOnConn(conn *sqlite.Conn, sessionID string) (bool, error) {
	capture, found, err := readCapture(conn, ingest.SessionID(sessionID))
	if err != nil || !found {
		return false, err
	}
	return capture.CaptureFormat == ingest.ContentCaptureFormatFull, nil
}

// shimBodyRecordsOnConn reads one session's partition-0 body rows in entry
// order with their stored digests. A your-range variant filters by entry
// index; a nil bound leaves that side open.
func shimBodyRecordsOnConn(conn *sqlite.Conn, sessionID string, minIndex, maxIndex *int) ([]EntryRecord, error) {
	query := `SELECT
    b.body_id, b.session_id, b.body_digest, b.entry_index, b.harness, b.entry_type, b.role,
    b.timestamp_ms, b.content_preview, b.tokens_in, b.tokens_out,
    b.has_tool_use, b.tool_kind, b.tool_names_csv,
    b.has_thinking, b.is_error, b.stop_reason, b.raw_byte_length,
    b.tool_call_id, b.entry_id, b.parent_entry_id, b.depth, b.parent_index,
    b.tool_input, b.tool_output,
    b.model_id, b.tokens_reasoning, b.cache_read, b.cache_write,
    b.extra, b.extra_verbatim, b.part_type, b.source_entry_ref,
    b.prov_origin, b.prov_actor, b.prov_delivery, b.prov_ownership,
    b.prov_evidence, b.prov_input_modality, b.prov_submission_ref
FROM session_generation_entries m
JOIN session_entry_bodies b
  ON b.session_id = m.session_id AND b.body_digest = m.body_digest
JOIN sessions s ON s.session_id = m.session_id
WHERE m.session_id = ? AND m.generation_id = s.active_generation_id AND m.partition_id = 0`
	args := []any{sessionID}
	if minIndex != nil {
		query += ` AND m.entry_index >= ?`
		args = append(args, *minIndex)
	}
	if maxIndex != nil {
		query += ` AND m.entry_index <= ?`
		args = append(args, *maxIndex)
	}
	query += ` ORDER BY m.entry_index`
	var records []EntryRecord
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanBodyRow(stmt, 0)
			if err != nil {
				return err
			}
			records = append(records, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read shim entries for %s: %w", sessionID, err)
	}
	return records, nil
}

// verifyShimRecords recomputes every row's canonical digest against its
// stored anchor. Full reads call this before shaping anything, so a column
// altered under its digest refuses with no partial output.
func verifyShimRecords(records []EntryRecord) error {
	for i := range records {
		if digest := SerializeEntryDigest(records[i]); digest != records[i].BodyDigest {
			return fmt.Errorf("store: shim entry at index %d fails digest verification; stored bytes do not match the captured digest; no entries were served; run harvest verify --content and re-index the session to repair", records[i].EntryIndex)
		}
	}
	return nil
}

// shimListEntries shapes partition-0 rows for the ListEntries-family
// callers: bounded like the mirror, with the same retained-evidence gate the
// mirror path applies per entry.
func shimListEntries(records []EntryRecord, bounded bool) ([]schema.SessionEntry, error) {
	entries := make([]schema.SessionEntry, 0, len(records))
	for _, record := range records {
		entry := legacyShape(record, bounded)
		if _, _, err := ingest.DecodePiEntryExtra(entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// shimFirstUserPreview reads the first depth-zero user preview for one
// session through the shim, bounded exactly as the mirror bounded it.
func shimFirstUserPreview(conn *sqlite.Conn, sessionID string) (string, error) {
	preview := ""
	err := sqlitex.ExecuteTransient(conn, `SELECT b.content_preview FROM session_generation_entries m
JOIN session_entry_bodies b
  ON b.session_id = m.session_id AND b.body_digest = m.body_digest
JOIN sessions s ON s.session_id = m.session_id
WHERE m.session_id = ? AND m.generation_id = s.active_generation_id AND m.partition_id = 0
AND b.role = 'user' AND b.depth = 0
ORDER BY m.entry_index ASC LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				preview = stmt.ColumnText(0)
			}
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("store: shim first user preview for %q: %w", sessionID, err)
	}
	bounded, err := shimBoundedOnConn(conn, sessionID)
	if err != nil {
		return "", err
	}
	if bounded {
		preview = contentPreview(preview)
	}
	return TruncateToRunes(preview, defaults.SessionPreviewMaxChars), nil
}

// shimFirstUserPreviewBulk reads the first depth-zero user preview per
// shimmed session in one query, with the same per-session MIN(entry_index)
// semantics as the mirror bulk read.
func shimFirstUserPreviewBulk(conn *sqlite.Conn, sessionIDs []string) (map[string]string, error) {
	result := make(map[string]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT m.session_id, b.content_preview FROM session_generation_entries m
JOIN session_entry_bodies b
  ON b.session_id = m.session_id AND b.body_digest = m.body_digest
JOIN sessions s ON s.session_id = m.session_id
WHERE m.partition_id = 0 AND b.role = 'user' AND b.depth = 0
AND m.generation_id = s.active_generation_id
AND (m.session_id, m.entry_index) IN (
  SELECT m2.session_id, MIN(m2.entry_index)
  FROM session_generation_entries m2
  JOIN sessions s2 ON s2.session_id = m2.session_id
  JOIN session_entry_bodies b2
    ON b2.session_id = m2.session_id AND b2.body_digest = m2.body_digest
  WHERE m2.session_id IN (` + strings.Join(placeholders, ",") + `)
  AND m2.partition_id = 0 AND b2.role = 'user' AND b2.depth = 0
  AND m2.generation_id = s2.active_generation_id
  GROUP BY m2.session_id
)`
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sid := stmt.ColumnText(0)
			preview := ""
			if stmt.ColumnType(1) != sqlite.TypeNull {
				preview = stmt.ColumnText(1)
			}
			result[sid] = preview
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: shim first user preview bulk query: %w", err)
	}
	ids := make([]string, 0, len(result))
	for sid := range result {
		ids = append(ids, sid)
	}
	bounded, err := shimBoundedMapOnConn(conn, ids)
	if err != nil {
		return nil, err
	}
	for sid, preview := range result {
		if bounded[sid] {
			preview = contentPreview(preview)
		}
		result[sid] = TruncateToRunes(preview, defaults.SessionPreviewMaxChars)
	}
	return result, nil
}

// shimLeadingUserPreviewsBulk reads up to perSession leading depth-zero user

// shimBoundedMapOnConn reports the preview bound per session in one query:
// full-content captures bound, everything else (including no capture row)
// reads unbounded, the way the mirror stored it.
func shimBoundedMapOnConn(conn *sqlite.Conn, sessionIDs []string) (map[string]bool, error) {
	bounded := make(map[string]bool, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return bounded, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT session_id, capture_format FROM session_content_captures
WHERE session_id IN (` + strings.Join(placeholders, ",") + `)`
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			bounded[stmt.ColumnText(0)] = stmt.ColumnText(1) == string(ingest.ContentCaptureFormatFull)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read capture formats for %d session(s): %w", len(sessionIDs), err)
	}
	return bounded, nil
}

// isFullCaptureOnConn reports whether a session's stored capture is a
// full-content one. Sessions without a capture row read as preview.
func isFullCaptureOnConn(conn *sqlite.Conn, sessionID string) (bool, error) {
	return shimBoundedOnConn(conn, sessionID)
}

// shimLeadingUserPreviewsBulk reads up to perSession leading depth-zero user
// previews per shimmed session, in entry order, with the same row-number
// semantics as the mirror bulk read.
func shimLeadingUserPreviewsBulk(conn *sqlite.Conn, sessionIDs []string, perSession int) (map[string][]string, error) {
	result := make(map[string][]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs), len(sessionIDs)+1)
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	args = append(args, perSession)
	query := `SELECT session_id, content_preview, rn FROM (
  SELECT m.session_id AS session_id, b.content_preview AS content_preview,
    ROW_NUMBER() OVER (PARTITION BY m.session_id ORDER BY m.entry_index ASC) AS rn
  FROM session_generation_entries m
  JOIN session_entry_bodies b
    ON b.session_id = m.session_id AND b.body_digest = m.body_digest
  JOIN sessions s ON s.session_id = m.session_id
  WHERE m.session_id IN (` + strings.Join(placeholders, ",") + `)
  AND m.partition_id = 0 AND b.role = 'user' AND b.depth = 0
  AND m.generation_id = s.active_generation_id
) WHERE rn <= ? ORDER BY session_id, rn`
	type rawPreview struct {
		sessionID string
		preview   string
		first     bool
	}
	var raws []rawPreview
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			preview := ""
			if stmt.ColumnType(1) != sqlite.TypeNull {
				preview = stmt.ColumnText(1)
			}
			raws = append(raws, rawPreview{sessionID: stmt.ColumnText(0), preview: preview, first: stmt.ColumnInt(2) == 1})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: shim leading user previews bulk query: %w", err)
	}
	ids := make([]string, 0, len(sessionIDs))
	seen := make(map[string]bool, len(sessionIDs))
	for _, raw := range raws {
		if !seen[raw.sessionID] {
			seen[raw.sessionID] = true
			ids = append(ids, raw.sessionID)
		}
	}
	bounded, err := shimBoundedMapOnConn(conn, ids)
	if err != nil {
		return nil, err
	}
	for _, raw := range raws {
		// Repeated requested IDs in later batches must not duplicate records.
		if raw.first {
			result[raw.sessionID] = nil
		}
		preview := raw.preview
		if bounded[raw.sessionID] {
			preview = contentPreview(preview)
		}
		result[raw.sessionID] = append(result[raw.sessionID], TruncateToRunes(preview, defaults.SessionPreviewMaxChars))
	}
	return result, nil
}

// shimMaxEntryIndex reads the highest mapped main-partition index, or -1
// when the session maps nothing.
func shimMaxEntryIndex(conn *sqlite.Conn, sessionID string) (int, error) {
	maxIdx := -1
	err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(MAX(m.entry_index), -1) FROM session_generation_entries m
JOIN sessions s ON s.session_id = m.session_id
WHERE m.session_id = ? AND m.generation_id = s.active_generation_id AND m.partition_id = 0`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			maxIdx = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		return -1, fmt.Errorf("store: shim max entry index for %s: %w", sessionID, err)
	}
	return maxIdx, nil
}

// shimFirstEntry reads the lowest mapped main-partition index with its role,
// or nil when the session maps nothing.
func shimFirstEntry(conn *sqlite.Conn, sessionID string) (*EntryHead, error) {
	var head *EntryHead
	err := sqlitex.ExecuteTransient(conn, `SELECT m.entry_index, b.role FROM session_generation_entries m
JOIN session_entry_bodies b
  ON b.session_id = m.session_id AND b.body_digest = m.body_digest
JOIN sessions s ON s.session_id = m.session_id
WHERE m.session_id = ? AND m.generation_id = s.active_generation_id AND m.partition_id = 0
ORDER BY m.entry_index ASC LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			head = &EntryHead{
				EntryIndex: stmt.ColumnInt(0),
				Role:       schema.Role(stmt.ColumnText(1)),
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: shim first entry for %s: %w", sessionID, err)
	}
	return head, nil
}
