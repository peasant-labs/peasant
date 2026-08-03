package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SQL constants for the metrics read path.
const (
	// V23+: host_slug is no longer a column on sessions; must JOIN host_slugs.
	sqlLookupSessionLocation = `SELECT h.host_slug, s.parent_id
FROM sessions s
JOIN host_slugs h ON s.opaque_host_id = h.opaque_id
WHERE s.session_id = ? LIMIT 1`

	sqlGetMetrics = `SELECT
    session_id, turn_count, subagent_count,
    title, outcome,
    total_tokens, input_tokens, output_tokens,
    tool_calls, files_touched, lines_changed,
    duration_minutes,
    retry_loops, retry_tokens_wasted, within_session_reverts,
    signal_density, spec_quality_score, exploration_ratio,
    scope_breadth, discovery_turns,
    m2_token_outcome_ratio,
    m3_unique_tool_count,
    m4_error_recovery_count, m4_consecutive_error_max,
    m5_context_utilization_pct, m5_peak_context_tokens, m5_avg_message_tokens,
    m6_output_survival_pct, m6_lines_survived, m6_lines_total,
    m7_spec_word_count, m7_spec_has_examples, m7_spec_has_constraints,
    computed_at, compute_version,
    cost_input_usd, cost_output_usd, cost_reasoning_usd,
    cost_cache_read_usd, cost_cache_write_usd, cost_total_usd, cost_model_id,
    scope
FROM session_metrics WHERE session_id = ?`

	sqlMetricsExist = `SELECT compute_version FROM session_metrics WHERE session_id = ?`

	sqlListEntries = `SELECT
    session_id, entry_index, provider, entry_type, role,
    timestamp_ms, content_preview, tokens_in, tokens_out,
    has_tool_use, tool_names_csv, has_thinking, is_error,
    raw_byte_length, tool_call_id, entry_id, parent_entry_id, extra,
    depth, parent_index, tool_input, tool_output,
    tool_kind, stop_reason, part_type
FROM session_entries
WHERE session_id = ?
ORDER BY entry_index`

	sqlListEntriesExt = `SELECT entry_index, key, value_text, value_int, value_real
FROM session_entries_ext
WHERE session_id = ?
ORDER BY entry_index, key`

	sqlListEntriesRange = `SELECT
    session_id, entry_index, provider, entry_type, role,
    timestamp_ms, content_preview, tokens_in, tokens_out,
    has_tool_use, tool_names_csv, has_thinking, is_error,
    raw_byte_length, tool_call_id, entry_id, parent_entry_id, extra,
    depth, parent_index, tool_input, tool_output,
    tool_kind, stop_reason, part_type
FROM session_entries
WHERE session_id = ? AND entry_index BETWEEN ? AND ?
ORDER BY entry_index`

	sqlListEntriesExtRange = `SELECT entry_index, key, value_text, value_int, value_real
FROM session_entries_ext
WHERE session_id = ? AND entry_index BETWEEN ? AND ?
ORDER BY entry_index, key`

	sqlMaxEntryIndex = `SELECT COALESCE(MAX(entry_index), -1) FROM session_entries WHERE session_id = ?`
)

// LookupSessionLocation returns the host_slug and parent_id for a session.
// Returns ("", "", nil) if the session is not found in the DB.
// Used by reconstructFromMetadata to avoid scanning all host directories.
func (s *Store) LookupSessionLocation(ctx context.Context, sessionID ingest.SessionID) (hostSlug string, parentID string, err error) {
	conn, connErr := s.pool.Take(ctx)
	if connErr != nil {
		return "", "", fmt.Errorf("store: take connection: %w", connErr)
	}
	defer s.pool.Put(conn)

	err = sqlitex.ExecuteTransient(conn, sqlLookupSessionLocation, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			hostSlug = stmt.ColumnText(0)
			// parent_id is nullable; ColumnText returns "" for NULL.
			parentID = stmt.ColumnText(1)
			return nil
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("store: lookup session location for %s: %w", sessionID, err)
	}
	return hostSlug, parentID, nil
}

// LookupSourceInfo returns source_path, source_format, and model_harness for a session.
// Returns ("", "", "", nil) if the session is not found.
// Used as a fallback when peasant-sync metadata is missing (e.g. subagent sessions).
// Delegates to SessionSourceInfo to avoid duplicating the query.
func (s *Store) LookupSourceInfo(ctx context.Context, sessionID ingest.SessionID) (sourcePath string, sourceFormat ingest.SourceFormat, provider string, err error) {
	info, err := s.SessionSourceInfo(ctx, string(sessionID))
	if err != nil {
		return "", "", "", fmt.Errorf("store: lookup source info for %s: %w", sessionID, err)
	}
	if info == nil {
		return "", "", "", nil
	}
	return info.SourcePath, info.SourceFormat, info.Harness, nil
}

