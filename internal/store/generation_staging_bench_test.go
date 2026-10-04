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

// benchGeneration builds one generation whose content records all have a
// captured blob, shaped like the per-part generation a large OpenCode session
// produces: many small files, each fsynced before the manifest and rename.
func benchGeneration(tb testing.TB, n int) (indexformat.Generation, map[schema.SourceEntryRef][]byte) {
	tb.Helper()
	payload := []byte(strings.Repeat("x", benchBlobBytes))
	content := make([]indexformat.ContentRecord, 0, n)
	blobs := make(map[schema.SourceEntryRef][]byte, n)
	for i := 0; i < n; i++ {
		ref := schema.SourceEntryRef(fmt.Sprintf("e_%06d", i))
		content = append(content, indexformat.ContentRecord{Ref: ref})
		blobs[ref] = payload
	}
	sid, err := schema.NewSessionID(benchSessionID)
	if err != nil {
		tb.Fatal(err)
	}
	generation := indexformat.Generation{
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  ingest.HarnessOpenCode,
		},
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("a", 64),
	}
	return generation, blobs
}

// BenchmarkStageGeneration measures one staging call for a 1000-blob
// generation with different blob-writer counts. The store-wide slot pool is
// sized to the per-call worker count so the single staging call under test is
// not bounded by the shared pool.
func BenchmarkStageGeneration(b *testing.B) {
	for _, workers := range []int{1, 4, 8, 16} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			artifacts, err := NewOSGenerationArtifactStore(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			concrete := artifacts.(*osGenerationArtifactStore)
			concrete.blobWorkers = workers
			concrete.blobSlots = make(chan struct{}, workers)
			generation, blobs := benchGeneration(b, benchBlobCount)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				generation.ID = fmt.Sprintf("g_bench_%d_%d", workers, i)
				if _, err := concrete.Stage(context.Background(), generation, blobs); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
