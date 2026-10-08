package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// ErrNoCapturedStats marks a session with no session_captured_stats row.
// Native sessions gain their row from the v62 backfill and keep it through
// capture, resume, and derived updates; a missing row means a non-native
// session or a store that predates the backfill, and its stats read as
// unknown (zeros and absent pointers on the wire).
var ErrNoCapturedStats = errors.New("store: the session has no captured stats row; its measurements are unknown")

// capturedStatsColumns is the session_captured_stats column order this file
// scans. It matches the v62 table definition field for field.
const capturedStatsColumns = `session_id, turn_count, input_submission_count,
    tool_call_count, subagent_count, duration_ms, tokens_in, tokens_out,
    thought_tokens, cached_read_tokens, cached_write_tokens,
    seed_json, source, updated_at_ms, overflow`

// ReadCapturedStats returns one session's mutable measurements with a single
// primary-key read. NULL columns stay nil (unknown); the wire mapping
// (capturedStatsToWire) decides how unknown renders.
func (s *Store) ReadCapturedStats(ctx context.Context, id schema.SessionID) (CapturedStats, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return CapturedStats{}, fmt.Errorf("store: take connection for captured stats %s: %w", id, err)
	}
	defer s.pool.Put(conn)
	return readCapturedStatsOnConn(conn, id)
}

