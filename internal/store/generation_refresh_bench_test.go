package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// BenchmarkManagedGenerationMetadataRefresh measures the clone alone for an
// already-active managed generation: the store operation that copies the entry
// mapping and structural rows under a new generation id, refreshes the
// metadata-derived columns and children, and commits one transaction. The
// source generation is installed OUTSIDE the timed region, so the number is the
// metadata-only cost a later full-refresh benchmark can subtract from the parse
// lane. It touches only synthetic, test-owned data.
func BenchmarkManagedGenerationMetadataRefresh(b *testing.B) {
	for _, entries := range []int{2000, 20000} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			s := openBenchGenerationStore(b)
			defer func() { _ = s.Close() }()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				sid, err := schema.NewSessionID(fmt.Sprintf("dddddddd-dddd-4ddd-8ddd-%012d", i))
				if err != nil {
					b.Fatal(err)
				}
				seedBenchGenerationSession(b, s, sid)
				candidate := benchV2InstallGeneration(sid, fmt.Sprintf("g_refresh_source_%d", i), entries)
				state, err := s.ReadIndexState(ctx, ingest.SessionID(sid))
				if err != nil {
					b.Fatalf("read the seeded index state: %v", err)
				}
				inputHash := strings.Repeat("b", 64)
				artifact := strings.Repeat("c", 64)
				outcome, err := s.ActivateGeneration(ctx, GenerationActivation{
					Generation: candidate, IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
					IndexedAtMs: 1700000005000, ExpectedState: state, IndexedInputHash: &inputHash, ArtifactIdentity: &artifact,
				})
				if err != nil {
					b.Fatalf("seed the source generation: %v", err)
				}
				if outcome.Disposition != ingest.ActivationCommittedNow {
					b.Fatalf("source activation disposition = %v, want committed now", outcome.Disposition)
				}
				expected, err := s.ReadIndexState(ctx, ingest.SessionID(sid))
				if err != nil {
					b.Fatalf("read the source proof: %v", err)
				}
				refreshed := candidate.Generation.Metadata
				refreshed.ModelHarness = defaults.HarnessClaudeCode
				refreshed.Version = fmt.Sprintf("refresh-%d", i)
				b.StartTimer()
				_, err = s.RefreshManagedGeneration(ctx, ManagedGenerationRefresh{
					SessionID: sid, SourceGenerationID: candidate.Generation.ID,
					Metadata: refreshed, Expected: expected,
				})
				if err != nil {
					b.Fatalf("refresh the source generation: %v", err)
				}
			}
		})
	}
}
