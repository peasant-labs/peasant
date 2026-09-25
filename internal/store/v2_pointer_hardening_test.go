package store

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

// TestAsV2ValueHardening pins nil-safe normalization of both V2 spellings:
// value V2 and non-nil *V2 normalize to the generation; nil *V2 and non-V2
// report not-ok without panic. Store incomplete_new and Earlier-expansion
// guards use this, so a pointer caller cannot bypass them through the pointer
// form. No production *V2 caller exists yet; this hardens the pointer form
// alongside native callers for defense in depth.
func TestAsV2ValueHardening(t *testing.T) {
	t.Parallel()
	value := indexformat.V2{Generation: indexformat.Generation{
		ID:           "gen-1",
		Completeness: indexformat.GenerationCompletenessIncompleteNew,
	}}
	if got, ok := asV2Value(value); !ok || got.Generation.ID != "gen-1" ||
		got.Generation.Completeness != indexformat.GenerationCompletenessIncompleteNew {
		t.Fatalf("value V2 not normalized: %+v %v", got, ok)
	}
	if got, ok := asV2Value(&value); !ok || got.Generation.ID != "gen-1" {
		t.Fatalf("non-nil *V2 not normalized: %+v %v", got, ok)
	}
	var nilPtr *indexformat.V2
	if _, ok := asV2Value(nilPtr); ok {
		t.Fatal("nil *V2 reported ok; want not-ok (no generation)")
	}
	if _, ok := asV2Value(indexformat.V1{}); ok {
		t.Fatal("V1 reported as V2; want not-ok")
	}
	if !isManagedGenerationWrite(value) || !isManagedGenerationWrite(&value) {
		t.Fatal("managed write not recognized for both spellings")
	}
	if isManagedGenerationWrite(nilPtr) {
		t.Fatal("nil *V2 reported as managed write; want false")
	}
}
