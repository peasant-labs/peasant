// Package metrics provides the metrics computation engine for Peasant.
// It computes session-level metrics from indexed session_entries
// and persists the results to session_metrics via MetricsStore.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/title"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// CurrentComputeVersion is the compute_version written by this build.
// Increment when MetricFunc logic changes to trigger recomputation.
const CurrentComputeVersion = 6

// MetricFunc computes a partial SessionMetrics update from session entries.
// It receives the session's entries and existing metrics, and returns a
// partial SessionMetrics with only the computed fields populated.
// Other fields remain nil/zero and are merged by the engine.
type MetricFunc func(ctx context.Context, sessionID ingest.SessionID, entries []schema.SessionEntry, existing *ingest.SessionMetrics) *ingest.SessionMetrics

// Compile-time guard: Engine must implement SessionAnalyzer.
var _ ingest.SessionAnalyzer = (*Engine)(nil)

// Engine orchestrates metric computation for sessions.
type Engine struct {
	store  ingest.MetricsStore
	funcs  []namedMetricFunc
	force  bool
	titles title.Pipeline
}

type namedMetricFunc struct {
	name string
	fn   MetricFunc
}

// NewEngine creates an Engine with the default set of MetricFuncs.
// Models-dependent MetricFuncs (contextUtilization, cost) use the hardcoded
// fallback (200K context window, no cost data). Use NewEngineWithModels for
// real model lookups.
func NewEngine(store ingest.MetricsStore) *Engine {
	e := newEngine(store)
	e.funcs = defaultMetricFuncs(nil, nil, e.computeTitle)
	return e
}

// NewEngineWithModels creates an Engine that uses the ModelsSyncer for
// context window lookups and cost computation via the closure pattern.
func NewEngineWithModels(store ingest.MetricsStore, syncer ingest.ModelsSyncer) *Engine {
	e := newEngine(store)
	e.funcs = defaultMetricFuncs(syncer, nil, e.computeTitle)
	return e
}

// NewEngineWithAll creates an Engine with ModelsSyncer for cost/context lookups
// and GitDiffAnalyzer for M6 output survival computation.
func NewEngineWithAll(store ingest.MetricsStore, syncer ingest.ModelsSyncer, analyzer ingest.GitDiffAnalyzer) *Engine {
	e := newEngine(store)
	e.funcs = defaultMetricFuncs(syncer, analyzer, e.computeTitle)
	return e
}

func newEngine(store ingest.MetricsStore) *Engine {
	pipeline, err := title.Default()
	if err != nil {
		slog.Error("metrics: initialize canonical title pipeline", "error", err)
	}
	return &Engine{store: store, titles: pipeline}
}

func (e *Engine) computeTitle(ctx context.Context, sessionID ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	if e.titles == nil {
		return nil
	}
	var preview string
	for i := range entries {
		if entries[i].Depth == 0 && entries[i].Role == ingest.RoleUser && entries[i].ContentPreview != nil {
			preview = *entries[i].ContentPreview
			break
		}
	}
	if preview == "" {
		return nil
	}
	harness, projectPath, err := e.store.GetTitleContext(ctx, sessionID)
	if err != nil || harness == "" || projectPath == "" {
		slog.Warn("metrics: load complete title context; generated title omitted", "session_id", sessionID, "error", err)
		return nil
	}
	result, err := e.titles.Generate(preview, redact.TitleContext{Harness: harness, ProjectPath: projectPath})
	if err != nil {
		slog.Warn("metrics: generate canonical title; generated title omitted", "session_id", sessionID, "error", err)
		return nil
	}
	if result.Text == "" {
		return nil
	}
	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{TitleGenerated: &result.Text}}
}

// SetForce enables or disables force mode. When true, ComputeMetrics
// skips the compute_version check and recomputes all sessions.
func (e *Engine) SetForce(force bool) {
	e.force = force
}

