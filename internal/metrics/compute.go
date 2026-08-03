package metrics

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// defaultMetricFuncs returns the standard set of MetricFuncs.
// If syncer is non-nil, context utilization and cost use real model data;
// otherwise they use hardcoded fallbacks (200K context window, no cost).
func defaultMetricFuncs(syncer ingest.ModelsSyncer, analyzer ingest.GitDiffAnalyzer, titleFunc MetricFunc) []namedMetricFunc {
	funcs := []namedMetricFunc{
		{name: "tokens", fn: computeTokens},
		{name: "turns", fn: computeTurns},
		{name: "title", fn: titleFunc},
		{name: "outcome", fn: computeOutcome},
		{name: "duration", fn: computeDuration},
		{name: "retryLoops", fn: computeRetryLoops},
		{name: "specQuality", fn: computeSpecQuality},
		// v2 MetricFuncs — operate on in-memory entries with full-depth data.
		{name: "signalDensity", fn: computeSignalDensity},
		{name: "reverts", fn: computeReverts},
		{name: "fileAnalysis", fn: computeFileAnalysis},
		{name: "uniqueTools", fn: computeUniqueTools},
		{name: "errorRecovery", fn: computeErrorRecovery},
		{name: "contextUtilization", fn: makeContextUtilizationFunc(syncer)},
		{name: "tokenOutcomeRatio", fn: computeTokenOutcomeRatio},
		{name: "cost", fn: makeCostFunc(syncer)},
		{name: "scope", fn: computeScope},
		{name: "outputSurvival", fn: makeOutputSurvivalFunc(analyzer)},
	}
	return funcs
}

// computeTokens computes token metrics from session entries.
// InputTokens = peak context window usage (MAX per-entry input tokens).
// OutputTokens = total output tokens across all entries (SUM).
// TotalTokens = peak context (same as InputTokens — the meaningful session size).
func computeTokens(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	var peakIn, totalOut int
	var hasTokens bool

	for i := range entries {
		if entries[i].TokensIn != nil {
			if *entries[i].TokensIn > peakIn {
				peakIn = *entries[i].TokensIn
			}
			hasTokens = true
		}
		if entries[i].TokensOut != nil {
			totalOut += *entries[i].TokensOut
			hasTokens = true
		}
	}

	if !hasTokens {
		return nil
	}

	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{
		TotalTokens:  &peakIn,
		InputTokens:  &peakIn,
		OutputTokens: &totalOut,
	}}
}

// computeTurns counts user+assistant depth=0 turns, tool calls, and subagent entries.
func computeTurns(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	var turnCount, toolCalls, subagents int

	for i := range entries {
		// Count depth=0 user and assistant entries as turns.
		if entries[i].Depth == 0 && (entries[i].Role == ingest.RoleUser || entries[i].Role == ingest.RoleAssistant) {
			turnCount++
		}
		if entries[i].HasToolUse {
			// Count individual tools from CSV if available.
			if entries[i].ToolNamesCSV != nil {
				names := strings.Split(*entries[i].ToolNamesCSV, ",")
				toolCalls += len(names)
			} else {
				toolCalls++
			}
		}
		// Subagent heuristic: tool_use entries with "Task" in tool names.
		if entries[i].ToolNamesCSV != nil && strings.Contains(*entries[i].ToolNamesCSV, "Task") {
			subagents++
		}
	}

	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{
		TurnCount:     &turnCount,
		SubagentCount: &subagents,
		ToolCalls:     &toolCalls,
	}}
}

