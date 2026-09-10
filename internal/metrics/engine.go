// Package metrics provides the metrics computation engine for Peasant.
// It computes session-level metrics from indexed session_entries
// and persists the results to session_metrics via MetricsStore.
package metrics

import (
	"context"
	"errors"
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
const CurrentComputeVersion = 8

// MetricFunc computes a partial SessionMetrics update from session entries.
// It receives the session's entries and existing metrics, and returns a
// partial SessionMetrics with only the computed fields populated.
// Other fields remain nil/zero and are merged by the engine.
type MetricFunc func(ctx context.Context, sessionID ingest.SessionID, entries []schema.SessionEntry, existing *ingest.SessionMetrics) *ingest.SessionMetrics

// Compile-time guard: Engine must implement SessionAnalyzer.
var _ ingest.SessionAnalyzer = (*Engine)(nil)
var _ ingest.MetricsRecomputer = (*Engine)(nil)

// Engine orchestrates metric computation for sessions.
type Engine struct {
	store  ingest.MetricsStore
	funcs  []namedMetricFunc
	force  bool
	titles title.Pipeline
	models ingest.ModelsSyncer
	git    ingest.GitDiffAnalyzer
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
	e.models = syncer
	e.funcs = defaultMetricFuncs(syncer, nil, e.computeTitle)
	return e
}

// NewEngineWithAll creates an Engine with ModelsSyncer for cost/context lookups
// and GitDiffAnalyzer for M6 output survival computation.
func NewEngineWithAll(store ingest.MetricsStore, syncer ingest.ModelsSyncer, analyzer ingest.GitDiffAnalyzer) *Engine {
	e := newEngine(store)
	e.models, e.git = syncer, analyzer
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

// computeTitle derives the published title from the first user turn that
// carries real user prose. Every depth-0 turn whose stored role is user is a
// candidate, in transcript order, and the canonical pipeline decides which one
// is usable: a turn that cleans to empty text held only harness-injected markup,
// and a turn that fails to clean is unusable and must never be exposed raw.
// Both are skipped in favour of the next candidate. When no candidate is usable
// the metric is omitted and the generic harness fallback applies downstream.
func (e *Engine) computeTitle(ctx context.Context, sessionID ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	harness, projectPath, err := e.store.GetTitleContext(ctx, sessionID)
	if err != nil {
		slog.Warn("metrics: load title context", "session_id", sessionID, "error", err)
		return nil
	}
	return e.computeTitleWithContext(sessionID, entries, harness, projectPath)
}

func (e *Engine) computeTitleWithContext(sessionID ingest.SessionID, entries []schema.SessionEntry, harness schema.Harness, projectPath string) *ingest.SessionMetrics {
	if e.titles == nil {
		return nil
	}
	var candidates []string
	for i := range entries {
		if entries[i].Depth == 0 && entries[i].Role == ingest.RoleUser && entries[i].ContentPreview != nil && *entries[i].ContentPreview != "" {
			candidates = append(candidates, *entries[i].ContentPreview)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if harness == "" || projectPath == "" {
		slog.Warn("metrics: incomplete title context; generated title omitted", "session_id", sessionID)
		return nil
	}
	result, index, skipped := e.titles.GenerateFromTurns(candidates, redact.TitleContext{Harness: harness, ProjectPath: projectPath})
	// The pipeline reports one error per unusable candidate, in candidate order,
	// but a candidate skipped for holding no prose reports no error, so the
	// ordinal below counts unusable candidates rather than transcript turns.
	for order, skipErr := range skipped {
		slog.Warn("metrics: skip unusable user turn while generating the title", "session_id", sessionID, "unusable_candidate", order+1, "user_turn_candidates", len(candidates), "error", skipErr)
	}
	if index == -1 {
		return nil
	}
	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{TitleGenerated: &result.Text}}
}

// applyNativeSessionName overrides the generated title with the session name
// the harness itself recorded, when the indexed rows carry one. It is the one
// rule for both compute paths: the stored-input path and the older
// list-entries path call it, so a session shows the same title whichever path
// computed it, and a native name can never reach the store unsanitized.
//
// An explicit clear (a recorded empty name) clears the title rather than
// falling back to generated prose: the user removed the name on purpose.
// A recorded non-empty name is user-written text from outside Peasant, so it
// passes the same title privacy policy as every outward title before it is
// stored. resolveTitleContext is called only when there is a name to sanitize,
// so a session with no native name costs no extra lookup.
func (e *Engine) applyNativeSessionName(merged *ingest.SessionMetrics, indexed []schema.SessionEntry, resolveTitleContext func() (schema.Harness, string, error)) error {
	nativeName, err := recordedNativeSessionName(indexed)
	if err != nil {
		return err
	}
	if nativeName == nil {
		return nil
	}
	name := ""
	if *nativeName != "" && e.titles != nil {
		harness, projectPath, err := resolveTitleContext()
		if err != nil {
			return fmt.Errorf("read the title context of session %s before applying its recorded harness-native name: %w; prior metrics were preserved; restore the session's stored harness and project path, then recompute", merged.SessionID, err)
		}
		result, err := e.titles.Sanitize(*nativeName, redact.TitleContext{Harness: harness, ProjectPath: projectPath})
		if err != nil {
			return fmt.Errorf("apply the title privacy policy to the recorded harness-native name of session %s: %w; the unsanitized name was not stored; correct the policy input and recompute", merged.SessionID, err)
		}
		name = result.Text
	}
	merged.TitleGenerated = &name
	return nil
}

// recordedNativeSessionName reports the last harness-native session name the
// indexed rows carry, or nil when no row records one. A recorded empty name is
// an explicit clear and is reported as a non-nil empty string, which is why the
// result is a pointer.
func recordedNativeSessionName(indexed []schema.SessionEntry) (*string, error) {
	var nativeName *string
	for _, entry := range indexed {
		if entry.Harness != schema.HarnessPi {
			continue
		}
		extra, _, err := ingest.DecodePiEntryExtra(entry)
		if err != nil {
			return nil, fmt.Errorf("read the typed evidence of indexed row %d of session %s while looking for its harness-native name: %w; prior metrics were preserved; reindex the session, then recompute", entry.EntryIndex, entry.SessionID, err)
		}
		if extra.SessionName != nil {
			nativeName = extra.SessionName
		}
	}
	return nativeName, nil
}

// SetForce enables or disables force mode. When true, ComputeMetrics
// skips the compute_version check and recomputes all sessions.
func (e *Engine) SetForce(force bool) {
	e.force = force
}

// ComputeMetrics computes metrics for the given sessions.
// Returns the count of sessions that were actually (re)computed.
// Production stores skip only matching proven inputs at the current version.
func (e *Engine) ComputeMetrics(ctx context.Context, sessionIDs []ingest.SessionID) (int, error) {
	return e.computeMetrics(ctx, sessionIDs, false)
}

// RecomputeMetrics refreshes only the successful index targets supplied by the
// current pipeline invocation. Proven equal inputs need no recomputation.
func (e *Engine) RecomputeMetrics(ctx context.Context, sessionIDs []ingest.SessionID) (int, error) {
	if backing, ok := e.store.(ingest.MetricInputStore); ok {
		return e.computeCapturedMetrics(ctx, backing, sessionIDs)
	}
	forced := *e
	forced.force = true
	computed := 0
	var failures []error
	for _, sid := range sessionIDs {
		n, err := forced.computeMetrics(ctx, []ingest.SessionID{sid}, true)
		computed += n
		if err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return computed, errors.Join(failures...)
}

func (e *Engine) computeMetrics(ctx context.Context, sessionIDs []ingest.SessionID, reportFailures bool) (int, error) {
	if backing, ok := e.store.(ingest.MetricInputStore); ok {
		return e.computeCapturedMetrics(ctx, backing, sessionIDs)
	}
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
			if reportFailures {
				return computed, fmt.Errorf("read indexed entries before recomputing metrics for session %s: %w; prior metrics were preserved; restore database access and retry", sid, err)
			}
			slog.Warn("metrics: list entries", "session_id", sid, "error", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}

		// Load existing metrics (may be nil for first computation).
		existing, err := e.store.GetMetrics(ctx, sid)
		if err != nil {
			if reportFailures {
				return computed, fmt.Errorf("read metric producer before recomputing session %s: %w; prior metrics were preserved; restore database access and retry", sid, err)
			}
			slog.Warn("metrics: get existing", "session_id", sid, "error", err)
			continue
		}
		if existing != nil && existing.ComputeVersion != nil && *existing.ComputeVersion > CurrentComputeVersion {
			refusal := fmt.Errorf("recompute metrics for session %s: stored compute version %d is newer than this build's version %d; prior metrics and producer stamps were preserved; upgrade Peasant before retrying", sid, *existing.ComputeVersion, CurrentComputeVersion)
			if reportFailures {
				return computed, refusal
			}
			slog.Warn("metrics: newer producer refused", "session_id", sid, "error", refusal)
			continue
		}

		// Run all MetricFuncs and merge results. The native session name is
		// read from every indexed row, not only the conversational ones.
		indexed := entries
		entries = ingest.ConversationalEntries(entries)
		merged := &ingest.SessionMetrics{
			SessionID: sid,
		}

		// Retained adapter statistics are inputs, not prior computed output.
		// Missing historical seeds stay unknown; current metrics functions can
		// still derive their supported fields from indexed entries.
		if seeds, ok := e.store.(ingest.MetricSeedStore); ok {
			seed, seedErr := seeds.GetMetricSeed(ctx, sid)
			if seedErr != nil {
				if reportFailures {
					return computed, fmt.Errorf("read retained adapter inputs before recomputing metrics for session %s: %w; prior metrics were preserved; reconcile valid managed metadata and retry", sid, seedErr)
				}
				slog.Warn("metrics: read retained seed; prior metrics preserved", "session_id", sid, "error", seedErr)
				continue
			}
			if seed != nil {
				merged.TurnCount = &seed.TurnCount
				merged.SubagentCount = &seed.SubagentCount
				merged.InputTokens = &seed.TokensIn
				merged.OutputTokens = &seed.TokensOut
				merged.ToolCalls = &seed.ToolCallCount
				duration := float64(seed.DurationMs) / 60000
				merged.DurationMinutes = &duration
			}
		}

		for _, nf := range e.funcs {
			result := nf.fn(ctx, sid, entries, merged)
			if result != nil {
				mergeSessionMetrics(merged, result)
			}
		}
		if err := e.applyNativeSessionName(merged, indexed, func() (schema.Harness, string, error) {
			return e.store.GetTitleContext(ctx, sid)
		}); err != nil {
			return computed, err
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
