package ingest

import "testing"

// BenchmarkCodexParseOnce measures the production prepareCodexRecord native
// canonical path over the fixture-owned wide recipes (128/512/2048 siblings,
// fixed per-leaf size). It is the timing signal TestCodexParseOncePerformance
// deliberately no longer asserts: run it with
//
//	go test -bench BenchmarkCodexParseOnce -benchmem ./internal/ingest/
//
// and compare across revisions (benchstat) rather than gating a shared CI
// runner on a wall-clock ratio.
func BenchmarkCodexParseOnce(b *testing.B) {
	for _, row := range wideCodexLexicalCases(b) {
		record, _, _ := buildWideCarriedRecord(row.WideSiblings, row.LeafSize, row.UnknownEvery)
		position := UnknownSourcePosition{Line: 11, SourceID: "wide-stream", Public: codexPublicPosition("wide-thread\x00wide-stream", 11, 0)}
		payload := []byte(record)
		b.Run(row.Name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				if _, _, err := prepareCodexRecord(payload, position, true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