// GetMetrics returns the SessionMetrics for a session, or nil if none exists.
func (s *Store) GetMetrics(ctx context.Context, sessionID ingest.SessionID) (*ingest.SessionMetrics, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var m *ingest.SessionMetrics
	err = sqlitex.ExecuteTransient(conn, sqlGetMetrics, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			m = scanSessionMetrics(stmt)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get metrics for %s: %w", sessionID, err)
	}
	return m, nil
}

// MetricsExist returns true if session_metrics exists for the session
// with compute_version >= the given version.
func (s *Store) MetricsExist(ctx context.Context, sessionID ingest.SessionID, computeVersion int) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var exists bool
	err = sqlitex.ExecuteTransient(conn, sqlMetricsExist, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnInt(0) >= computeVersion {
				exists = true
			}
			return nil
		},
	})
	if err != nil {
		return false, fmt.Errorf("store: check metrics for %s: %w", sessionID, err)
	}
	return exists, nil
}

// ListEntries returns all session_entries for a session ordered by entry_index.
// Known ext keys are re-hydrated from session_entries_ext back into the Extra JSON string.
func (s *Store) ListEntries(ctx context.Context, sessionID ingest.SessionID) ([]schema.SessionEntry, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var entries []schema.SessionEntry
	err = sqlitex.ExecuteTransient(conn, sqlListEntries, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entries = append(entries, scanSessionEntry(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list entries for %s: %w", sessionID, err)
	}

	// Query ext rows and merge known keys back into Extra JSON.
	extMap := make(map[int]map[string]any) // entry_index -> key -> value
	err = sqlitex.ExecuteTransient(conn, sqlListEntriesExt, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			idx := stmt.ColumnInt(0)
			key := stmt.ColumnText(1)
			if extMap[idx] == nil {
				extMap[idx] = make(map[string]any)
			}
			// Read the non-null value column.
			if stmt.ColumnType(2) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnText(2) // value_text
			} else if stmt.ColumnType(3) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnInt(3) // value_int
			} else if stmt.ColumnType(4) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnFloat(4) // value_real
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list entries ext for %s: %w", sessionID, err)
	}

	// Merge ext values back into each entry's Extra JSON.
	for i := range entries {
		extKVs, ok := extMap[entries[i].EntryIndex]
		if !ok || len(extKVs) == 0 {
			continue
		}
		mergeExtIntoExtra(&entries[i], extKVs)
	}

	return entries, nil
}

// ListEntriesRange returns session_entries for a session where entry_index is in
// [fromIndex, toIndex] (inclusive), ordered by entry_index. ext values are
// re-hydrated from session_entries_ext using the same logic as ListEntries.
// Returns an empty slice (not an error) when no entries exist in the range.
func (s *Store) ListEntriesRange(ctx context.Context, sessionID schema.SessionID, fromIndex, toIndex int) ([]schema.SessionEntry, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var entries []schema.SessionEntry
	err = sqlitex.ExecuteTransient(conn, sqlListEntriesRange, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), fromIndex, toIndex},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entries = append(entries, scanSessionEntry(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list entries range [%d,%d] for %s: %w", fromIndex, toIndex, sessionID, err)
	}

	// Query ext rows for the range and merge known keys back into Extra JSON.
	extMap := make(map[int]map[string]any) // entry_index -> key -> value
	err = sqlitex.ExecuteTransient(conn, sqlListEntriesExtRange, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), fromIndex, toIndex},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			idx := stmt.ColumnInt(0)
			key := stmt.ColumnText(1)
			if extMap[idx] == nil {
				extMap[idx] = make(map[string]any)
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnText(2) // value_text
			} else if stmt.ColumnType(3) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnInt(3) // value_int
			} else if stmt.ColumnType(4) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnFloat(4) // value_real
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list entries ext range [%d,%d] for %s: %w", fromIndex, toIndex, sessionID, err)
	}

	for i := range entries {
		extKVs, ok := extMap[entries[i].EntryIndex]
		if !ok || len(extKVs) == 0 {
			continue
		}
		mergeExtIntoExtra(&entries[i], extKVs)
	}

	return entries, nil
}

