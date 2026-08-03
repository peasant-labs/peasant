package export_test

// eee_drift_test.go detects schema drift between the vendored eval.schema.json
// and the pinned every_eval_ever version this file was written against (v0.2.1).
//
// Background: eval.schema.json is the full EEE evaluation result document schema
// from evaleval/every_eval_ever. Peasant's EEEMetricConfig maps to the metric_config
// sub-object within evaluation_results items. The authoritative metric_config
// sub-schema uses different field names than EEEMetricConfig (EEE stores
// evaluation-level metadata like lower_is_better and score_type, while Peasant
// adds Peasant-specific fields like name, type, scale, categories, range). This is
// intentional: EEEMetricConfig is Peasant's per-annotation-type export shape; it
// does not claim to be a 1-to-1 mirror of EEE's metric_config.
//
// Drift detection strategy:
//  1. Read the "version" field from the vendored eval.schema.json.
//  2. Assert it matches the pinned version (eeeVendoredSchemaVersion).
//     If EEE releases a new version, this test fails — prompting a re-vendor
//     review cycle before the new schema is adopted.
//  3. Assert that the metric_config sub-schema still declares "lower_is_better"
//     as its sole required field — this is the one constraint our export must
//     satisfy, and any change to it is a breaking contract change.

import (
	"encoding/json"
	"testing"

	"github.com/peasant-labs/schema"
)

// eeeVendoredSchemaVersion is the "version" field value in the vendored
// eval.schema.json. Update this constant when re-vendoring a new release
// after the review cycle described in PROVENANCE.md.
const eeeVendoredSchemaVersion = "0.2.1"

// TestEEESchema_VendoredVersionMatches asserts that the "version" field inside
// the vendored eval.schema.json matches eeeVendoredSchemaVersion. If EEE
// releases a new version and the file is updated without bumping this constant,
// the test fails to prevent silent drift.
func TestEEESchema_VendoredVersionMatches(t *testing.T) {
	data := schema.EvalSchemaJSON

	var schemaObj struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &schemaObj); err != nil {
		t.Fatalf("eee_drift: parse eval.schema.json: %v", err)
	}

	if schemaObj.Version != eeeVendoredSchemaVersion {
		t.Errorf("vendored eval.schema.json version = %q, want %q\n"+
			"  If you re-vendored a new release, update eeeVendoredSchemaVersion in eee_drift_test.go\n"+
			"  and update PROVENANCE.md with the new source URL and fetch date.",
			schemaObj.Version, eeeVendoredSchemaVersion)
	}
}

// TestEEEMetricConfig_RequiredContractField asserts that the metric_config
// sub-schema in the vendored eval.schema.json still requires "lower_is_better".
//
// This is the sole required field that Peasant's export must always provide.
// If EEE removes or renames this constraint, the conformance test fixtures
// and EEEMetricConfig.LowerIsBetter must be revisited.
func TestEEEMetricConfig_RequiredContractField(t *testing.T) {
	data := schema.EvalSchemaJSON

	// Navigate to evaluation_results.items.properties.metric_config.required
	var doc struct {
		Properties struct {
			EvaluationResults struct {
				Items struct {
					Properties struct {
						MetricConfig struct {
							Required []string `json:"required"`
						} `json:"metric_config"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"evaluation_results"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("eee_drift: parse eval.schema.json for metric_config.required: %v", err)
	}

	required := doc.Properties.EvaluationResults.Items.Properties.MetricConfig.Required
	if len(required) == 0 {
		t.Fatal("eee_drift: metric_config.required is empty — expected [\"lower_is_better\"].\n" +
			"  The authoritative schema may have changed. Re-read PROVENANCE.md and re-vendor.")
	}

	found := false
	for _, field := range required {
		if field == "lower_is_better" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("eee_drift: metric_config.required = %v — expected to contain \"lower_is_better\".\n"+
			"  EEE changed its required fields. Review PROVENANCE.md and update EEEMetricConfig accordingly.",
			required)
	}
}
