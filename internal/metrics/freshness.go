package metrics

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

var _ ingest.SessionMetricsEnsurer = (*Engine)(nil)

// EnsureSessionMetrics reports completion for this session, including reuse of
// proven current output. Aggregate computation counts are not completion proof.
func (e *Engine) EnsureSessionMetrics(ctx context.Context, sid ingest.SessionID) (computed, current bool, err error) {
	backing, ok := e.store.(ingest.MetricInputStore)
	if !ok {
		return false, false, fmt.Errorf("check metrics for session %s: store cannot capture inputs; classification was deferred; use the persistent metrics store", sid)
	}
	// Inspect the cheap producer row before loading entries or external context.
	existing, err := e.store.GetMetrics(ctx, sid)
	if err != nil {
		return false, false, err
	}
	if existing != nil && existing.ComputeVersion != nil && *existing.ComputeVersion > CurrentComputeVersion {
		return false, false, fmt.Errorf("check metrics for session %s: stored compute version %d exceeds this build's %d; upgrade Peasant before retrying", sid, *existing.ComputeVersion, CurrentComputeVersion)
	}
	computed, err = e.computeCapturedSession(ctx, backing, sid)
	return computed, err == nil, err
}

func metricInputHash(database string, models capturedModels, git capturedGitMetric) (string, error) {
	identity, err := json.Marshal(struct {
		Database string
		Version  int
		Models   capturedModels
		Git      capturedGitMetric
	}{database, CurrentComputeVersion, models, git})
	if err != nil {
		return "", err
	}
	return schema.ComputeTranscriptHash(identity), nil
}

func metricsMatchInput(existing *ingest.SessionMetrics, inputHash string) bool {
	if existing == nil || existing.ComputeVersion == nil || *existing.ComputeVersion != CurrentComputeVersion || existing.InputHash == nil || *existing.InputHash != inputHash || existing.OutputHash == nil {
		return false
	}
	output, err := ingest.MetricOutputHash(existing)
	return err == nil && output == *existing.OutputHash
}

// MetricsCurrentForInput verifies a coherent classifier capture against metrics
// produced by the default or stored-model engine. Other enabled external inputs
// cannot be established from this database snapshot and fail closed.
func MetricsCurrentForInput(input *ingest.MetricInput) bool {
	if input == nil {
		return false
	}
	owned := *input
	if input.StoredModels {
		models := capturedModels{Enabled: true, Window: 200000, Model: input.Model}
		if input.Model != nil && input.Model.ContextWindow != nil && *input.Model.ContextWindow > 0 {
			models.Window = *input.Model.ContextWindow
		}
		database, err := owned.Hash()
		if err != nil {
			return false
		}
		identity, err := metricInputHash(database, models, capturedGitMetric{})
		if err == nil && metricsMatchInput(input.Existing, identity) {
			return true
		}
	}
	owned.StoredModels, owned.Model = false, nil
	database, err := owned.Hash()
	if err != nil {
		return false
	}
	identity, err := metricInputHash(database, capturedModels{Window: 200000}, capturedGitMetric{})
	return err == nil && metricsMatchInput(input.Existing, identity)
}