// computeOutcome classifies the session outcome from error patterns.
//
// Heuristic (using depth=0 user+assistant entries only):
//
// Failed:
//   - errorRate ≥ 0.3 (overall high)
//   - OR trailingAllErrors (last 3 entries all errors)
//   - OR maxConsecutiveErrors ≥ 5 (long stuck streak)
//   - OR tailErrorRate ≥ 0.5 (session collapsed at end)
//
// Partial:
//   - errorRate ≥ 0.05 (moderate)
//   - OR lastIsError (ended on error)
//   - OR maxConsecutiveErrors ≥ 3 (medium stuck streak)
//   - OR tailErrorRate ≥ 0.2 (notable error cluster at end)
//
// Resolved: everything else.
// No meaningful entries → nil.
func computeOutcome(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	// Filter to depth=0 user+assistant entries.
	var meaningful []schema.SessionEntry
	for i := range entries {
		if entries[i].Depth == 0 && (entries[i].Role == ingest.RoleUser || entries[i].Role == ingest.RoleAssistant) {
			meaningful = append(meaningful, entries[i])
		}
	}

	if len(meaningful) == 0 {
		return nil
	}

	n := len(meaningful)

	// Overall error rate.
	var errorCount int
	for i := range meaningful {
		if meaningful[i].IsError {
			errorCount++
		}
	}
	errorRate := float64(errorCount) / float64(n)

	// Trailing-errors rule: last 3 meaningful entries all have IsError.
	trailingAllErrors := false
	if n >= 3 {
		last3 := meaningful[n-3:]
		trailingAllErrors = last3[0].IsError && last3[1].IsError && last3[2].IsError
	}

	lastIsError := meaningful[n-1].IsError

	// Max consecutive errors: longest run of consecutive IsError=true entries.
	var maxConsecutiveErrors, currentStreak int
	for i := range meaningful {
		if meaningful[i].IsError {
			currentStreak++
			if currentStreak > maxConsecutiveErrors {
				maxConsecutiveErrors = currentStreak
			}
		} else {
			currentStreak = 0
		}
	}

	// Tail error rate: error concentration in the last quarter of the session.
	tailSize := n / 4
	if tailSize < 3 {
		tailSize = 3
	}
	if tailSize > n {
		tailSize = n
	}
	tail := meaningful[n-tailSize:]
	var tailErrors int
	for i := range tail {
		if tail[i].IsError {
			tailErrors++
		}
	}
	tailErrorRate := float64(tailErrors) / float64(len(tail))

	var outcome ingest.SessionOutcome
	switch {
	case errorRate >= 0.3 || trailingAllErrors || maxConsecutiveErrors >= 5 || tailErrorRate >= 0.5:
		outcome = ingest.OutcomeFailed
	case errorRate >= 0.05 || lastIsError || maxConsecutiveErrors >= 3 || tailErrorRate >= 0.2:
		outcome = ingest.OutcomePartial
	default:
		outcome = ingest.OutcomeResolved
	}

	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{Outcome: &outcome}}
}

// computeDuration calculates session duration from first to last timestamp.
func computeDuration(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	var minTs, maxTs int64
	var hasTs bool

	for i := range entries {
		if entries[i].TimestampMs == nil {
			continue
		}
		ts := *entries[i].TimestampMs
		if !hasTs {
			minTs = ts
			maxTs = ts
			hasTs = true
			continue
		}
		if ts < minTs {
			minTs = ts
		}
		if ts > maxTs {
			maxTs = ts
		}
	}

	if !hasTs || maxTs <= minTs {
		return nil
	}

	durationMin := float64(maxTs-minTs) / 60000.0
	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{DurationMinutes: &durationMin}}
}

// computeRetryLoops detects error retry loops across assistant turns.
// Consecutive depth=0 assistant entries with IsError=true form a streak.
// A streak of ≥2 counts as a retry loop. Wasted tokens prefer TokensIn+TokensOut,
// falling back to RawByteLength/4.
func computeRetryLoops(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	var retryLoops int
	var currentStreak int
	var lastAssistantWasError bool
	var totalWastedTokens int
	var streakTokens int

	for i := range entries {
		if entries[i].Depth != 0 || entries[i].Role != ingest.RoleAssistant {
			continue
		}

		if entries[i].IsError {
			currentStreak++
			streakTokens += entryTokenEstimate(&entries[i])
			lastAssistantWasError = true
		} else {
			if lastAssistantWasError && currentStreak >= 2 {
				retryLoops++
				totalWastedTokens += streakTokens
			}
			currentStreak = 0
			streakTokens = 0
			lastAssistantWasError = false
		}
	}

	// Handle trailing error streak.
	if lastAssistantWasError && currentStreak >= 2 {
		retryLoops++
		totalWastedTokens += streakTokens
	}

	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{
		RetryLoops:        &retryLoops,
		RetryTokensWasted: &totalWastedTokens,
	}}
}

