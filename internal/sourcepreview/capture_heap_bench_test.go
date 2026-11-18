package sourcepreview_test

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// heapObjectsMetric is the heap held by objects, live or not yet swept: what
// HeapAlloc reports, read without stopping the world.
const heapObjectsMetric = "/memory/classes/heap/objects:bytes"

// heapSampleInterval is how often the sampler reads the heap during a capture.
const heapSampleInterval = 100 * time.Microsecond

// heapSampler records the largest heap-object size seen while it runs.
type heapSampler struct {
	stop chan struct{}
	done sync.WaitGroup
	peak uint64
}

func startHeapSampler() *heapSampler {
	sampler := &heapSampler{stop: make(chan struct{})}
	sampler.done.Add(1)
	go func() {
		defer sampler.done.Done()
		sample := []metrics.Sample{{Name: heapObjectsMetric}}
		ticker := time.NewTicker(heapSampleInterval)
		defer ticker.Stop()
		for {
			metrics.Read(sample)
			sampler.peak = max(sampler.peak, sample[0].Value.Uint64())
			select {
			case <-sampler.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return sampler
}

func (s *heapSampler) finish() uint64 {
	close(s.stop)
	s.done.Wait()
	return s.peak
}

// BenchmarkCaptureWebHeap measures what one web capture holds in memory, for
// the decision on the web source cap: the plan lowers the cap when the
// transient peak of a capture passes 128 MiB.
//
// It reads the 32 MiB synthetic transcript of BenchmarkCaptureWeb32MiB with the
// web options and one segment per request, under three source caps, and
// reports per cap the median of the runs of:
//
//   - retained-MiB: the largest heap left after a forced GC following each
//     segment, which is what the capture keeps between requests (loaded
//     prefix, digests, buffered turns) plus the benchmark's own small state;
//   - peak-MiB: the largest heap-object size sampled every 100µs during the
//     whole capture, which the largest re-parse dominates. Under gogc-100 this
//     is what a default server process reaches; under gogc-10 the collector
//     runs ten times as often, so the peak approaches the live transient;
//   - k: peak-MiB divided by the source cap, the expansion factor of a
//     re-parse.
//
// Peasant's server sets neither GOGC nor a memory limit, so gogc-100 is what a
// user's process does.
//
//	go test ./internal/sourcepreview -run '^$' -bench BenchmarkCaptureWebHeap -benchtime 3x
func BenchmarkCaptureWebHeap(b *testing.B) {
	listing := writeGenerated(b, 7_700, 4_000, 0)
	for _, capMiB := range []int64{16, 24, 32} {
		for _, gogc := range []int{100, 10} {
			b.Run(fmt.Sprintf("cap-%dMiB/gogc-%d", capMiB, gogc), func(b *testing.B) {
				previous := debug.SetGCPercent(gogc)
				defer debug.SetGCPercent(previous)
				var retained, peaks []uint64
				for b.Loop() {
					keep, peak := measureCaptureHeap(b, listing, capMiB<<20)
					retained = append(retained, keep)
					peaks = append(peaks, peak)
				}
				peak := median(peaks)
				b.ReportMetric(float64(median(retained))/(1<<20), "retained-MiB")
				b.ReportMetric(float64(peak)/(1<<20), "peak-MiB")
				b.ReportMetric(float64(peak)/float64(capMiB<<20), "k")
			})
		}
	}
}

func measureCaptureHeap(b *testing.B, listing onboarding.SessionListing, sourceCap int64) (retained, peak uint64) {
	b.Helper()
	reader := sourcepreview.NewReader(&ingest.OSFileSystem{}, []onboarding.SessionListing{listing}, sourcepreview.WithGitResolver(testutil.NoGitResolver()))
	captureOpts := sourcepreview.CaptureOptions{
		FirstReadBytes: defaults.TranscriptInitialReadBytes,
		SliceReadBytes: defaults.TranscriptContinuationReadBytes,
		SourceCapBytes: sourceCap,
		TurnCap:        defaults.KickstartWebPreviewTurnCap,
	}
	segmentOpts := sourcepreview.SegmenterOptions{
		BudgetBytes:        schema.TranscriptSegmentBudgetBytes,
		FrameCapBytes:      defaults.KickstartWebPreviewFrameCapBytes,
		SerializedCapBytes: defaults.KickstartWebPreviewSerializedCapBytes,
	}
	runtime.GC()
	sampler := startHeapSampler()
	capture, page, err := reader.Open(b.Context(), fixtureSessionID, captureOpts)
	if err != nil {
		b.Fatalf("open the capture: %v", err)
	}
	defer capture.Close()
	seg, err := sourcepreview.NewSegmenter("1", segmentOpts)
	if err != nil {
		b.Fatalf("build the segmenter: %v", err)
	}
	seg.Push(page)
	ended := page.Continuation != sourcepreview.ContinuationMore
	var stats runtime.MemStats
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
		// The forced GC is not part of the transient: pause the sampler's
		// view of it by reading the retained set only after collection.
		runtime.GC()
		runtime.ReadMemStats(&stats)
		retained = max(retained, stats.HeapAlloc)
	}
	return retained, sampler.finish()
}

func median(values []uint64) uint64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}
