package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

const (
	benchBlobCount = 1000
	benchBlobBytes = 4096
	benchSessionID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

// benchGeneration builds one generation whose content records are all
// retained (non-emitted) blobs, shaped like the per-part generation a large
// OpenCode session produces: many small blobs, each staged idempotently
// without a lock. The records stay in a captured context segment so the
// generation is self-contained.
func benchGeneration(tb testing.TB, n int) (indexformat.Generation, map[schema.SourceEntryRef][]byte) {
	tb.Helper()
	payload := []byte(strings.Repeat("x", benchBlobBytes))
	content := make([]indexformat.ContentRecord, 0, n)
	blobs := make(map[schema.SourceEntryRef][]byte, n)
	refs := make([]schema.SourceEntryRef, 0, n)
	for i := 0; i < n; i++ {
		ref := schema.SourceEntryRef(fmt.Sprintf("e_%06d", i))
		content = append(content, indexformat.ContentRecord{Ref: ref})
		blobs[ref] = payload
		refs = append(refs, ref)
	}
	sid, err := schema.NewSessionID(benchSessionID)
	if err != nil {
		tb.Fatal(err)
	}
	inputCount := int64(1)
	generation := indexformat.Generation{
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  ingest.HarnessOpenCode,
			Stats:         schema.SessionStats{InputSubmissionCount: &inputCount},
		},
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("a", 64),
		Segments: []indexformat.ContextSegment{{
			PhysicalSourceID: "bench-source",
			Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
			Inclusion:        indexformat.SegmentInclusionInherited,
			CapturedRefs:     refs,
		}},
	}
	return generation, blobs
}

// BenchmarkStageGeneration measures one staging call for a 1000-blob
// generation shaped like the per-part generation a large OpenCode session
// produces: many small retained blobs staged idempotently without a lock.
func BenchmarkStageGeneration(b *testing.B) {
	s, _ := openGenerationStore(b)
	seedGenerationSession(b, s, benchSessionID)
	generation, blobs := benchGeneration(b, benchBlobCount)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		generation.ID = fmt.Sprintf("g_bench_%d", i)
		filled := filledCandidateForValidation(b, indexformat.V2{Generation: generation}, blobs)
		if _, err := s.StageGeneration(context.Background(), GenerationActivation{Generation: filled, Blobs: blobs}); err != nil {
			b.Fatal(err)
		}
	}
}