// ComputeMetrics computes metrics for the given sessions.
// Returns the count of sessions that were actually (re)computed.
// Sessions that already have compute_version >= CurrentComputeVersion
// are skipped unless Force is true.
func (e *Engine) ComputeMetrics(ctx context.Context, sessionIDs []ingest.SessionID) (int, error) {
	computed := 0

	for _, sid := range sessionIDs {
		if ctx.Err() != nil {
			return computed, ctx.Err()
		}

		// Skip if already computed at current version (unless forced).
		if !e.force {
			exists, err := e.store.MetricsExist(ctx, sid, CurrentComputeVersion)
			if err != nil {
				slog.Warn("metrics: check version", "session_id", sid, "error", err)
				continue
			}
			if exists {
				continue
			}
		}

		// Load session entries.
		entries, err := e.store.ListEntries(ctx, sid)
		if err != nil {
			slog.Warn("metrics: list entries", "session_id", sid, "error", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}

		// Load existing metrics (may be nil for first computation).
		existing, err := e.store.GetMetrics(ctx, sid)
		if err != nil {
			slog.Warn("metrics: get existing", "session_id", sid, "error", err)
		}

		// Run all MetricFuncs and merge results.
		merged := &ingest.SessionMetrics{
			SessionID: sid,
		}

		// Preserve retained v1 fields from existing metrics.
		if existing != nil {
			merged.TurnCount = existing.TurnCount
			merged.SubagentCount = existing.SubagentCount
			merged.InputTokens = existing.InputTokens
			merged.OutputTokens = existing.OutputTokens
			merged.ToolCalls = existing.ToolCalls
			merged.DurationMinutes = existing.DurationMinutes
		}

		for _, nf := range e.funcs {
			result := nf.fn(ctx, sid, entries, existing)
			if result != nil {
				mergeSessionMetrics(merged, result)
			}
		}

		// Set metadata.
		now := time.Now().UnixMilli()
		merged.ComputedAt = &now
		v := CurrentComputeVersion
		merged.ComputeVersion = &v

		// Persist.
		if err := e.store.SaveMetrics(ctx, merged); err != nil {
			return computed, fmt.Errorf("metrics: save %s: %w", sid, err)
		}
		computed++
	}

	return computed, nil
}

// ComputeInsights delegates to UpdateDailySummary on the MetricsStore.
// This is a thin wrapper — the SQL aggregation already exists in the store.
func (e *Engine) ComputeInsights(ctx context.Context, days []string) error {
	return e.store.UpdateDailySummary(ctx, days)
}

// mergeSessionMetrics copies non-nil fields from src into dst.
func mergeSessionMetrics(dst, src *ingest.SessionMetrics) {
	if src.TurnCount != nil {
		dst.TurnCount = src.TurnCount
	}
	if src.SubagentCount != nil {
		dst.SubagentCount = src.SubagentCount
	}
	if src.TitleGenerated != nil {
		dst.TitleGenerated = src.TitleGenerated
	}
	if src.Outcome != nil {
		dst.Outcome = src.Outcome
	}
	if src.TotalTokens != nil {
		dst.TotalTokens = src.TotalTokens
	}
	if src.InputTokens != nil {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens != nil {
		dst.OutputTokens = src.OutputTokens
	}
	if src.ToolCalls != nil {
		dst.ToolCalls = src.ToolCalls
	}
	if src.FilesTouched != nil {
		dst.FilesTouched = src.FilesTouched
	}
	if src.LinesChanged != nil {
		dst.LinesChanged = src.LinesChanged
	}
	if src.DurationMinutes != nil {
		dst.DurationMinutes = src.DurationMinutes
	}
	if src.RetryLoops != nil {
		dst.RetryLoops = src.RetryLoops
	}
	if src.RetryTokensWasted != nil {
		dst.RetryTokensWasted = src.RetryTokensWasted
	}
	if src.WithinSessionReverts != nil {
		dst.WithinSessionReverts = src.WithinSessionReverts
	}
	if src.SignalDensity != nil {
		dst.SignalDensity = src.SignalDensity
	}
	if src.SpecQualityScore != nil {
		dst.SpecQualityScore = src.SpecQualityScore
	}
	if src.ExplorationRatio != nil {
		dst.ExplorationRatio = src.ExplorationRatio
	}
	if src.ScopeBreadth != nil {
		dst.ScopeBreadth = src.ScopeBreadth
	}
	if src.DiscoveryTurns != nil {
		dst.DiscoveryTurns = src.DiscoveryTurns
	}
	// M-series fields (M2-M7).
	if src.M2TokenOutcomeRatio != nil {
		dst.M2TokenOutcomeRatio = src.M2TokenOutcomeRatio
	}
	if src.M3UniqueToolCount != nil {
		dst.M3UniqueToolCount = src.M3UniqueToolCount
	}
	if src.M4ErrorRecoveryCount != nil {
		dst.M4ErrorRecoveryCount = src.M4ErrorRecoveryCount
	}
	if src.M4ConsecutiveErrorMax != nil {
		dst.M4ConsecutiveErrorMax = src.M4ConsecutiveErrorMax
	}
	if src.M5ContextUtilizationPct != nil {
		dst.M5ContextUtilizationPct = src.M5ContextUtilizationPct
	}
	if src.M5PeakContextTokens != nil {
		dst.M5PeakContextTokens = src.M5PeakContextTokens
	}
	if src.M5AvgMessageTokens != nil {
		dst.M5AvgMessageTokens = src.M5AvgMessageTokens
	}
	if src.M6OutputSurvivalPct != nil {
		dst.M6OutputSurvivalPct = src.M6OutputSurvivalPct
	}
	if src.M6LinesSurvived != nil {
		dst.M6LinesSurvived = src.M6LinesSurvived
	}
	if src.M6LinesTotal != nil {
		dst.M6LinesTotal = src.M6LinesTotal
	}
	if src.M7SpecWordCount != nil {
		dst.M7SpecWordCount = src.M7SpecWordCount
	}
	// M7 booleans: merge true values (any MetricFunc setting true wins).
	if src.M7SpecHasExamples != nil {
		if dst.M7SpecHasExamples == nil || *src.M7SpecHasExamples {
			dst.M7SpecHasExamples = src.M7SpecHasExamples
		}
	}
	if src.M7SpecHasConstraints != nil {
		if dst.M7SpecHasConstraints == nil || *src.M7SpecHasConstraints {
			dst.M7SpecHasConstraints = src.M7SpecHasConstraints
		}
	}
	// v3 cost fields.
	if src.CostInputUSD != nil {
		dst.CostInputUSD = src.CostInputUSD
	}
	if src.CostOutputUSD != nil {
		dst.CostOutputUSD = src.CostOutputUSD
	}
	if src.CostReasoningUSD != nil {
		dst.CostReasoningUSD = src.CostReasoningUSD
	}
	if src.CostCacheReadUSD != nil {
		dst.CostCacheReadUSD = src.CostCacheReadUSD
	}
	if src.CostCacheWriteUSD != nil {
		dst.CostCacheWriteUSD = src.CostCacheWriteUSD
	}
	if src.CostTotalUSD != nil {
		dst.CostTotalUSD = src.CostTotalUSD
	}
	if src.CostModelID != nil {
		dst.CostModelID = src.CostModelID
	}
	if src.Scope != nil {
		dst.Scope = src.Scope
	}
}