// entryTokenEstimate returns token count for an entry, preferring real tokens
// over byte-length heuristic.
func entryTokenEstimate(e *schema.SessionEntry) int {
	var tokens int
	if e.TokensIn != nil {
		tokens += *e.TokensIn
	}
	if e.TokensOut != nil {
		tokens += *e.TokensOut
	}
	if tokens > 0 {
		return tokens
	}
	if e.RawByteLength != nil {
		return *e.RawByteLength / 4
	}
	return 0
}

// computeSpecQuality analyzes the first user message for spec quality indicators.
// Scoring uses continuous scales for natural distribution:
//   - Word count: linear 0–60 (capped at 150 words)
//   - Example indicators matched: graduated 0–20 (capped at 4 matches)
//   - Constraint indicators matched: graduated 0–20 (capped at 4 matches)
//
// Range: 0–100. Rounded to 1 decimal place.
func computeSpecQuality(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	// Find first user message.
	var firstUserPreview string
	for i := range entries {
		if entries[i].Role == ingest.RoleUser && entries[i].ContentPreview != nil {
			firstUserPreview = *entries[i].ContentPreview
			break
		}
	}

	if firstUserPreview == "" {
		return nil
	}

	words := strings.Fields(firstUserPreview)
	wordCount := len(words)

	lower := strings.ToLower(firstUserPreview)

	// Count matching example indicators (graduated scoring).
	exampleCount := 0
	for _, indicator := range []string{"example", "e.g.", "for instance", "such as", "like this", "```", "`"} {
		if strings.Contains(lower, indicator) {
			exampleCount++
		}
	}

	// Count matching constraint indicators (graduated scoring).
	constraintCount := 0
	for _, indicator := range []string{"must", "should", "require", "constraint", "limit", "don't", "do not", "never", "always"} {
		if strings.Contains(lower, indicator) {
			constraintCount++
		}
	}

	// Word count: continuous 0–60 scale (linear, capped at 150 words).
	wordScore := math.Min(float64(wordCount), 150.0) / 150.0 * 60.0

	// Examples: graduated 0–20 scale (capped at 4 matches).
	exampleScore := math.Min(float64(exampleCount), 4.0) / 4.0 * 20.0

	// Constraints: graduated 0–20 scale (capped at 4 matches).
	constraintScore := math.Min(float64(constraintCount), 4.0) / 4.0 * 20.0

	quality := math.Round((wordScore+exampleScore+constraintScore)*10) / 10
	specWordCount := wordCount
	specHasExamples := exampleCount > 0
	specHasConstraints := constraintCount > 0

	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{
		SpecQualityScore:     &quality,
		M7SpecWordCount:      &specWordCount,
		M7SpecHasExamples:    &specHasExamples,
		M7SpecHasConstraints: &specHasConstraints,
	}}
}

// --- v2 MetricFuncs ---

// computeSignalDensity calculates the percentage of meaningful depth=0 user+assistant entries.
// An entry is "meaningful" if it has tool_use, thinking, or content preview > 20 chars.
// Returns nil if no qualifying entries exist. Result is 0–100 (percentage).
func computeSignalDensity(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	var totalCount, meaningfulCount int

	for i := range entries {
		if entries[i].Depth != 0 || (entries[i].Role != ingest.RoleUser && entries[i].Role != ingest.RoleAssistant) {
			continue
		}
		totalCount++
		if entries[i].HasToolUse || entries[i].HasThinking {
			meaningfulCount++
			continue
		}
		if entries[i].ContentPreview != nil && len(*entries[i].ContentPreview) > 20 {
			meaningfulCount++
		}
	}

	if totalCount == 0 {
		return nil
	}

	density := float64(meaningfulCount) / float64(totalCount) * 100.0
	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{SignalDensity: &density}}
}