// readCapturedStatsOnConn borrows an already-open read transaction so the
// snapshot builder can read stats in the same snapshot as the catalog.
func readCapturedStatsOnConn(conn *sqlite.Conn, id schema.SessionID) (CapturedStats, error) {
	stats := CapturedStats{SessionID: id}
	found := false
	err := sqlitex.ExecuteTransient(conn, `SELECT `+capturedStatsColumns+` FROM session_captured_stats WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			return scanCapturedStats(stmt, &stats)
		},
	})
	if err != nil {
		return CapturedStats{}, fmt.Errorf("store: read captured stats for session %s: %w; no measurements were returned", id, err)
	}
	if !found {
		return CapturedStats{}, fmt.Errorf("%w: session %s", ErrNoCapturedStats, id)
	}
	return stats, nil
}

// scanCapturedStats reads one session_captured_stats row. NULL means unknown
// for every measurement; source is validated at this trust boundary.
func scanCapturedStats(stmt *sqlite.Stmt, stats *CapturedStats) error {
	stats.TurnCount = nullableIntCol(stmt, 1)
	stats.InputSubmissionCount = nullableInt64Col(stmt, 2)
	stats.ToolCallCount = nullableIntCol(stmt, 3)
	stats.SubagentCount = nullableIntCol(stmt, 4)
	stats.DurationMs = nullableInt64Col(stmt, 5)
	stats.TokensIn = nullableIntCol(stmt, 6)
	stats.TokensOut = nullableIntCol(stmt, 7)
	stats.ThoughtTokens = nullableIntCol(stmt, 8)
	stats.CachedReadTokens = nullableIntCol(stmt, 9)
	stats.CachedWriteTokens = nullableIntCol(stmt, 10)
	if stmt.ColumnType(11) != sqlite.TypeNull {
		seed := stmt.ColumnText(11)
		stats.SeedJSON = &seed
	}
	source, err := NewStatsSource(stmt.ColumnText(12))
	if err != nil {
		return fmt.Errorf("scan captured stats for session %s: %w", stats.SessionID, err)
	}
	stats.Source = source
	stats.UpdatedAtMs = stmt.ColumnInt64(13)
	if stmt.ColumnType(14) != sqlite.TypeNull {
		overflow := stmt.ColumnText(14)
		stats.Overflow = &overflow
	}
	return nil
}

func nullableIntCol(stmt *sqlite.Stmt, col int) *int {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return nil
	}
	v := stmt.ColumnInt(col)
	return &v
}

func nullableInt64Col(stmt *sqlite.Stmt, col int) *int64 {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return nil
	}
	v := stmt.ColumnInt64(col)
	return &v
}

// nullableText reads one nullable TEXT column, preserving NULL against empty.
func nullableText(stmt *sqlite.Stmt, col int) *string {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return nil
	}
	v := stmt.ColumnText(col)
	return &v
}

// capturedStatsToWire applies the NULL-to-wire mapping: non-pointer wire
// fields read NULL as 0 (turnCount, toolCallCount, subagentCount, tokensIn,
// tokensOut, durationMs); omitempty pointers read NULL as absent
// (inputSubmissionCount, thoughtTokens, cachedReadTokens, cachedWriteTokens).
// This matches the retired behavior where a missing metric row also read as
// 0/absent; unknown stays distinguishable only at the stats layer.
func capturedStatsToWire(stats CapturedStats) schema.SessionStats {
	return schema.SessionStats{
		TurnCount:            derefOrZero(stats.TurnCount),
		InputSubmissionCount: stats.InputSubmissionCount,
		ToolCallCount:        derefOrZero(stats.ToolCallCount),
		SubagentCount:        derefOrZero(stats.SubagentCount),
		DurationMs:           derefOrZeroInt64(stats.DurationMs),
		TokensIn:             derefOrZero(stats.TokensIn),
		TokensOut:            derefOrZero(stats.TokensOut),
		ThoughtTokens:        stats.ThoughtTokens,
		CachedReadTokens:     stats.CachedReadTokens,
		CachedWriteTokens:    stats.CachedWriteTokens,
	}
}

func derefOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefOrZeroInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// sqlUpsertCapturedStats merges one stats update into session_captured_stats.
// A field the update omits keeps its stored value, so a partial capture never
// NULLs knowledge; overflow merges the same way. An update older than the
// stored updated_at_ms is ignored, and equal timestamps allow an idempotent
// rewrite. source records the latest update's origin as a row label.
//
// The seed rule is enforced here, not by convention: only a harness update
// may write seed_json. A harness update carrying no seed document clears the
// column — the latest harness knowledge holds no adapter evidence, so a stale
// document must not survive as evidence. A derived update never touches it, so
// computed output can never be consumed as adapter evidence even after a
// partial harness update relabels the row.
const sqlUpsertCapturedStats = `INSERT INTO session_captured_stats (
    session_id, turn_count, input_submission_count, tool_call_count,
    subagent_count, duration_ms, tokens_in, tokens_out, thought_tokens,
    cached_read_tokens, cached_write_tokens, seed_json, source, updated_at_ms,
    overflow
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
    turn_count = COALESCE(excluded.turn_count, session_captured_stats.turn_count),
    input_submission_count = COALESCE(excluded.input_submission_count, session_captured_stats.input_submission_count),
    tool_call_count = COALESCE(excluded.tool_call_count, session_captured_stats.tool_call_count),
    subagent_count = COALESCE(excluded.subagent_count, session_captured_stats.subagent_count),
    duration_ms = COALESCE(excluded.duration_ms, session_captured_stats.duration_ms),
    tokens_in = COALESCE(excluded.tokens_in, session_captured_stats.tokens_in),
    tokens_out = COALESCE(excluded.tokens_out, session_captured_stats.tokens_out),
    thought_tokens = COALESCE(excluded.thought_tokens, session_captured_stats.thought_tokens),
    cached_read_tokens = COALESCE(excluded.cached_read_tokens, session_captured_stats.cached_read_tokens),
    cached_write_tokens = COALESCE(excluded.cached_write_tokens, session_captured_stats.cached_write_tokens),
    seed_json = CASE WHEN excluded.source = 'harness' THEN excluded.seed_json ELSE session_captured_stats.seed_json END,
    source = excluded.source,
    updated_at_ms = excluded.updated_at_ms,
    overflow = COALESCE(excluded.overflow, session_captured_stats.overflow)
WHERE excluded.updated_at_ms >= session_captured_stats.updated_at_ms`

// UpsertCapturedStats merges one stats update outside an ambient transaction:
// capture, resume, backfill (source harness) and COMPUTE derived updates.
// It also syncs the sessions.input_submission_count mirror in the same
// transaction, so list paths keep reading the mirror.
func (s *Store) UpsertCapturedStats(ctx context.Context, stats CapturedStats) (applied bool, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("store: take connection for captured stats upsert %s: %w", stats.SessionID, err)
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer end(&err)
	return upsertCapturedStatsOnConn(conn, stats)
}

// upsertCapturedStatsOnConn merges one update inside the caller's
// transaction. The activation commit calls this for its bookkeeping; COMPUTE
// calls it for derived values. It reports whether the update won the age
// gate; an older update is ignored without error.
func upsertCapturedStatsOnConn(conn *sqlite.Conn, stats CapturedStats) (bool, error) {
	if stats.SessionID == "" {
		return false, fmt.Errorf("store: upsert captured stats with an empty session identity; no measurements were merged; supply the owning session")
	}
	if !stats.Source.IsValid() {
		return false, fmt.Errorf("store: upsert captured stats for session %s with source %q outside the closed set; no measurements were merged; use harness or derived", stats.SessionID, string(stats.Source))
	}
	allowed, err := SeedWriteAllowed(stats.Source)
	if err != nil {
		return false, fmt.Errorf("store: upsert captured stats for session %s: %w", stats.SessionID, err)
	}
	seed := any(nil)
	if allowed && stats.SeedJSON != nil {
		seed = *stats.SeedJSON
	}
	if err := sqlitex.ExecuteTransient(conn, sqlUpsertCapturedStats, &sqlitex.ExecOptions{
		Args: []any{
			string(stats.SessionID),
			nullableArg(stats.TurnCount), nullableArg64(stats.InputSubmissionCount),
			nullableArg(stats.ToolCallCount), nullableArg(stats.SubagentCount),
			nullableArg64(stats.DurationMs),
			nullableArg(stats.TokensIn), nullableArg(stats.TokensOut),
			nullableArg(stats.ThoughtTokens),
			nullableArg(stats.CachedReadTokens), nullableArg(stats.CachedWriteTokens),
			seed, string(stats.Source), stats.UpdatedAtMs, nullableStr(stats.Overflow),
		},
	}); err != nil {
		return false, fmt.Errorf("store: upsert captured stats for session %s: %w; prior measurements are unchanged", stats.SessionID, err)
	}
	applied := conn.Changes() > 0
	if err := syncInputSubmissionMirrorOnConn(conn, stats.SessionID); err != nil {
		return false, err
	}
	if err := clearLegacySeedOnConn(conn, stats.SessionID); err != nil {
		return false, err
	}
	return applied, nil
}

// clearLegacySeedOnConn clears the retired sessions seed column once a
// native session owns a captured stats row: the harness-only seed home
// carries the evidence from here on, and the retired column must not keep a
// stale document beside it. Sessions without an active generation are never
// touched; their seed still serves the legacy path.
func clearLegacySeedOnConn(conn *sqlite.Conn, id schema.SessionID) error {
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET metric_seed_json = NULL
WHERE session_id = ? AND active_generation_id IS NOT NULL AND metric_seed_json IS NOT NULL`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
	}); err != nil {
		return fmt.Errorf("store: clear retired metric seed for session %s: %w", id, err)
	}
	return nil
}
// syncInputSubmissionMirrorOnConn repoints the sessions mirror at the merged
// row, so list and grouping paths keep serving one narrow row. The mirror is
// a copy of the row's current value: an update that reports no count leaves
// the mirror exactly where the row already is.
func syncInputSubmissionMirrorOnConn(conn *sqlite.Conn, id schema.SessionID) error {
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET input_submission_count = (SELECT input_submission_count FROM session_captured_stats WHERE session_id = ?) WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id), string(id)},
	}); err != nil {
		return fmt.Errorf("store: sync input submission mirror for session %s: %w", id, err)
	}
	return nil
}

