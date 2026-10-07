package ingest

import (
	"sort"
	"testing"
)

// TestCodexParseOncePerformance is the proportional gate for the Codex native
// canonical path. It drives the production prepareCodexRecord with the
// fixture-owned wide recipes (128/512/2048 siblings, fixed per-leaf size) and
// gates on ALLOCATION scaling: for 4x siblings/bytes, allocations per parse
// grow by at most 6x.
//
// Allocations are deterministic, so this gate is reproducible on any runner.
// Wall-clock time is deliberately NOT asserted here: a ratio of sample times on
// a shared CI runner flakes under load, and a flaky gate is worse than no gate.
// The timing signal lives in BenchmarkCodexParseOnce, and the CPU complexity
// (parse once, O(1) leaf resolution) is proven by source review alongside it:
// the per-retained-sibling loop in prepareCodexRecord resolves each leaf with
// originalBlocks.at(field, index) -- O(1) slice resolution plus the leaf
// retain -- with no json.Unmarshal of the carried item or its content/summary
// arrays inside the loop; indexCodexOriginalBlocks parses them once before
// normalization. Verify with
//
//	grep -n "Unmarshal" internal/ingest/codex_unknown.go
//
// and confirm the sibling loop (for _, record := range nested) contains only
// parseCodexOriginalPointer + at + retain.
func TestCodexParseOncePerformance(t *testing.T) {
	wide := wideCodexLexicalCases(t)

	type measurement struct {
		name      string
		siblings  int
		sourceLen int
		allocs    float64
	}
	measurements := make([]measurement, 0, len(wide))
	for _, row := range wide {
		record, _, _ := buildWideCarriedRecord(row.WideSiblings, row.LeafSize, row.UnknownEvery)
		position := UnknownSourcePosition{Line: 11, SourceID: "wide-stream", Public: codexPublicPosition("wide-thread\x00wide-stream", 11, 0)}
		// One explicit run proves the recipe retains leaves before the
		// allocation measurement, so an empty-recipe mistake fails loudly.
		if _, unknown, err := prepareCodexRecord([]byte(record), position, true); err != nil {
			t.Fatal(err)
		} else if len(unknown) == 0 {
			t.Fatal("no retained leaves in the wide recipe")
		}
		allocs := testing.AllocsPerRun(5, func() {
			if _, _, err := prepareCodexRecord([]byte(record), position, true); err != nil {
				t.Fatal(err)
			}
		})
		measurements = append(measurements, measurement{
			name:      row.Name,
			siblings:  row.WideSiblings,
			sourceLen: len(record),
			allocs:    allocs,
		})
		t.Logf("%s: siblings=%d sourceBytes=%d allocsPerRun=%.1f", row.Name, row.WideSiblings, len(record), allocs)
	}
	// 4x siblings/bytes must cost at most 6x allocations.
	for i := 1; i < len(measurements); i++ {
		prev, cur := measurements[i-1], measurements[i]
		byteRatio := float64(cur.sourceLen) / float64(prev.sourceLen)
		allocRatio := cur.allocs / prev.allocs
		t.Logf("ratio %s->%s: bytes=%.2f allocs=%.2f", prev.name, cur.name, byteRatio, allocRatio)
		if byteRatio < 3.5 || byteRatio > 4.5 {
			t.Fatalf("source byte ratio %.2f not ~4x; recipe sizes drifted", byteRatio)
		}
		if allocRatio > 6.0 {
			t.Fatalf("alloc ratio %.2f exceeds 6x for 4x input (%s->%s %.1f vs %.1f)",
				allocRatio, prev.name, cur.name, prev.allocs, cur.allocs)
		}
	}
}

// wideCodexLexicalCases returns the fixture-owned wide recipes (128/512/2048
// siblings) sorted by sibling count, failing when the fixture no longer carries
// exactly the three wide shapes the proportional gate and the benchmark assume.
func wideCodexLexicalCases(t testing.TB) []codexLexicalCase {
	t.Helper()
	var wide []codexLexicalCase
	for _, row := range loadCodexLexicalFixtures(t) {
		if row.WideSiblings > 0 {
			wide = append(wide, row)
		}
	}
	if len(wide) != 3 {
		t.Fatalf("wide recipes = %d, want 3 (128/512/2048)", len(wide))
	}
	sort.Slice(wide, func(i, j int) bool { return wide[i].WideSiblings < wide[j].WideSiblings })
	return wide
}
