package ingest

import (
	"sort"
	"testing"
	"time"
)

// TestCodexParseOncePerformance is the SLICE-4 L3 proportional gate. It drives
// the production prepareCodexRecord native canonical path with the
// fixture-owned wide recipes (128/512/2048 siblings, fixed per-leaf size) and
// proves linear scaling: for 4x siblings/bytes, median time and allocation
// count grow by at most 6x after warm-up, under -race, over repeated samples.
//
// Source-review proof (paired with timing, never timing alone): the
// per-retained-sibling loop in prepareCodexRecord resolves each leaf with
// originalBlocks.at(field, index) -- O(1) slice resolution plus the leaf
// retain. No json.Unmarshal of the carried item or its content/summary arrays
// occurs inside that loop; indexCodexOriginalBlocks parses them once before
// normalization. Verify with:
//   grep -n "Unmarshal" internal/ingest/codex_unknown.go
// and confirm the sibling loop (for _, record := range nested) contains only
// parseCodexOriginalPointer + at + retain.
//
// This test asserts ratios only, never absolute wall-clock time, per the
// proposal gate.
func TestCodexParseOncePerformance(t *testing.T) {
	cases := loadCodexLexicalFixtures(t)
	var wide []codexLexicalCase
	for _, row := range cases {
		if row.WideSiblings > 0 {
			wide = append(wide, row)
		}
	}
	if len(wide) != 3 {
		t.Fatalf("wide recipes = %d, want 3 (128/512/2048)", len(wide))
	}
	sort.Slice(wide, func(i, j int) bool { return wide[i].WideSiblings < wide[j].WideSiblings })

	type measurement struct {
		name       string
		siblings   int
		sourceLen  int
		medianNs   int64
		allocs     float64
		samplesNs  []int64
	}
	measurements := make([]measurement, 0, len(wide))
	for _, row := range wide {
		record, _, _ := buildWideCarriedRecord(row.WideSiblings, row.LeafSize, row.UnknownEvery)
		position := UnknownSourcePosition{Line: 11, SourceID: "wide-stream", Public: codexPublicPosition("wide-thread\x00wide-stream", 11, 0)}
		// Warm-up: one full run discarded, per gate.
		if _, _, err := prepareCodexRecord([]byte(record), position, true); err != nil {
			t.Fatal(err)
		}
		const samples = 3
		times := make([]int64, 0, samples)
		for s := 0; s < samples; s++ {
			start := time.Now()
			_, unknown, err := prepareCodexRecord([]byte(record), position, true)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			if len(unknown) == 0 {
				t.Fatal("no retained leaves in performance sample")
			}
			times = append(times, elapsed.Nanoseconds())
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		median := times[len(times)/2]
		allocs := testing.AllocsPerRun(5, func() {
			_, _, _ = prepareCodexRecord([]byte(record), position, true)
		})
		measurements = append(measurements, measurement{
			name:      row.Name,
			siblings:  row.WideSiblings,
			sourceLen: len(record),
			medianNs:  median,
			allocs:    allocs,
			samplesNs: append([]int64(nil), times...),
		})
		t.Logf("%s: siblings=%d sourceBytes=%d medianNs=%d allocsPerRun=%.1f samplesNs=%v",
			row.Name, row.WideSiblings, len(record), median, allocs, times)
	}
	// 4x siblings/bytes must cost at most 6x time and 6x allocs (median).
	for i := 1; i < len(measurements); i++ {
		prev, cur := measurements[i-1], measurements[i]
		byteRatio := float64(cur.sourceLen) / float64(prev.sourceLen)
		timeRatio := float64(cur.medianNs) / float64(prev.medianNs)
		allocRatio := cur.allocs / prev.allocs
		t.Logf("ratio %s->%s: bytes=%.2f time=%.2f allocs=%.2f",
			prev.name, cur.name, byteRatio, timeRatio, allocRatio)
		if byteRatio < 3.5 || byteRatio > 4.5 {
			t.Fatalf("source byte ratio %.2f not ~4x; recipe sizes drifted", byteRatio)
		}
		if timeRatio > 6.0 {
			t.Fatalf("time ratio %.2f exceeds 6x for 4x input (%s->%s medians %dns vs %dns samples %v vs %v)",
				timeRatio, prev.name, cur.name, prev.medianNs, cur.medianNs, prev.samplesNs, cur.samplesNs)
		}
		if allocRatio > 6.0 {
			t.Fatalf("alloc ratio %.2f exceeds 6x for 4x input (%s->%s %.1f vs %.1f)",
				allocRatio, prev.name, cur.name, prev.allocs, cur.allocs)
		}
	}
}