// MaxEntryIndex returns the maximum entry_index for a session, or -1 if the
// session has no indexed entries (empty session or session not found in DB).
func (s *Store) MaxEntryIndex(ctx context.Context, sessionID schema.SessionID) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return -1, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	maxIdx := -1
	err = sqlitex.ExecuteTransient(conn, sqlMaxEntryIndex, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			maxIdx = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		return -1, fmt.Errorf("store: max entry index for %s: %w", sessionID, err)
	}
	return maxIdx, nil
}

// mergeExtIntoExtra merges ext key-value pairs into the entry's Extra JSON string.
func mergeExtIntoExtra(e *schema.SessionEntry, extKVs map[string]any) {
	var existing map[string]any
	if e.Extra != nil {
		_ = json.Unmarshal([]byte(*e.Extra), &existing)
	}
	if existing == nil {
		existing = make(map[string]any)
	}
	for k, v := range extKVs {
		existing[k] = v
	}
	b, err := json.Marshal(existing)
	if err != nil {
		return
	}
	s := string(b)
	e.Extra = &s
}

// scanSessionMetrics reads a SessionMetrics from the current statement.
// Column order must match sqlGetMetrics.
func scanSessionMetrics(stmt *sqlite.Stmt) *ingest.SessionMetrics {
	cv := stmt.ColumnInt(34)
	m := &ingest.SessionMetrics{
		SessionID: schema.SessionID(stmt.ColumnText(0)),
	}
	m.ComputeVersion = &cv

	// Column 1: turn_count (nullable INTEGER)
	if stmt.ColumnType(1) != sqlite.TypeNull {
		v := stmt.ColumnInt(1)
		m.TurnCount = &v
	}
	// Column 2: subagent_count
	if stmt.ColumnType(2) != sqlite.TypeNull {
		v := stmt.ColumnInt(2)
		m.SubagentCount = &v
	}
	// Column 3: title
	if stmt.ColumnType(3) != sqlite.TypeNull {
		v := stmt.ColumnText(3)
		m.TitleGenerated = &v
	}
	// Column 4: outcome
	if stmt.ColumnType(4) != sqlite.TypeNull {
		v := schema.SessionOutcome(stmt.ColumnText(4))
		m.Outcome = &v
	}
	// Column 5: total_tokens
	if stmt.ColumnType(5) != sqlite.TypeNull {
		v := stmt.ColumnInt(5)
		m.TotalTokens = &v
	}
	// Column 6: input_tokens
	if stmt.ColumnType(6) != sqlite.TypeNull {
		v := stmt.ColumnInt(6)
		m.InputTokens = &v
	}
	// Column 7: output_tokens
	if stmt.ColumnType(7) != sqlite.TypeNull {
		v := stmt.ColumnInt(7)
		m.OutputTokens = &v
	}
	// Column 8: tool_calls
	if stmt.ColumnType(8) != sqlite.TypeNull {
		v := stmt.ColumnInt(8)
		m.ToolCalls = &v
	}
	// Column 9: files_touched
	if stmt.ColumnType(9) != sqlite.TypeNull {
		v := stmt.ColumnInt(9)
		m.FilesTouched = &v
	}
	// Column 10: lines_changed
	if stmt.ColumnType(10) != sqlite.TypeNull {
		v := stmt.ColumnInt(10)
		m.LinesChanged = &v
	}
	// Column 11: duration_minutes
	if stmt.ColumnType(11) != sqlite.TypeNull {
		v := stmt.ColumnFloat(11)
		m.DurationMinutes = &v
	}
	// Column 12: retry_loops
	if stmt.ColumnType(12) != sqlite.TypeNull {
		v := stmt.ColumnInt(12)
		m.RetryLoops = &v
	}
	// Column 13: retry_tokens_wasted
	if stmt.ColumnType(13) != sqlite.TypeNull {
		v := stmt.ColumnInt(13)
		m.RetryTokensWasted = &v
	}
	// Column 14: within_session_reverts
	if stmt.ColumnType(14) != sqlite.TypeNull {
		v := stmt.ColumnInt(14)
		m.WithinSessionReverts = &v
	}
	// Column 15: signal_density
	if stmt.ColumnType(15) != sqlite.TypeNull {
		v := stmt.ColumnFloat(15)
		m.SignalDensity = &v
	}
	// Column 16: spec_quality_score
	if stmt.ColumnType(16) != sqlite.TypeNull {
		v := stmt.ColumnFloat(16)
		m.SpecQualityScore = &v
	}
	// Column 17: exploration_ratio
	if stmt.ColumnType(17) != sqlite.TypeNull {
		v := stmt.ColumnFloat(17)
		m.ExplorationRatio = &v
	}
	// Column 18: scope_breadth
	if stmt.ColumnType(18) != sqlite.TypeNull {
		v := stmt.ColumnInt(18)
		m.ScopeBreadth = &v
	}
	// Column 19: discovery_turns
	if stmt.ColumnType(19) != sqlite.TypeNull {
		v := stmt.ColumnInt(19)
		m.DiscoveryTurns = &v
	}
	// Column 20: m2_token_outcome_ratio
	if stmt.ColumnType(20) != sqlite.TypeNull {
		v := stmt.ColumnFloat(20)
		m.M2TokenOutcomeRatio = &v
	}
	// Column 21: m3_unique_tool_count
	if stmt.ColumnType(21) != sqlite.TypeNull {
		v := stmt.ColumnInt(21)
		m.M3UniqueToolCount = &v
	}
	// Column 22: m4_error_recovery_count
	if stmt.ColumnType(22) != sqlite.TypeNull {
		v := stmt.ColumnInt(22)
		m.M4ErrorRecoveryCount = &v
	}
	// Column 23: m4_consecutive_error_max
	if stmt.ColumnType(23) != sqlite.TypeNull {
		v := stmt.ColumnInt(23)
		m.M4ConsecutiveErrorMax = &v
	}
	// Column 24: m5_context_utilization_pct
	if stmt.ColumnType(24) != sqlite.TypeNull {
		v := stmt.ColumnFloat(24)
		m.M5ContextUtilizationPct = &v
	}
	// Column 25: m5_peak_context_tokens
	if stmt.ColumnType(25) != sqlite.TypeNull {
		v := stmt.ColumnInt(25)
		m.M5PeakContextTokens = &v
	}
	// Column 26: m5_avg_message_tokens
	if stmt.ColumnType(26) != sqlite.TypeNull {
		v := stmt.ColumnInt(26)
		m.M5AvgMessageTokens = &v
	}
	// Column 27: m6_output_survival_pct
	if stmt.ColumnType(27) != sqlite.TypeNull {
		v := stmt.ColumnFloat(27)
		m.M6OutputSurvivalPct = &v
	}
	// Column 28: m6_lines_survived
	if stmt.ColumnType(28) != sqlite.TypeNull {
		v := stmt.ColumnInt(28)
		m.M6LinesSurvived = &v
	}
	// Column 29: m6_lines_total
	if stmt.ColumnType(29) != sqlite.TypeNull {
		v := stmt.ColumnInt(29)
		m.M6LinesTotal = &v
	}
	// Column 30: m7_spec_word_count
	if stmt.ColumnType(30) != sqlite.TypeNull {
		v := stmt.ColumnInt(30)
		m.M7SpecWordCount = &v
	}
	// Column 31: m7_spec_has_examples
	if stmt.ColumnType(31) != sqlite.TypeNull {
		v := stmt.ColumnInt(31) == 1
		m.M7SpecHasExamples = &v
	}
	// Column 32: m7_spec_has_constraints
	if stmt.ColumnType(32) != sqlite.TypeNull {
		v := stmt.ColumnInt(32) == 1
		m.M7SpecHasConstraints = &v
	}
	// Column 33: computed_at
	if stmt.ColumnType(33) != sqlite.TypeNull {
		v := stmt.ColumnInt64(33)
		m.ComputedAt = &v
	}
	// Column 35: cost_input_usd (v3)
	if stmt.ColumnType(35) != sqlite.TypeNull {
		v := stmt.ColumnFloat(35)
		m.CostInputUSD = &v
	}
	// Column 36: cost_output_usd
	if stmt.ColumnType(36) != sqlite.TypeNull {
		v := stmt.ColumnFloat(36)
		m.CostOutputUSD = &v
	}
	// Column 37: cost_reasoning_usd
	if stmt.ColumnType(37) != sqlite.TypeNull {
		v := stmt.ColumnFloat(37)
		m.CostReasoningUSD = &v
	}
	// Column 38: cost_cache_read_usd
	if stmt.ColumnType(38) != sqlite.TypeNull {
		v := stmt.ColumnFloat(38)
		m.CostCacheReadUSD = &v
	}
	// Column 39: cost_cache_write_usd
	if stmt.ColumnType(39) != sqlite.TypeNull {
		v := stmt.ColumnFloat(39)
		m.CostCacheWriteUSD = &v
	}
	// Column 40: cost_total_usd
	if stmt.ColumnType(40) != sqlite.TypeNull {
		v := stmt.ColumnFloat(40)
		m.CostTotalUSD = &v
	}
	// Column 41: cost_model_id
	if stmt.ColumnType(41) != sqlite.TypeNull {
		v := stmt.ColumnText(41)
		m.CostModelID = &v
	}
	// Column 42: scope
	if stmt.ColumnType(42) != sqlite.TypeNull {
		v := stmt.ColumnText(42)
		m.Scope = &v
	}

	return m
}