func nullableArg(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableArg64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// upsertActivationStatsOnConn records one activation's harness measurements
// in the captured stats row: every reported field wins, the capture's stats
// document becomes the seed, and the sessions input-submission mirror moves
// in the same transaction. The writer slice owns the full activation
// bookkeeping (C4); this bridge feeds the moved readers until it lands, and
// the merge replaces it with the full bookkeeping call.
func upsertActivationStatsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, stats schema.SessionStats) (bool, error) {
	seed, err := json.Marshal(stats)
	if err != nil {
		return false, fmt.Errorf("store: encode activation stats seed for session %s: %w; no measurements were merged", sessionID, err)
	}
	seedDoc := string(seed)
	turnCount := stats.TurnCount
	toolCallCount := stats.ToolCallCount
	subagentCount := stats.SubagentCount
	durationMs := stats.DurationMs
	tokensIn := stats.TokensIn
	tokensOut := stats.TokensOut
	return upsertCapturedStatsOnConn(conn, CapturedStats{
		SessionID:            sessionID,
		TurnCount:            &turnCount,
		InputSubmissionCount: stats.InputSubmissionCount,
		ToolCallCount:        &toolCallCount,
		SubagentCount:        &subagentCount,
		DurationMs:           &durationMs,
		TokensIn:             &tokensIn,
		TokensOut:            &tokensOut,
		ThoughtTokens:        stats.ThoughtTokens,
		CachedReadTokens:     stats.CachedReadTokens,
		CachedWriteTokens:    stats.CachedWriteTokens,
		SeedJSON:             &seedDoc,
		Source:               StatsSourceHarness,
		UpdatedAtMs:          time.Now().UnixMilli(),
	})
}