// computeReverts counts files that were edited multiple times (re-edits).
// Parses ToolInput JSON from depth=1 tool_use entries and tracks edit occasions
// per file path. A file with ≥2 edit occasions counts as a re-edit.
// Recognized tools: Write (has "content"), Edit (has "old_string"/"new_string"),
// NotebookEdit (has "notebook_path").
func computeReverts(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	editCounts := make(map[string]int)

	for i := range entries {
		if entries[i].Depth != 1 || entries[i].EntryType != ingest.EntryTypeToolUse || entries[i].ToolInput == nil {
			continue
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(*entries[i].ToolInput), &parsed); err != nil {
			continue
		}

		// Determine file path: file_path for Write/Edit, notebook_path for NotebookEdit.
		filePath, _ := parsed["file_path"].(string)
		if filePath == "" {
			filePath, _ = parsed["notebook_path"].(string)
		}
		if filePath == "" {
			continue
		}

		// Only count write/edit tools (must have content, old_string, or notebook_path).
		_, hasContent := parsed["content"].(string)
		_, hasOldString := parsed["old_string"].(string)
		_, hasNotebook := parsed["notebook_path"].(string)
		if !hasContent && !hasOldString && !hasNotebook {
			continue
		}

		editCounts[filePath]++
	}

	if len(editCounts) == 0 {
		return nil
	}

	reEditCount := 0
	for _, count := range editCounts {
		if count >= 2 {
			reEditCount++
		}
	}

	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{WithinSessionReverts: &reEditCount}}
}

// computeFileAnalysis counts distinct file paths and total lines changed from depth=1 tool_use entries.
func computeFileAnalysis(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	fileSet := make(map[string]struct{})
	var linesChanged int

	for i := range entries {
		if entries[i].Depth != 1 || entries[i].EntryType != ingest.EntryTypeToolUse || entries[i].ToolInput == nil {
			continue
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(*entries[i].ToolInput), &parsed); err != nil {
			slog.Debug("metrics: malformed ToolInput JSON", "entry_index", entries[i].EntryIndex)
			continue
		}

		filePath, _ := parsed["file_path"].(string)
		if filePath == "" {
			filePath, _ = parsed["notebook_path"].(string)
		}
		if filePath != "" {
			fileSet[filePath] = struct{}{}
		}

		// Count lines changed based on tool type.
		if content, ok := parsed["content"].(string); ok {
			// Write tool: count newlines in content.
			linesChanged += strings.Count(content, "\n") + 1
		} else if _, hasOld := parsed["old_string"]; hasOld {
			// Edit tool: delta = abs(newLines - oldLines).
			oldStr, _ := parsed["old_string"].(string)
			newStr, _ := parsed["new_string"].(string)
			oldLines := strings.Count(oldStr, "\n") + 1
			newLines := strings.Count(newStr, "\n") + 1
			delta := newLines - oldLines
			if delta < 0 {
				delta = -delta
			}
			linesChanged += delta
		}
	}

	if len(fileSet) == 0 {
		return nil
	}

	count := len(fileSet)
	result := &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{FilesTouched: &count}}
	if linesChanged > 0 {
		result.LinesChanged = &linesChanged
	}
	return result
}

// computeUniqueTools (M3) counts distinct tool names from depth=1 tool_use entries.
func computeUniqueTools(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	toolSet := make(map[string]struct{})

	for i := range entries {
		if entries[i].Depth != 1 || entries[i].EntryType != ingest.EntryTypeToolUse || entries[i].ToolNamesCSV == nil {
			continue
		}
		for _, name := range strings.Split(*entries[i].ToolNamesCSV, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				toolSet[name] = struct{}{}
			}
		}
	}

	if len(toolSet) == 0 {
		return nil
	}

	count := len(toolSet)
	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{M3UniqueToolCount: &count}}
}