// scanSessionEntry reads a SessionEntry from the current statement.
// Column order must match sqlListEntries.
func scanSessionEntry(stmt *sqlite.Stmt) schema.SessionEntry {
	e := schema.SessionEntry{
		SessionID:   schema.SessionID(stmt.ColumnText(0)),
		EntryIndex:  stmt.ColumnInt(1),
		Harness:     schema.Harness(stmt.ColumnText(2)),
		EntryType:   schema.EntryType(stmt.ColumnText(3)),
		Role:        schema.Role(stmt.ColumnText(4)),
		HasToolUse:  stmt.ColumnInt(9) == 1,
		HasThinking: stmt.ColumnInt(11) == 1,
		IsError:     stmt.ColumnInt(12) == 1,
	}
	// Column 5: timestamp_ms
	if stmt.ColumnType(5) != sqlite.TypeNull {
		v := stmt.ColumnInt64(5)
		e.TimestampMs = &v
	}
	// Column 6: content_preview
	if stmt.ColumnType(6) != sqlite.TypeNull {
		v := stmt.ColumnText(6)
		e.ContentPreview = &v
	}
	// Column 7: tokens_in
	if stmt.ColumnType(7) != sqlite.TypeNull {
		v := stmt.ColumnInt(7)
		e.TokensIn = &v
	}
	// Column 8: tokens_out
	if stmt.ColumnType(8) != sqlite.TypeNull {
		v := stmt.ColumnInt(8)
		e.TokensOut = &v
	}
	// Column 10: tool_names_csv
	if stmt.ColumnType(10) != sqlite.TypeNull {
		v := stmt.ColumnText(10)
		e.ToolNamesCSV = &v
	}
	// Column 13: raw_byte_length
	if stmt.ColumnType(13) != sqlite.TypeNull {
		v := stmt.ColumnInt(13)
		e.RawByteLength = &v
	}
	// Column 14: tool_call_id
	if stmt.ColumnType(14) != sqlite.TypeNull {
		v := stmt.ColumnText(14)
		e.ToolCallID = &v
	}
	// Column 15: entry_id
	if stmt.ColumnType(15) != sqlite.TypeNull {
		v := stmt.ColumnText(15)
		e.EntryID = &v
	}
	// Column 16: parent_entry_id
	if stmt.ColumnType(16) != sqlite.TypeNull {
		v := stmt.ColumnText(16)
		e.ParentEntryID = &v
	}
	// Column 17: extra
	if stmt.ColumnType(17) != sqlite.TypeNull {
		v := stmt.ColumnText(17)
		e.Extra = &v
	}
	// Column 18: depth (v2 full-depth indexing)
	e.Depth = stmt.ColumnInt(18)
	// Column 19: parent_index
	if stmt.ColumnType(19) != sqlite.TypeNull {
		v := stmt.ColumnInt(19)
		e.ParentIndex = &v
	}
	// Column 20: tool_input
	if stmt.ColumnType(20) != sqlite.TypeNull {
		v := stmt.ColumnText(20)
		e.ToolInput = &v
	}
	// Column 21: tool_output
	if stmt.ColumnType(21) != sqlite.TypeNull {
		v := stmt.ColumnText(21)
		e.ToolOutput = &v
	}
	// Column 22: tool_kind (v11 ACP-aligned)
	if stmt.ColumnType(22) != sqlite.TypeNull {
		v := schema.ToolCallKind(stmt.ColumnText(22))
		e.ToolKind = &v
	}
	// Column 23: stop_reason (v11 ACP-aligned)
	if stmt.ColumnType(23) != sqlite.TypeNull {
		v := schema.StopReason(stmt.ColumnText(23))
		e.StopReason = &v
	}
	// Column 24: part_type (v26)
	if stmt.ColumnType(24) != sqlite.TypeNull {
		v := stmt.ColumnText(24)
		e.PartType = &v
	}
	return e
}
