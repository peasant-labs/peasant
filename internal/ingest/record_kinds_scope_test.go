package ingest

import (
	"reflect"
	"strings"
	"testing"
)

// recordKindConsumerScopeTokens are the naming fragments the registry and the
// harvest report must never carry. Rendering belongs to internal/transcript and
// to Fairtrade; the registry describes parser behavior and the report counts it.
var recordKindConsumerScopeTokens = []string{"renderer", "rendered", "visual", "viewer"}

// TestRecordKindSurfaceCarriesNoVisualizationState is the standing guard for the
// consumer boundary. The registry and the harvest report are parser-facing, so a
// renderer name, visualization flag, or viewer-coverage field added to either
// surface would leak Fairtrade's presentation state into a reporting artifact.
func TestRecordKindSurfaceCarriesNoVisualizationState(t *testing.T) {
	surfaces := []any{
		RecordKindRegistry{},
		RecordKindHarness{},
		RecordKindInventory{},
		RecordKindSource{},
		RecordKindVersions{},
		RecordKind{},
		RecordKindRefusalCount{},
		RetainedUnknownKindCount{},
		PipelineSummary{},
		PipelineResult{},
	}
	for _, surface := range surfaces {
		value := reflect.ValueOf(surface)
		if value.Kind() != reflect.Struct {
			t.Fatalf("%T is not a struct", surface)
		}
		for i := range value.NumField() {
			field := value.Type().Field(i)
			// The whole tag is scanned rather than one key at a time, so a
			// serialized name is caught whichever tag namespace carries it.
			for _, name := range []string{field.Name, string(field.Tag)} {
				lowered := strings.ToLower(name)
				for _, token := range recordKindConsumerScopeTokens {
					if strings.Contains(lowered, token) {
						t.Errorf("%s.%s carries %q: renderer and viewer state stay out of the registry and the harvest report", value.Type().Name(), field.Name, name)
					}
				}
			}
		}
	}
}