// computeErrorRecovery (M4) tracks error→success transitions and max consecutive error streak.
func computeErrorRecovery(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
	var recoveryCount int
	var maxConsecutive int
	var currentStreak int
	var lastWasError bool
	var hasEntries bool

	for i := range entries {
		if entries[i].IsError {
			currentStreak++
			if currentStreak > maxConsecutive {
				maxConsecutive = currentStreak
			}
			lastWasError = true
			hasEntries = true
		} else {
			if lastWasError {
				recoveryCount++
			}
			currentStreak = 0
			lastWasError = false
			hasEntries = true
		}
	}

	if !hasEntries {
		return nil
	}

	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{
		M4ErrorRecoveryCount:  &recoveryCount,
		M4ConsecutiveErrorMax: &maxConsecutive,
	}}
}

// makeContextUtilizationFunc (M5) creates a MetricFunc that calculates context window utilization.
// If syncer is non-nil, queries real per-model context windows from the models table.
// Falls back to 200,000 tokens if model not found or syncer is nil.
func makeContextUtilizationFunc(syncer ingest.ModelsSyncer) MetricFunc {
	const defaultContextWindow = 200000

	return func(ctx context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
		var cumulativeTokens int
		var peakTokens int
		var totalTokens int
		var messageCount int

		for i := range entries {
			tokIn := 0
			tokOut := 0
			if entries[i].TokensIn != nil {
				tokIn = *entries[i].TokensIn
			}
			if entries[i].TokensOut != nil {
				tokOut = *entries[i].TokensOut
			}
			msgTokens := tokIn + tokOut
			if msgTokens > 0 {
				cumulativeTokens += msgTokens
				totalTokens += msgTokens
				messageCount++
				if cumulativeTokens > peakTokens {
					peakTokens = cumulativeTokens
				}
			}
		}

		if messageCount == 0 {
			return nil
		}

		// Determine context window: try real model lookup, fall back to default.
		contextWindow := defaultContextWindow
		if syncer != nil {
			modelID := extractModelID(entries)
			if modelID != "" {
				if cw, found, err := syncer.GetContextWindow(ctx, modelID); err == nil && found && cw > 0 {
					contextWindow = cw
				}
			}
		}

		pct := float64(peakTokens) / float64(contextWindow) * 100.0
		if pct > 100.0 {
			pct = 100.0
		}
		avgMsg := totalTokens / messageCount

		return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{
			M5ContextUtilizationPct: &pct,
			M5PeakContextTokens:     &peakTokens,
			M5AvgMessageTokens:      &avgMsg,
		}}
	}
}

