package export_test

import (
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

func boolPtr(v bool) *bool { return &v }

// ---------------------------------------------------------------------------
// Per-mapping unit tests (exercise AnnotationTypeToEEEMetric directly)
// ---------------------------------------------------------------------------

// TestCategoricalNominal tests: enumerated+text, LowerIsBetter=nil -> categorical/nominal.
func TestCategoricalNominal(t *testing.T) {
	at := store.AnnotationTypeRow{
		TypeID:          "quality.session_outcome",
		Description:     "Session outcome",
		ValueDomainKind: schema.DomainEnumerated,
		Datatype:        schema.DatatypeText,
		ValueConstraint: `["resolved","partial","failed","abandoned"]`,
	}
	mc, err := export.AnnotationTypeToEEEMetric(at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.Type != export.EEEMetricCategorical {
		t.Errorf("Type = %q, want %q", mc.Type, export.EEEMetricCategorical)
	}
	if mc.Scale != export.EEEScaleNominal {
		t.Errorf("Scale = %q, want %q", mc.Scale, export.EEEScaleNominal)
	}
	if len(mc.Categories) != 4 {
		t.Errorf("Categories length = %d, want 4", len(mc.Categories))
	}
}

// TestCategoricalOrdinal tests: enumerated+text, LowerIsBetter=true -> categorical/ordinal.
func TestCategoricalOrdinal(t *testing.T) {
	at := store.AnnotationTypeRow{
		TypeID:          "quality.session_outcome",
		ValueDomainKind: schema.DomainEnumerated,
		Datatype:        schema.DatatypeText,
		ValueConstraint: `["resolved","partial","failed"]`,
		LowerIsBetter:   boolPtr(true),
	}
	mc, err := export.AnnotationTypeToEEEMetric(at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.Scale != export.EEEScaleOrdinal {
		t.Errorf("Scale = %q, want %q", mc.Scale, export.EEEScaleOrdinal)
	}
	if mc.LowerIsBetter == nil || !*mc.LowerIsBetter {
		t.Error("LowerIsBetter should be true")
	}
}

// TestCategoricalOrdinal_ViaScaleKind tests: enumerated + ScaleKind=ordinal -> ordinal.
func TestCategoricalOrdinal_ViaScaleKind(t *testing.T) {
	at := store.AnnotationTypeRow{
		TypeID:          "quality.session_outcome",
		ValueDomainKind: schema.DomainEnumerated,
		Datatype:        schema.DatatypeText,
		ValueConstraint: `["resolved","partial","failed"]`,
		ScaleKind:       schema.ScaleOrdinal,
	}
	mc, err := export.AnnotationTypeToEEEMetric(at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.Scale != export.EEEScaleOrdinal {
		t.Errorf("Scale = %q, want %q", mc.Scale, export.EEEScaleOrdinal)
	}
}

// TestNumericRealWithRange tests: described+real with range -> numeric/continuous.
func TestNumericRealWithRange(t *testing.T) {
	at := store.AnnotationTypeRow{
		TypeID:          "quality.signal_density",
		ValueDomainKind: schema.DomainDescribed,
		Datatype:        schema.DatatypeReal,
		ValueConstraint: `{"min":0,"max":1}`,
	}
	mc, err := export.AnnotationTypeToEEEMetric(at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.Type != export.EEEMetricNumeric {
		t.Errorf("Type = %q, want %q", mc.Type, export.EEEMetricNumeric)
	}
	if mc.Scale != export.EEEScaleContinuous {
		t.Errorf("Scale = %q, want %q", mc.Scale, export.EEEScaleContinuous)
	}
	if mc.Range == nil {
		t.Fatal("Range should not be nil")
	}
	if mc.Range.Min != 0 || mc.Range.Max != 1 {
		t.Errorf("Range = [%.1f, %.1f], want [0.0, 1.0]", mc.Range.Min, mc.Range.Max)
	}
}

// TestNumericIntegerNoRange tests: described+integer without range -> numeric, no range.
func TestNumericIntegerNoRange(t *testing.T) {
	at := store.AnnotationTypeRow{
		TypeID:          "metrics.retry_loops",
		ValueDomainKind: schema.DomainDescribed,
		Datatype:        schema.DatatypeInteger,
		ValueConstraint: `{}`,
	}
	mc, err := export.AnnotationTypeToEEEMetric(at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.Type != export.EEEMetricNumeric {
		t.Errorf("Type = %q, want %q", mc.Type, export.EEEMetricNumeric)
	}
	if mc.Range != nil {
		t.Errorf("Range should be nil for integer without range, got %+v", mc.Range)
	}
}

// TestBooleanDescribed tests: described+boolean -> boolean.
func TestBooleanDescribed(t *testing.T) {
	at := store.AnnotationTypeRow{
		TypeID:          "quality.has_errors",
		ValueDomainKind: schema.DomainDescribed,
		Datatype:        schema.DatatypeBoolean,
		ValueConstraint: `{}`,
	}
	mc, err := export.AnnotationTypeToEEEMetric(at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.Type != export.EEEMetricBoolean {
		t.Errorf("Type = %q, want %q", mc.Type, export.EEEMetricBoolean)
	}
}

// TestBooleanEnumerated tests edge case: enumerated+boolean -> boolean (datatype wins).
func TestBooleanEnumerated(t *testing.T) {
	at := store.AnnotationTypeRow{
		TypeID:          "quality.has_errors",
		ValueDomainKind: schema.DomainEnumerated,
		Datatype:        schema.DatatypeBoolean,
		ValueConstraint: `["true","false"]`,
	}
	mc, err := export.AnnotationTypeToEEEMetric(at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.Type != export.EEEMetricBoolean {
		t.Errorf("Type = %q, want %q (boolean datatype should win)", mc.Type, export.EEEMetricBoolean)
	}
}

// TestLowerIsBetterPassthrough tests nil/true/false passthrough.
func TestLowerIsBetterPassthrough(t *testing.T) {
	base := store.AnnotationTypeRow{
		TypeID:          "quality.test",
		ValueDomainKind: schema.DomainEnumerated,
		Datatype:        schema.DatatypeText,
		ValueConstraint: `["a","b"]`,
	}

	// nil
	mc, err := export.AnnotationTypeToEEEMetric(base)
	if err != nil {
		t.Fatalf("nil: %v", err)
	}
	if mc.LowerIsBetter != nil {
		t.Error("nil case: LowerIsBetter should be nil")
	}

	// true
	base.LowerIsBetter = boolPtr(true)
	mc, err = export.AnnotationTypeToEEEMetric(base)
	if err != nil {
		t.Fatalf("true: %v", err)
	}
	if mc.LowerIsBetter == nil || !*mc.LowerIsBetter {
		t.Error("true case: LowerIsBetter should be true")
	}

	// false
	base.LowerIsBetter = boolPtr(false)
	mc, err = export.AnnotationTypeToEEEMetric(base)
	if err != nil {
		t.Fatalf("false: %v", err)
	}
	if mc.LowerIsBetter == nil || *mc.LowerIsBetter {
		t.Error("false case: LowerIsBetter should be false")
	}
}

// TestDescribedText_CategoricalNominal tests: described+text -> categorical/nominal.
func TestDescribedText_CategoricalNominal(t *testing.T) {
	at := store.AnnotationTypeRow{
		TypeID:          "metadata.freetext",
		ValueDomainKind: schema.DomainDescribed,
		Datatype:        schema.DatatypeText,
		ValueConstraint: `{}`,
	}
	mc, err := export.AnnotationTypeToEEEMetric(at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mc.Type != export.EEEMetricCategorical {
		t.Errorf("Type = %q, want %q", mc.Type, export.EEEMetricCategorical)
	}
	if mc.Scale != export.EEEScaleNominal {
		t.Errorf("Scale = %q, want %q", mc.Scale, export.EEEScaleNominal)
	}
}

// ---------------------------------------------------------------------------
// ExportEEE integration tests
// ---------------------------------------------------------------------------

// TestExportEEE_RoundTrip_JSON tests JSON marshal/unmarshal round-trip via ExportEEE.
func TestExportEEE_RoundTrip_JSON(t *testing.T) {
	types := []store.AnnotationTypeRow{
		{
			TypeID:          "quality.session_outcome",
			Description:     "Outcome",
			ValueDomainKind: schema.DomainEnumerated,
			Datatype:        schema.DatatypeText,
			ValueConstraint: `["resolved","failed"]`,
		},
		{
			TypeID:          "quality.signal_density",
			Description:     "Signal density",
			ValueDomainKind: schema.DomainDescribed,
			Datatype:        schema.DatatypeReal,
			ValueConstraint: `{"min":0,"max":1}`,
		},
	}

	result, err := export.ExportEEE(types)
	if err != nil {
		t.Fatalf("ExportEEE: %v", err)
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got export.ExportResult[export.EEEMetricConfig]
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.TotalTypes != 2 {
		t.Errorf("TotalTypes = %d, want 2", got.TotalTypes)
	}
	if len(got.Configs) != 2 {
		t.Errorf("len(Configs) = %d, want 2", len(got.Configs))
	}
	if got.Configs[0].Type != export.EEEMetricCategorical {
		t.Errorf("Configs[0].Type = %q, want %q", got.Configs[0].Type, export.EEEMetricCategorical)
	}
	if got.Configs[1].Type != export.EEEMetricNumeric {
		t.Errorf("Configs[1].Type = %q, want %q", got.Configs[1].Type, export.EEEMetricNumeric)
	}
}

// TestExportEEE_EmptyTypes tests that nil/empty input produces empty result.
func TestExportEEE_EmptyTypes(t *testing.T) {
	result, err := export.ExportEEE(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TotalTypes != 0 {
		t.Errorf("TotalTypes = %d, want 0", result.TotalTypes)
	}
	if len(result.Configs) != 0 {
		t.Errorf("Configs length = %d, want 0", len(result.Configs))
	}
}

// TestEEEMapper_FormatName tests that EEEMapper.FormatName returns the official repo name.
func TestEEEMapper_FormatName(t *testing.T) {
	m := &export.EEEMapper{}
	if got := m.FormatName(); got != "every_eval_ever" {
		t.Errorf("FormatName() = %q, want %q", got, "every_eval_ever")
	}
}

// ---------------------------------------------------------------------------
// Generic Export[T] aggregation test (stub mapper)
// ---------------------------------------------------------------------------

// stubMetric is a minimal metric type for testing the generic Export[T] function.
type stubMetric struct {
	id string
}

// stubMapper implements MetricMapper[stubMetric] for testing generic aggregation.
type stubMapper struct{}

func (m *stubMapper) MapType(at store.AnnotationTypeRow) (stubMetric, error) {
	return stubMetric{id: at.TypeID}, nil
}

func (m *stubMapper) FormatName() string { return "stub" }

// TestExport_GenericAggregation verifies Export[T] aggregates correctly with any mapper.
func TestExport_GenericAggregation(t *testing.T) {
	types := []store.AnnotationTypeRow{
		{TypeID: "a"},
		{TypeID: "b"},
		{TypeID: "c"},
	}

	result, err := export.Export[stubMetric](&stubMapper{}, types)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TotalTypes != 3 {
		t.Errorf("TotalTypes = %d, want 3", result.TotalTypes)
	}
	if len(result.Configs) != 3 {
		t.Errorf("len(Configs) = %d, want 3", len(result.Configs))
	}
	if result.Configs[0].id != "a" {
		t.Errorf("Configs[0].id = %q, want %q", result.Configs[0].id, "a")
	}
	if result.Configs[1].id != "b" {
		t.Errorf("Configs[1].id = %q, want %q", result.Configs[1].id, "b")
	}
	if result.Configs[2].id != "c" {
		t.Errorf("Configs[2].id = %q, want %q", result.Configs[2].id, "c")
	}
}