// ReadMetricSeed returns the harness-captured adapter statistics for one
// session, or nil when the seed is unknown. Native sessions read the
// harness-only seed_json home: a COMPUTE-only row has no seed, a derived
// update leaves a captured seed unchanged, and a partial harness update
// without a document clears it, so computed output is never consumed as
// adapter evidence. Non-native sessions keep the legacy sessions column.
func (s *Store) ReadMetricSeed(ctx context.Context, id schema.SessionID) (*ingest.StatsInfo, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for metric seed %s: %w; retry when database access is available", id, err)
	}
	defer s.pool.Put(conn)
	return readMetricSeedOnConn(conn, id)
}

// readMetricSeedOnConn borrows an already-open connection so ingest paths can
// read the seed in their own snapshot.
func readMetricSeedOnConn(conn *sqlite.Conn, id schema.SessionID) (*ingest.StatsInfo, error) {
	native, err := isNativeSessionOnConn(conn, id)
	if err != nil {
		return nil, err
	}
	if !native {
		return getMetricSeedOnConn(conn, ingest.SessionID(id))
	}
	var seed *ingest.StatsInfo
	err = sqlitex.ExecuteTransient(conn, `SELECT seed_json FROM session_captured_stats WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) == sqlite.TypeNull {
				return nil
			}
			return decodeSeedDocument(stmt.ColumnText(0), &seed)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: decode harness metric seed for session %s before computation: %w; prior metrics were preserved", id, err)
	}
	return seed, nil
}

// decodeSeedDocument parses one harness seed document into adapter
// statistics. Only a JSON object is evidence; anything else fails closed so a
// malformed document is never mistaken for unknown input.
func decodeSeedDocument(raw string, seed **ingest.StatsInfo) error {
	trimmed := raw
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '\n' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("retained metric seed must be a JSON object; NULL alone represents unknown input")
	}
	*seed = &ingest.StatsInfo{}
	return json.Unmarshal([]byte(raw), *seed)
}

// isNativeSessionOnConn reports whether a session owns an active generation
// in either catalog table. Native sessions read the harmonized stats home;
// sessions without an active generation keep the legacy representation.
func isNativeSessionOnConn(conn *sqlite.Conn, id schema.SessionID) (bool, error) {
	native := false
	err := sqlitex.ExecuteTransient(conn, `SELECT s.active_generation_id IS NOT NULL FROM sessions s WHERE s.session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			native = stmt.ColumnInt(0) == 1
			return nil
		},
	})
	if err != nil {
		return false, fmt.Errorf("store: resolve representation for session %s: %w", id, err)
	}
	return native, nil
}
