package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
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

// TestV2PointerSpellingRefusedAtStoreWrite ties the asV2Value unit pin to
// production wiring. A *V2 result driven through IndexSessionEntryBatch is
// refused fail-closed at the index-format boundary (the V2 handler accepts
// only the value spelling), and the previously activated complete generation
// stays visible. The incomplete_new producer-revision guard is driven through
// the value spelling on the same path, proving the normalized guard runs at
// store-write level rather than only in the unit.
func TestV2PointerSpellingRefusedAtStoreWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openGenerationStore(t)
	const sidStr = "11111111-2222-3333-4444-555555555555"
	seedGenerationSession(t, s, sidStr)
	id, err := schema.NewSessionID(sidStr)
	if err != nil {
		t.Fatal(err)
	}
	complete, completeBlobs := buildTestGeneration(t, id, "gen-complete", "complete text", "complete input", "complete output")
	if err := activateTestGeneration(t, s, complete, completeBlobs); err != nil {
		t.Fatalf("activate prior complete generation: %v", err)
	}
	incomplete, blobs := buildIncompleteGeneration(t, id, "gen-pointer", "pointer text", "pointer input", "pointer output")
	filled := filledCandidateForValidation(t, incomplete, blobs)

	// Pointer spelling: refused at the format boundary before any replacement.
	// The candidate is otherwise valid (filled + validated above), so the
	// refusal can only come from the spelling, never from generation shape.
	refused := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: ingest.SessionID(sidStr), Result: &filled, IndexVersion: 2,
	}})
	if len(refused) != 1 || refused[0].Err == nil {
		t.Fatalf("pointer *V2 write certified: %+v", refused)
	}
	if !strings.Contains(refused[0].Err.Error(), "*indexformat.V2") {
		t.Fatalf("pointer refusal does not name the spelling: %v", refused[0].Err)
	}
	if got := visibleGeneration(t, s, id); got != "gen-complete" {
		t.Fatalf("pointer write replaced the visible generation: %q", got)
	}

	// Value spelling claiming a producer revision: refused by the normalized
	// incomplete_new guard, prior generation preserved.
	claimed := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: ingest.SessionID(sidStr), Result: filled, IndexVersion: 2, IndexerVersion: 5,
	}})
	if len(claimed) != 1 || claimed[0].Err == nil {
		t.Fatalf("incomplete value write claiming producer revision certified: %+v", claimed)
	}
	if !strings.Contains(claimed[0].Err.Error(), "claims producer revision") {
		t.Fatalf("producer-revision refusal is not the incomplete_new guard: %v", claimed[0].Err)
	}
	if got := visibleGeneration(t, s, id); got != "gen-complete" {
		t.Fatalf("guarded write replaced the visible generation: %q", got)
	}
}
