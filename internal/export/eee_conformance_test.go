package export_test

// eee_conformance_test.go validates that EEE export output conforms to
// the metric_config sub-schema within the vendored eval.schema.json, read from
// the schema module's embedded schema.EvalSchemaJSON accessor.
//
// The authoritative eval.schema.json is the full EEE evaluation result document
// schema from evaleval/every_eval_ever v0.2.1. Peasant's EEEMetricConfig maps to
// the metric_config sub-object within evaluation_results items. The conformance
// test validates each EEEMetricConfig produced by ExportEEE against that
// sub-schema using a JSON pointer path:
//
//	eval.schema.json#/properties/evaluation_results/items/properties/metric_config
//
// The metric_config sub-schema has one required field: lower_is_better. All
// other fields are optional and no additionalProperties constraint applies, so
// Peasant's extra fields (name, type, scale, categories, range) pass through.

import (
	"bytes"
	"encoding/json"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// eeeMetricConfigSubSchemaPtr is the JSON pointer to the metric_config
// sub-schema within the authoritative eval.schema.json document.
const eeeMetricConfigSubSchemaPtr = "eval.schema.json#/properties/evaluation_results/items/properties/metric_config"

// loadCompiledEEEMetricConfigSchema compiles the metric_config sub-schema from
// the schema module's embedded eval.schema.json (schema.EvalSchemaJSON).
//
// The metric_config sub-schema is located at:
//
//	properties.evaluation_results.items.properties.metric_config
//
// santhosh-tekuri/jsonschema supports fragment-based sub-schema compilation via
// JSON pointer syntax in the URL.
func loadCompiledEEEMetricConfigSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("eval.schema.json", bytes.NewReader(schema.EvalSchemaJSON)); err != nil {
		t.Fatalf("eee_conformance: add schema resource from schema.EvalSchemaJSON: %v", err)
	}

	sch, err := compiler.Compile(eeeMetricConfigSubSchemaPtr)
	if err != nil {
		t.Fatalf("eee_conformance: compile metric_config sub-schema at %s: %v",
			eeeMetricConfigSubSchemaPtr, err)
	}

	return sch
}

// validateConfig marshals cfg to JSON, decodes it back to interface{}, and
// validates it against the compiled metric_config JSON sub-schema. This
// round-trip ensures we validate the exact JSON shape the consumer sees, not
// the Go struct.
//
// The authoritative metric_config sub-schema requires lower_is_better. Test
// fixtures must always set it to reflect the real contract.
func validateConfig(t *testing.T, sch *jsonschema.Schema, i int, cfg export.EEEMetricConfig) {
	t.Helper()

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("config[%d] (%s) marshal: %v", i, cfg.Name, err)
	}

	var decoded interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("config[%d] (%s) unmarshal to interface{}: %v", i, cfg.Name, err)
	}

	if err := sch.Validate(decoded); err != nil {
		t.Errorf("config[%d] (%s) metric_config sub-schema validation failed:\n  json: %s\n  err:  %v", i, cfg.Name, data, err)
	}
}

// TestEEE_ConformsToSchema validates that ExportEEE output for a representative
// set of annotation types conforms to the metric_config sub-schema within
// eval.schema.json.
//
// Coverage: one fixture per EEEMetricType (categorical/ordinal, categorical/nominal,
// numeric/continuous with range, numeric/continuous without range, boolean).
//
// Every fixture includes lower_is_better because the authoritative metric_config
// sub-schema requires it.
func TestEEE_ConformsToSchema(t *testing.T) {
	sch := loadCompiledEEEMetricConfigSchema(t)

	lb := true
	lbFalse := false
	types := []store.AnnotationTypeRow{
		{
			// categorical / ordinal (enumerated + ordinal scale)
			TypeID:          "quality.session_outcome",
			Description:     "Session outcome quality",
			ValueDomainKind: schema.DomainEnumerated,
			Datatype:        schema.DatatypeText,
			ValueConstraint: `["resolved","partial","failed","abandoned"]`,
			LowerIsBetter:   &lb,
		},
		{
			// categorical / nominal (enumerated, no ordering)
			TypeID:          "quality.failure_mode",
			Description:     "Primary failure mode",
			ValueDomainKind: schema.DomainEnumerated,
			Datatype:        schema.DatatypeText,
			ValueConstraint: `["timeout","error","refusal","other"]`,
			LowerIsBetter:   &lbFalse,
		},
		{
			// numeric / continuous with range
			TypeID:          "quality.signal_density",
			Description:     "Signal density (0.0–1.0)",
			ValueDomainKind: schema.DomainDescribed,
			Datatype:        schema.DatatypeReal,
			ValueConstraint: `{"min":0,"max":1}`,
			LowerIsBetter:   &lbFalse,
		},
		{
			// numeric / continuous with an explicit range
			// (score_type:continuous requires min_score and max_score per the
			// authoritative eval.schema.json v0.2.1 metric_config sub-schema)
			TypeID:          "metrics.retry_loops",
			Description:     "Number of retry loops",
			ValueDomainKind: schema.DomainDescribed,
			Datatype:        schema.DatatypeInteger,
			ValueConstraint: `{"min":0,"max":100}`,
			LowerIsBetter:   &lb,
		},
		{
			// boolean
			TypeID:          "quality.has_errors",
			Description:     "Whether the session had tool errors",
			ValueDomainKind: schema.DomainDescribed,
			Datatype:        schema.DatatypeBoolean,
			LowerIsBetter:   &lbFalse,
		},
	}

	result, err := export.ExportEEE(types)
	if err != nil {
		t.Fatalf("ExportEEE: %v", err)
	}

	if result.TotalTypes != len(types) {
		t.Errorf("TotalTypes = %d, want %d", result.TotalTypes, len(types))
	}

	for i, cfg := range result.Configs {
		validateConfig(t, sch, i, cfg)
	}
}
