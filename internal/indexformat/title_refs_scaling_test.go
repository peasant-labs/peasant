package indexformat_test

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

// longGeneration is a valid complete generation with n main user entries,
// each one also a title ref: the shape of a long Claude session, where every
// user prompt can seed the title.
func longGeneration(n int) indexformat.Generation {
	in := fixtureGeneration{
		ID:                   "gen-long",
		Completeness:         "complete",
		SourceEvidenceDigest: "sha3-256:evidence-long",
		Metadata: fixtureMetadata{
			SessionID:    "11111111-1111-4111-8111-111111111111",
			ModelHarness: "claude-code",
			TurnCount:    n,
		},
	}
	count := int64(n)
	in.Metadata.InputSubmissionCount = &count
	for i := 0; i < n; i++ {
		ref := fmt.Sprintf("e-main-%d", i)
		in.Main.Entries = append(in.Main.Entries, fixtureEntry{EntryIndex: i, Role: "user", SourceEntryRef: ref})
		in.TitleRefs = append(in.TitleRefs, ref)
	}
	return buildGeneration(in)
}

// longSnapshot is the read-side view of longGeneration(n).
func longSnapshot(n int) indexformat.ReadSnapshot {
	count := int64(n)
	in := fixtureSnapshot{
		IndexVersion: 2,
		GenerationID: "gen-long",
		Completeness: "complete",
		Session:      fixtureSession{ID: "11111111-1111-4111-8111-111111111111", Harness: "claude-code", TurnCount: n, InputSubmissionCount: &count},
		Metadata:     fixtureMetadata{SessionID: "11111111-1111-4111-8111-111111111111", ModelHarness: "claude-code", TurnCount: n, InputSubmissionCount: &count},
	}
	for i := 0; i < n; i++ {
		ref := fmt.Sprintf("e-main-%d", i)
		in.Main.Entries = append(in.Main.Entries, fixtureEntry{EntryIndex: i, Role: "user", SourceEntryRef: ref})
		in.TitleRefs = append(in.TitleRefs, ref)
	}
	return buildSnapshot(in)
}

// validator is the shared shape of Generation and ReadSnapshot.
type validator interface{ Validate() error }

// validateBytes is the heap bytes one Validate call allocates.
func validateBytes(t *testing.T, g validator) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := g.Validate(); err != nil {
		t.Fatalf("long fixture is invalid: %v", err)
	}
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestGenerationValidateScalesLinearlyWithTitleRefs guards #672. Validate
// rebuilt the map of every main entry once per title ref, so its cost grew
// with entries × title refs and a long session took minutes to ingest. The
// sizes are small on purpose (per TESTING.md): quadrupling the session must
// allocate about four times the bytes, not sixteen.
// Not parallel: runtime.MemStats counts the whole process, so another test
// allocating at the same time would skew the ratio.
func TestGenerationValidateScalesLinearlyWithTitleRefs(t *testing.T) {
	for name, build := range map[string]func(int) validator{
		"generation": func(n int) validator { return longGeneration(n) },
		"snapshot":   func(n int) validator { return longSnapshot(n) },
	} {
		small := validateBytes(t, build(256))
		large := validateBytes(t, build(1024))
		if ratio := float64(large) / float64(small); ratio > 8 {
			t.Errorf("%s Validate allocated %.1fx the bytes for 4x the entries (%d -> %d); want linear growth (~4x)", name, ratio, small, large)
		}
	}
}

func BenchmarkGenerationValidateLongSession(b *testing.B) {
	for _, n := range []int{1000, 4000} {
		g := longGeneration(n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := g.Validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
