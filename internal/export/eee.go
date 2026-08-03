// Package export provides export functionality for Peasant annotation data
// to external evaluation schemas. The primary target is the EveryEvalEver (EEE)
// schema (https://github.com/evaleval/every_eval_ever).
package export

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// EEE types — mirror the EveryEvalEver metric_config sub-schema
// ---------------------------------------------------------------------------

// EEEMetricType is the Peasant-internal type discriminator used to categorise
// annotation types before mapping to EEE ScoreType values.
type EEEMetricType string

const (
	EEEMetricCategorical EEEMetricType = "categorical"
	EEEMetricNumeric     EEEMetricType = "numeric"
	EEEMetricBoolean     EEEMetricType = "boolean"
)

// String implements fmt.Stringer.
func (t EEEMetricType) String() string { return string(t) }

// EEEScaleKind is the measurement scale in an EEE metric_config entry.
type EEEScaleKind string

const (
	EEEScaleNominal    EEEScaleKind = "nominal"
	EEEScaleOrdinal    EEEScaleKind = "ordinal"
	EEEScaleContinuous EEEScaleKind = "continuous"
)

// String implements fmt.Stringer.
func (k EEEScaleKind) String() string { return string(k) }

// EEEScoreType is the score_type discriminator in the authoritative EEE
// metric_config sub-schema. It must always be emitted to satisfy the
// if/then/else conditional in eval.schema.json v0.2.1.
//
// Mapping from Peasant types:
//   - EEEMetricBoolean / categorical-nominal → "binary"
//   - EEEMetricNumeric (continuous)          → "continuous"
//   - categorical-ordinal (levels)           → "levels"
type EEEScoreType string

const (
	EEEScoreTypeBinary     EEEScoreType = "binary"
	EEEScoreTypeContinuous EEEScoreType = "continuous"
	EEEScoreTypeLevels     EEEScoreType = "levels"
)

// String implements fmt.Stringer.
func (s EEEScoreType) String() string { return string(s) }

// EEERange defines a numeric min/max range for a metric.
type EEERange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// EEEMetricConfig is a single metric definition in the EEE schema.
// Each Peasant annotation type maps to one EEEMetricConfig.
//
// Field mapping to the authoritative eval.schema.json v0.2.1 metric_config:
//   - ScoreType      → score_type      (required to satisfy if/then/else)
//   - LowerIsBetter  → lower_is_better (required by the sub-schema)
//   - LevelNames     → level_names     (required when score_type == "levels")
//   - HasUnknownLevel→ has_unknown_level(required when score_type == "levels")
//   - MinScore/Max   → min_score/max_score (required when score_type == "continuous")
//
// Peasant-specific fields (name, description, type, scale, categories, range) are
// emitted as additional_details-compatible extras. The metric_config sub-schema
// in eval.schema.json v0.2.1 does not set additionalProperties:false, so these
// extra fields pass validation.
type EEEMetricConfig struct {
	// Peasant-specific fields (not in authoritative metric_config but allowed via open schema)
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Type        EEEMetricType `json:"type"`
	Scale       EEEScaleKind  `json:"scale,omitempty"`
	Categories  []string      `json:"categories,omitempty"`
	Range       *EEERange     `json:"range,omitempty"`

	// Authoritative EEE metric_config fields (eval.schema.json v0.2.1)
	ScoreType       EEEScoreType `json:"score_type"`
	LowerIsBetter   *bool        `json:"lower_is_better,omitempty"`
	LevelNames      []string     `json:"level_names,omitempty"`
	HasUnknownLevel *bool        `json:"has_unknown_level,omitempty"`
	MinScore        *float64     `json:"min_score,omitempty"`
	MaxScore        *float64     `json:"max_score,omitempty"`
}

// ---------------------------------------------------------------------------
// EEEMapper — implements MetricMapper[EEEMetricConfig]
// ---------------------------------------------------------------------------

// EEEMapper maps Peasant AnnotationTypeRows to EEE MetricConfigs.
type EEEMapper struct{}

// Compile-time interface guard.
var _ MetricMapper[EEEMetricConfig] = (*EEEMapper)(nil)

// MapType maps a single AnnotationTypeRow to an EEEMetricConfig.
func (m *EEEMapper) MapType(at store.AnnotationTypeRow) (EEEMetricConfig, error) {
	return AnnotationTypeToEEEMetric(at)
}

// FormatName returns the official EveryEvalEver repository name.
func (m *EEEMapper) FormatName() string { return "every_eval_ever" }

