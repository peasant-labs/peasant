package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SQL constant for saving v2 session metrics.
const sqlSaveMetrics = `INSERT OR REPLACE INTO session_metrics (
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
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// SaveMetrics persists a SessionMetrics row via INSERT OR REPLACE.
// All nullable fields use nil → SQL NULL binding.
func (s *Store) SaveMetrics(ctx context.Context, m *ingest.SessionMetrics) (err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err = sqlitex.ExecuteTransient(conn, sqlSaveMetrics, &sqlitex.ExecOptions{
		Args: []any{
			string(m.SessionID),
			derefInt(m.TurnCount),
			derefInt(m.SubagentCount),
			derefString2(m.TitleGenerated),
			derefSessionOutcome(m.Outcome),
			derefInt(m.TotalTokens),
			derefInt(m.InputTokens),
			derefInt(m.OutputTokens),
			derefInt(m.ToolCalls),
			derefInt(m.FilesTouched),
			derefInt(m.LinesChanged),
			derefFloat64(m.DurationMinutes),
			derefInt(m.RetryLoops),
			derefInt(m.RetryTokensWasted),
			derefInt(m.WithinSessionReverts),
			derefFloat64(m.SignalDensity),
			derefFloat64(m.SpecQualityScore),
			derefFloat64(m.ExplorationRatio),
			derefInt(m.ScopeBreadth),
			derefInt(m.DiscoveryTurns),
			derefFloat64(m.M2TokenOutcomeRatio),
			derefInt(m.M3UniqueToolCount),
			derefInt(m.M4ErrorRecoveryCount),
			derefInt(m.M4ConsecutiveErrorMax),
			derefFloat64(m.M5ContextUtilizationPct),
			derefInt(m.M5PeakContextTokens),
			derefInt(m.M5AvgMessageTokens),
			derefFloat64(m.M6OutputSurvivalPct),
			derefInt(m.M6LinesSurvived),
			derefInt(m.M6LinesTotal),
			derefInt(m.M7SpecWordCount),
			derefBoolToInt(m.M7SpecHasExamples),
			derefBoolToInt(m.M7SpecHasConstraints),
			derefInt64(m.ComputedAt),
			derefInt(m.ComputeVersion),
			// v3 cost columns (indices 35-41)
			derefFloat64(m.CostInputUSD),
			derefFloat64(m.CostOutputUSD),
			derefFloat64(m.CostReasoningUSD),
			derefFloat64(m.CostCacheReadUSD),
			derefFloat64(m.CostCacheWriteUSD),
			derefFloat64(m.CostTotalUSD),
			derefString2(m.CostModelID),
			// v3 scope column (index 42)
			derefString2(m.Scope),
		},
	}); err != nil {
		return fmt.Errorf("store: save metrics for %s: %w", m.SessionID, err)
	}

	return nil
}

func derefSessionOutcome(p *ingest.SessionOutcome) any {
	if p == nil {
		return nil
	}
	return string(*p)
}

func derefFloat64(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
