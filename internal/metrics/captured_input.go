package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

func (e *Engine) computeCapturedMetrics(ctx context.Context, backing ingest.MetricInputStore, sessionIDs []ingest.SessionID) (int, error) {
	computed := 0
	var failures error
	for _, sid := range sessionIDs {
		if err := ctx.Err(); err != nil {
			return computed, errors.Join(failures, err)
		}
		changed, err := e.computeCapturedSession(ctx, backing, sid)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("compute metrics for session %s: %w; previous values and completion proof were preserved; retry after restoring the reported input", sid, err))
		} else if changed {
			computed++
		}
	}
	return computed, failures
}

func (e *Engine) computeCapturedSession(ctx context.Context, backing ingest.MetricInputStore, sid ingest.SessionID) (bool, error) {
	// Production passes the same Store for metrics and model lookups. Capture
	// those model rows in its transaction; other injected readers are external.
	storedModels := e.models != nil && reflect.TypeOf(e.models).Comparable() && any(e.models) == any(e.store)
	input, err := backing.ReadMetricInput(ctx, sid, storedModels)
	if err != nil {
		return false, err
	}
	if input == nil {
		return false, fmt.Errorf("stored session input is unavailable")
	}
	existing := input.Existing
	if existing != nil && existing.ComputeVersion != nil && *existing.ComputeVersion > CurrentComputeVersion {
		return false, fmt.Errorf("stored compute version %d exceeds this build's %d; upgrade Peasant", *existing.ComputeVersion, CurrentComputeVersion)
	}
	models, err := e.captureModels(ctx, input)
	if err != nil {
		return false, err
	}
	git, err := captureGitMetric(ctx, e.git, input)
	if err != nil {
		return false, err
	}
	inputHash, err := metricInputHash(input.DatabaseHash, models, git)
	if err != nil {
		return false, err
	}
	if !e.force && metricsMatchInput(existing, inputHash) {
		return false, nil
	}
	merged := &ingest.SessionMetrics{SessionID: sid}
	if input.Seed != nil {
		seed := *input.Seed
		merged.TurnCount, merged.SubagentCount = &seed.TurnCount, &seed.SubagentCount
		merged.InputTokens, merged.OutputTokens = &seed.TokensIn, &seed.TokensOut
		merged.ToolCalls = &seed.ToolCallCount
		duration := float64(seed.DurationMs) / 60000
		merged.DurationMinutes = &duration
	} else if input.EndMS >= input.StartMS {
		duration := float64(input.EndMS-input.StartMS) / 60000
		merged.DurationMinutes = &duration
	}
	titleFunc := func(_ context.Context, _ ingest.SessionID, entries []schema.SessionEntry, _ *ingest.SessionMetrics) *ingest.SessionMetrics {
		return e.computeTitleWithContext(sid, entries, input.Harness, input.ProjectPath)
	}
	var modelLookup ingest.ModelsSyncer
	if models.Enabled {
		modelLookup = &models
	}
	for _, metric := range defaultMetricFuncs(modelLookup, nil, titleFunc) {
		if result := metric.fn(ctx, sid, input.Entries, merged); result != nil {
			mergeSessionMetrics(merged, result)
		}
	}
	if git.Result != nil {
		mergeSessionMetrics(merged, git.Result)
	}
	now, version := time.Now().UnixMilli(), CurrentComputeVersion
	merged.ComputedAt, merged.ComputeVersion, merged.InputHash = &now, &version, &inputHash
	outputHash, err := ingest.MetricOutputHash(merged)
	if err != nil {
		return false, err
	}
	merged.OutputHash = &outputHash
	if err := backing.SaveMetricsForInput(ctx, input, merged); err != nil {
		return false, err
	}
	return true, nil
}

// capturedModels adapts owned values to the existing pure metric closures.
// No closure can re-read the mutable model source during computation.
type capturedModels struct {
	Enabled bool
	Window  int
	Model   *ingest.MetricModel
}

var _ ingest.ModelsSyncer = (*capturedModels)(nil)

func (e *Engine) captureModels(ctx context.Context, input *ingest.MetricInput) (capturedModels, error) {
	result := capturedModels{Enabled: e.models != nil, Window: 200000, Model: input.Model}
	if !result.Enabled {
		return result, nil
	}
	if input.StoredModels {
		if result.Model != nil && result.Model.ContextWindow != nil && *result.Model.ContextWindow > 0 {
			result.Window = *result.Model.ContextWindow
		}
		return result, nil
	}
	modelID := ingest.MetricModelID(input.Entries)
	if modelID == "" {
		return result, nil
	}
	window, found, err := e.models.GetContextWindow(ctx, modelID)
	if err != nil {
		return result, fmt.Errorf("capture model context for %s: %w", modelID, err)
	}
	if found && window > 0 {
		result.Window = window
	}
	for _, provider := range []string{"", "anthropic", "openai", "google"} {
		model, err := e.models.GetModel(ctx, modelID, provider)
		if err != nil {
			return result, fmt.Errorf("capture model prices for %s/%s: %w", provider, modelID, err)
		}
		if model == nil {
			continue
		}
		// Copy only the consumed fields; LastSynced and display metadata are
		// deliberately absent from the owned representation and its digest.
		data, err := json.Marshal(model)
		if err != nil {
			return result, err
		}
		result.Model = &ingest.MetricModel{}
		if err := json.Unmarshal(data, result.Model); err != nil {
			return result, err
		}
		break
	}
	return result, nil
}

func (models *capturedModels) GetContextWindow(context.Context, string) (int, bool, error) {
	return models.Window, true, nil
}

func (models *capturedModels) GetModel(context.Context, string, string) (*ingest.ModelInfo, error) {
	if models.Model == nil {
		return nil, nil
	}
	m := models.Model
	return &ingest.ModelInfo{ModelID: m.ModelID, ProviderKey: m.ProviderKey,
		ContextWindow: m.ContextWindow, CostInputPerMTok: m.CostInputPerMTok,
		CostOutputPerMTok: m.CostOutputPerMTok, CostReasoningPerMTok: m.CostReasoningPerMTok,
		CostCacheReadPerMTok: m.CostCacheReadPerMTok, CostCacheWritePerMTok: m.CostCacheWritePerMTok}, nil
}

func (*capturedModels) SyncModels(context.Context, []ingest.ModelInfo) error {
	return fmt.Errorf("captured metric model values are read-only")
}