// ExportEEE is the convenience wrapper for CLI use.
// It calls Export[EEEMetricConfig] with an EEEMapper and returns the generic result.
func ExportEEE(types []store.AnnotationTypeRow) (*ExportResult[EEEMetricConfig], error) {
	return Export[EEEMetricConfig](&EEEMapper{}, types)
}

// ---------------------------------------------------------------------------
// Mapping: store.AnnotationTypeRow -> EEE MetricConfig
// ---------------------------------------------------------------------------

// AnnotationTypeToEEEMetric maps a single Peasant AnnotationTypeRow to an EEE MetricConfig.
//
// Mapping rules:
//   - any datatype=boolean              -> type:boolean, score_type:binary
//   - enumerated + ordinal scale        -> type:categorical, score_type:levels
//   - enumerated + nominal scale        -> type:categorical, score_type:binary
//   - described  + real/integer         -> type:numeric,     score_type:continuous
//   - described  + text                 -> type:categorical, score_type:binary
//
// The score_type field is always set to satisfy the if/then/else conditional in
// the authoritative eval.schema.json v0.2.1 metric_config sub-schema.
//
// When score_type is "levels", level_names (from categories) and has_unknown_level
// are set as required by the authoritative schema.
// When score_type is "continuous" and a min/max range is present, min_score and
// max_score are set as required by the authoritative schema.
func AnnotationTypeToEEEMetric(at store.AnnotationTypeRow) (EEEMetricConfig, error) {
	hasUnknownFalse := false
	metric := EEEMetricConfig{
		Name:          at.TypeID,
		Description:   at.Description,
		LowerIsBetter: at.LowerIsBetter,
	}

	switch {
	// Boolean always maps to boolean regardless of domain kind.
	case at.Datatype == schema.DatatypeBoolean:
		metric.Type = EEEMetricBoolean
		metric.ScoreType = EEEScoreTypeBinary

	// Enumerated -> categorical.
	case at.ValueDomainKind == schema.DomainEnumerated:
		metric.Type = EEEMetricCategorical
		categories := parsePermissibleValues(at.ValueConstraint)
		metric.Categories = categories
		if at.ScaleKind == schema.ScaleOrdinal || at.LowerIsBetter != nil {
			metric.Scale = EEEScaleOrdinal
			metric.ScoreType = EEEScoreTypeLevels
			// score_type:levels requires level_names and has_unknown_level.
			metric.LevelNames = categories
			metric.HasUnknownLevel = &hasUnknownFalse
		} else {
			metric.Scale = EEEScaleNominal
			metric.ScoreType = EEEScoreTypeBinary
		}

	// Described + real/integer -> numeric.
	case at.ValueDomainKind == schema.DomainDescribed && (at.Datatype == schema.DatatypeReal || at.Datatype == schema.DatatypeInteger):
		metric.Type = EEEMetricNumeric
		metric.Scale = EEEScaleContinuous
		metric.ScoreType = EEEScoreTypeContinuous
		if at.ValueConstraint != "" {
			r, err := parseConstraintRange(at.ValueConstraint)
			if err == nil && r != nil {
				metric.Range = r
				// score_type:continuous requires min_score and max_score.
				metric.MinScore = &r.Min
				metric.MaxScore = &r.Max
			}
		}

	// Described + text -> categorical nominal.
	case at.ValueDomainKind == schema.DomainDescribed && at.Datatype == schema.DatatypeText:
		metric.Type = EEEMetricCategorical
		metric.Scale = EEEScaleNominal
		metric.ScoreType = EEEScoreTypeBinary

	default:
		return EEEMetricConfig{}, fmt.Errorf("export: unsupported mapping: domain=%s datatype=%s for %q",
			at.ValueDomainKind, at.Datatype, at.TypeID)
	}

	return metric, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// constraintRange is the JSON structure expected in ValueConstraint for described numeric domains.
type constraintRange struct {
	Min *float64 `json:"min"`
	Max *float64 `json:"max"`
}

// parseConstraintRange attempts to parse the ValueConstraint JSON as a min/max range.
// Returns nil, nil if the spec does not contain min/max fields.
func parseConstraintRange(spec string) (*EEERange, error) {
	var cr constraintRange
	if err := json.Unmarshal([]byte(spec), &cr); err != nil {
		return nil, fmt.Errorf("parse constraint range: %w", err)
	}
	if cr.Min == nil || cr.Max == nil {
		return nil, nil
	}
	return &EEERange{Min: *cr.Min, Max: *cr.Max}, nil
}

// parsePermissibleValues parses the JSON-encoded permissible values array from ValueConstraint.
func parsePermissibleValues(constraint string) []string {
	if constraint == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(constraint), &values); err != nil {
		return nil
	}
	return values
}
