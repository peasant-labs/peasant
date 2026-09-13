package ingest

import (
	"context"
	"encoding/json"

	"github.com/peasant-labs/schema"
)

// MetricInput is one owned database view used by a metrics computation.
// Existing output and parser bookkeeping are excluded from DatabaseHash.
type MetricInput struct {
	SessionID    SessionID
	Entries      []schema.SessionEntry
	Seed         *StatsInfo
	Harness      Harness
	ProjectPath  string
	StartMS      int64
	EndMS        int64
	StoredModels bool
	Model        *MetricModel
	IndexState   *SessionIndexState `json:"-"`
	Existing     *SessionMetrics    `json:"-"`
	DatabaseHash string             `json:"-"`
}

// MetricModel contains only consumed model identity, context and prices.
// Missing rows/prices remain unknown; model synchronization time is not input.
type MetricModel struct {
	ModelID               string
	ProviderKey           string
	ContextWindow         *int
	CostInputPerMTok      *float64
	CostOutputPerMTok     *float64
	CostReasoningPerMTok  *float64
	CostCacheReadPerMTok  *float64
	CostCacheWritePerMTok *float64
}

type MetricInputStore interface {
	ReadMetricInput(context.Context, SessionID, bool) (*MetricInput, error)
	SaveMetricsForInput(context.Context, *MetricInput, *SessionMetrics) error
}

func (input *MetricInput) Hash() (string, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return schema.ComputeTranscriptHash(data), nil
}

// MetricOutputHash excludes the completion clock but includes the algorithm.
func MetricOutputHash(metrics *SessionMetrics) (string, error) {
	output := metrics.QualityMetrics
	output.ComputedAt = nil
	// These established NOT NULL SQLite columns store absent flags as false.
	// Hash that stored meaning so empty/minimal computations survive a reread.
	absent := false
	if output.M7SpecHasExamples == nil {
		output.M7SpecHasExamples = &absent
	}
	if output.M7SpecHasConstraints == nil {
		output.M7SpecHasConstraints = &absent
	}
	data, err := json.Marshal(output)
	if err != nil {
		return "", err
	}
	return schema.ComputeTranscriptHash(data), nil
}

// MetricModelID chooses the dominant recorded model with a stable tie break.
func MetricModelID(entries []schema.SessionEntry) string {
	counts := make(map[string]int)
	for _, entry := range entries {
		if entry.Extra == nil {
			continue
		}
		var extra struct {
			ModelID string `json:"model_id"`
		}
		if json.Unmarshal([]byte(*entry.Extra), &extra) == nil && extra.ModelID != "" {
			counts[extra.ModelID]++
		}
	}
	var best string
	for id, count := range counts {
		if count > counts[best] || count == counts[best] && id < best {
			best = id
		}
	}
	return best
}
