package sourcepreview_test

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// BenchmarkCaptureWeb32MiB reads a 32 MiB synthetic Claude Code transcript the
// way the web preview does: the web capture options, the web segmenter options
// and one segment per request, a source read only when no turn is buffered,
// until the source cap cuts the capture. It reports the first-segment latency,
// the mean latency of one later segment, the whole capture's wall time and the
// peak heap sampled after every segment.
//
// The targets in internal/defaults warn; the ceilings fail the benchmark, and
// exist to catch a catastrophic regression (a re-parse per segment, a lost
// bound), not runner contention.
//
//	go test ./internal/sourcepreview -run '^$' -bench BenchmarkCaptureWeb32MiB -benchtime 3x
func BenchmarkCaptureWeb32MiB(b *testing.B) {
	// A pair is about 4.3 KiB, so 7,700 pairs is just over 32 MiB.
	listing := writeGenerated(b, 7_700, 4_000, 0)
	reader := sourcepreview.NewReader(&ingest.OSFileSystem{}, []onboarding.SessionListing{listing}, sourcepreview.WithGitResolver(testutil.NoGitResolver()))
	captureOpts := sourcepreview.CaptureOptions{
		FirstReadBytes: defaults.TranscriptInitialReadBytes,
		SliceReadBytes: defaults.TranscriptContinuationReadBytes,
		SourceCapBytes: defaults.KickstartWebPreviewSourceCapBytes,
		TurnCap:        defaults.KickstartWebPreviewTurnCap,
	}
	segmentOpts := sourcepreview.SegmenterOptions{
		BudgetBytes:        schema.TranscriptSegmentBudgetBytes,
		FrameCapBytes:      defaults.KickstartWebPreviewFrameCapBytes,
		SerializedCapBytes: defaults.KickstartWebPreviewSerializedCapBytes,
	}
	var firstTotal, laterTotal, wallTotal time.Duration
	var laterCount, segmentsTotal int
	var peakHeap uint64
	var stats runtime.MemStats
	runs := 0
	for b.Loop() {
		runs++
		runtime.GC()
		start := time.Now()
		capture, page, err := reader.Open(b.Context(), fixtureSessionID, captureOpts)
		if err != nil {
			b.Fatalf("open the capture: %v", err)
		}
		seg, err := sourcepreview.NewSegmenter("1", segmentOpts)
		if err != nil {
			b.Fatalf("build the segmenter: %v", err)
		}
		seg.Push(page)
		ended := page.Continuation != sourcepreview.ContinuationMore
		segments := 0
		last := start
		for {
			for !seg.Pending() && !ended {
				page, err = capture.Next(b.Context())
				if err != nil {
					b.Fatalf("read the capture: %v", err)
				}
				seg.Push(page)
				ended = page.Continuation != sourcepreview.ContinuationMore
			}
			_, ok, err := seg.Next(ended)
			if errors.Is(err, sourcepreview.ErrSerializedBudget) || (err == nil && !ok) {
				break
			}
			if err != nil {
				b.Fatalf("cut a segment: %v", err)
			}
			now := time.Now()
			if segments == 0 {
				firstTotal += now.Sub(start)
			} else {
				laterTotal += now.Sub(last)
				laterCount++
			}
			last = now
			segments++
			runtime.ReadMemStats(&stats)
			peakHeap = max(peakHeap, stats.HeapAlloc)
		}
		wallTotal += time.Since(start)
		segmentsTotal += segments
		capture.Close()
	}
	first := firstTotal / time.Duration(runs)
	wall := wallTotal / time.Duration(runs)
	perSegment := time.Duration(0)
	if laterCount > 0 {
		perSegment = laterTotal / time.Duration(laterCount)
	}
	b.ReportMetric(float64(first.Microseconds())/1000, "first-segment-ms")
	b.ReportMetric(float64(perSegment.Microseconds())/1000, "later-segment-ms")
	b.ReportMetric(float64(wall.Microseconds())/1000, "capture-ms")
	b.ReportMetric(float64(segmentsTotal)/float64(runs), "segments")
	b.ReportMetric(float64(peakHeap)/(1<<20), "peak-heap-MiB")
	if first > defaults.SourcePreviewFirstSegmentBudget {
		b.Logf("WARN: first segment took %s, over the %s target", first, defaults.SourcePreviewFirstSegmentBudget)
	}
	if wall > defaults.SourcePreviewFullCaptureBudget {
		b.Logf("WARN: the capture took %s, over the %s target", wall, defaults.SourcePreviewFullCaptureBudget)
	}
	if wall > defaults.SourcePreviewFullCaptureCeiling {
		b.Fatalf("the capture took %s, over the %s ceiling", wall, defaults.SourcePreviewFullCaptureCeiling)
	}
	if int64(peakHeap) > defaults.SourcePreviewPeakHeapCeilingBytes {
		b.Fatalf("the peak heap was %d MiB, over the %d MiB ceiling", peakHeap>>20, defaults.SourcePreviewPeakHeapCeilingBytes>>20)
	}
}