// makeCostFunc creates a MetricFunc that computes cost-per-session from models.dev pricing.
// If syncer is nil, returns nil (no cost data available).
// Cost = price_per_mtok * tokens / 1,000,000 for each token type.
func makeCostFunc(syncer ingest.ModelsSyncer) MetricFunc {
	return func(ctx context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
		if syncer == nil {
			return nil
		}

		modelID := extractModelID(entries)
		if modelID == "" {
			return nil
		}

		// Try to find model with any provider key.
		model, err := syncer.GetModel(ctx, modelID, "")
		if err != nil || model == nil {
			// Try common provider keys.
			for _, pk := range []string{"anthropic", "openai", "google"} {
				model, err = syncer.GetModel(ctx, modelID, pk)
				if err == nil && model != nil {
					break
				}
			}
		}
		if model == nil {
			return nil
		}

		// Sum tokens by type across entries.
		var totalIn, totalOut int
		var totalReasoning, totalCacheRead, totalCacheWrite int
		for i := range entries {
			if entries[i].TokensIn != nil {
				totalIn += *entries[i].TokensIn
			}
			if entries[i].TokensOut != nil {
				totalOut += *entries[i].TokensOut
			}
			// Extract reasoning/cache tokens from Extra JSON.
			if entries[i].Extra != nil {
				var extra map[string]any
				if json.Unmarshal([]byte(*entries[i].Extra), &extra) == nil {
					if v, ok := extra["tokens_reasoning"]; ok {
						if fv, ok := v.(float64); ok {
							totalReasoning += int(fv)
						}
					}
					if v, ok := extra["cache_read"]; ok {
						if fv, ok := v.(float64); ok {
							totalCacheRead += int(fv)
						}
					}
					if v, ok := extra["cache_write"]; ok {
						if fv, ok := v.(float64); ok {
							totalCacheWrite += int(fv)
						}
					}
				}
			}
		}

		if totalIn == 0 && totalOut == 0 {
			return nil
		}

		result := &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{CostModelID: &modelID}}
		var totalCost float64

		if model.CostInputPerMTok != nil {
			cost := *model.CostInputPerMTok * float64(totalIn) / 1_000_000.0
			result.CostInputUSD = &cost
			totalCost += cost
		}
		if model.CostOutputPerMTok != nil {
			cost := *model.CostOutputPerMTok * float64(totalOut) / 1_000_000.0
			result.CostOutputUSD = &cost
			totalCost += cost
		}
		if model.CostReasoningPerMTok != nil && totalReasoning > 0 {
			cost := *model.CostReasoningPerMTok * float64(totalReasoning) / 1_000_000.0
			result.CostReasoningUSD = &cost
			totalCost += cost
		}
		if model.CostCacheReadPerMTok != nil && totalCacheRead > 0 {
			cost := *model.CostCacheReadPerMTok * float64(totalCacheRead) / 1_000_000.0
			result.CostCacheReadUSD = &cost
			totalCost += cost
		}
		if model.CostCacheWritePerMTok != nil && totalCacheWrite > 0 {
			cost := *model.CostCacheWritePerMTok * float64(totalCacheWrite) / 1_000_000.0
			result.CostCacheWriteUSD = &cost
			totalCost += cost
		}

		result.CostTotalUSD = &totalCost
		return result
	}
}

// extractModelID finds the most common model_id from entries' Extra JSON.
func extractModelID(entries []schema.SessionEntry) string {
	counts := make(map[string]int)
	for i := range entries {
		if entries[i].Extra == nil {
			continue
		}
		var extra map[string]any
		if json.Unmarshal([]byte(*entries[i].Extra), &extra) != nil {
			continue
		}
		if mid, ok := extra["model_id"].(string); ok && mid != "" {
			counts[mid]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	// Return the most common model_id.
	var bestID string
	var bestCount int
	for id, c := range counts {
		if c > bestCount {
			bestID = id
			bestCount = c
		}
	}
	return bestID
}

// computeTokenOutcomeRatio (M2) calculates total_tokens / outcome_weight.
// Weight: resolved=1.0, partial=0.5, failed=0.25.
// Returns nil if existing metrics have no outcome set (ratio computed on next recompute cycle).
func computeTokenOutcomeRatio(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, existing *ingest.SessionMetrics) *ingest.SessionMetrics {
	if existing == nil || existing.Outcome == nil {
		return nil
	}

	var weight float64
	switch *existing.Outcome {
	case ingest.OutcomeResolved:
		weight = 1.0
	case ingest.OutcomePartial:
		weight = 0.5
	case ingest.OutcomeFailed:
		weight = 0.25
	default:
		return nil
	}

	var totalTokens int
	for i := range entries {
		if entries[i].TokensIn != nil {
			totalTokens += *entries[i].TokensIn
		}
		if entries[i].TokensOut != nil {
			totalTokens += *entries[i].TokensOut
		}
	}

	if totalTokens == 0 {
		return nil
	}

	ratio := float64(totalTokens) / weight
	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{M2TokenOutcomeRatio: &ratio}}
}

// makeOutputSurvivalFunc (M6) creates a MetricFunc that computes output survival.
// If analyzer is nil, returns nil (M6 silently skipped).
// Compares Write/Edit tool outputs against git state to measure how much AI output survived.
func makeOutputSurvivalFunc(analyzer ingest.GitDiffAnalyzer) MetricFunc {
	return func(ctx context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
		if analyzer == nil {
			return nil
		}

		// Collect written file paths and content from depth=1 tool_use entries.
		type writeOp struct {
			filePath string
			content  string
		}
		var writes []writeOp
		for i := range entries {
			e := &entries[i]
			if e.Depth != 1 || e.EntryType != ingest.EntryTypeToolUse || e.ToolInput == nil {
				continue
			}
			var input map[string]any
			if json.Unmarshal([]byte(*e.ToolInput), &input) != nil {
				continue
			}
			fp, ok := input["file_path"].(string)
			if !ok || fp == "" {
				continue
			}
			content, _ := input["content"].(string)
			if content == "" {
				// Try new_string for Edit operations.
				content, _ = input["new_string"].(string)
			}
			if content == "" {
				continue
			}
			writes = append(writes, writeOp{filePath: fp, content: content})
		}

		if len(writes) == 0 {
			return nil
		}

		// Get commits in a reasonable time window (use entry timestamps).
		var startMS, endMS int64
		for i := range entries {
			if entries[i].TimestampMs != nil {
				ts := *entries[i].TimestampMs
				if startMS == 0 || ts < startMS {
					startMS = ts
				}
				if ts > endMS {
					endMS = ts
				}
			}
		}
		if startMS == 0 {
			return nil
		}

		since := time.Unix(0, startMS*int64(time.Millisecond))
		// Look up to 7 days after session start for committed changes.
		until := since.Add(7 * 24 * time.Hour)

		commits, err := analyzer.GetSessionCommits(ctx, ".", since, until)
		if err != nil || len(commits) == 0 {
			return nil
		}
		latestCommit := commits[len(commits)-1]

		var linesTotal, linesSurvived int
		for _, w := range writes {
			writtenLines := strings.Split(w.content, "\n")
			linesTotal += len(writtenLines)

			gitContent, err := analyzer.GetFileAtCommit(ctx, ".", w.filePath, latestCommit)
			if err != nil {
				// File deleted or not in git — 0 survived.
				continue
			}
			gitLines := make(map[string]bool)
			for _, line := range strings.Split(string(gitContent), "\n") {
				gitLines[line] = true
			}
			for _, line := range writtenLines {
				if gitLines[line] {
					linesSurvived++
				}
			}
		}

		if linesTotal == 0 {
			return nil
		}

		pct := float64(linesSurvived) / float64(linesTotal) * 100.0
		return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{
			M6OutputSurvivalPct: &pct,
			M6LinesSurvived:     &linesSurvived,
			M6LinesTotal:        &linesTotal,
		}}
	}
}

// ClassifyScope returns a scope classification based on the git remote URL.
// Heuristic: github.com/<user>/<repo> => "personal",
// github.com/<org>/<repo> with >1 team member => "team",
// enterprise domains or gitlab => "org",
// empty or local => "unknown".
// For v3, all github.com remotes default to "personal" since distinguishing
// user from org requires GitHub API calls.
func ClassifyScope(gitRemote string) string {
	if gitRemote == "" {
		return "unknown"
	}
	lower := strings.ToLower(gitRemote)
	if strings.Contains(lower, "localhost") || strings.HasPrefix(lower, "file://") {
		return "unknown"
	}
	if strings.Contains(lower, ".enterprise.") || strings.Contains(lower, "gitlab.") {
		return "org"
	}
	if strings.Contains(lower, "github.com") {
		return "personal"
	}
	return "unknown"
}

// computeScope classifies session scope from git remote stored in the entries' Extra JSON.
func computeScope(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, existing *ingest.SessionMetrics) *ingest.SessionMetrics {
	// Try to find git remote from existing session data or entries Extra.
	var gitRemote string

	// Look for git_remote in entries' Extra JSON populated by the OpenCode indexer.
	for i := range entries {
		if entries[i].Extra == nil {
			continue
		}
		var extra map[string]any
		if json.Unmarshal([]byte(*entries[i].Extra), &extra) != nil {
			continue
		}
		if r, ok := extra["git_remote"].(string); ok && r != "" {
			gitRemote = r
			break
		}
	}

	scope := ClassifyScope(gitRemote)
	return &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{Scope: &scope}}
}
