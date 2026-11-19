package sourcepreview_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
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
// web options and one segment per request, under the web source cap and two
// larger caps, and
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
//
// The held-call runs read the same transcript with one tool call at its head
// that no record answers. That is the worst case for the hold: the capture
// hands out no turn until the cap, then all of them at once, so the segmenter
// buffers every turn it read. They run at the web cap and at the terminal's.
func BenchmarkCaptureWebHeap(b *testing.B) {
	listing := writeGenerated(b, 7_700, 4_000, 0)
	held := writeGeneratedWithHeldCall(b, 7_700, 4_000)
	// The web source cap, then the larger caps the rule refused, so the curve
	// stays visible.
	caps := []int64{defaults.KickstartWebPreviewSourceCapBytes >> 20, 24, 32}
	type sweep struct {
		name    string
		listing onboarding.SessionListing
		capMiB  int64
	}
	var sweeps []sweep
	for _, capMiB := range caps {
		sweeps = append(sweeps, sweep{"cap", listing, capMiB})
	}
	for _, capMiB := range []int64{defaults.KickstartWebPreviewSourceCapBytes >> 20, defaults.KickstartTerminalPreviewSourceCapBytes >> 20} {
		sweeps = append(sweeps, sweep{"held-call/cap", held, capMiB})
	}
	for _, run := range sweeps {
		listing, capMiB := run.listing, run.capMiB
		for _, gogc := range []int{100, 10} {
			b.Run(fmt.Sprintf("%s-%dMiB/gogc-%d", run.name, capMiB, gogc), func(b *testing.B) {
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
		BudgetBytes:   schema.TranscriptSegmentBudgetBytes,
		FrameCapBytes: defaults.KickstartWebPreviewFrameCapBytes,
		// The sweep measures the source cap, so the serialized cap never cuts
		// first; at the web source cap this is exactly the web's value.
		SerializedCapBytes: max(defaults.KickstartWebPreviewSerializedCapBytes, sourceCap),
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

// writeGeneratedWithHeldCall is writeGenerated with a tool call at the head of
// the session that no record answers.
func writeGeneratedWithHeldCall(b *testing.B, pairs, answerBytes int) onboarding.SessionListing {
	b.Helper()
	lines := []string{
		`{"type":"user","uuid":"held-u","timestamp":"2026-01-01T00:00:00.000Z","message":{"role":"user","content":"start a long task."}}`,
		`{"type":"assistant","uuid":"held-a","parentUuid":"held-u","timestamp":"2026-01-01T00:00:00.500Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"call_held","name":"Bash","input":{"command":"sleep 3600"}}]}}`,
	}
	generated := generatedClaudeLines(pairs, answerBytes)
	generated[0] = strings.Replace(generated[0], `"uuid":"u0",`, `"uuid":"u0","parentUuid":"held-a",`, 1)
	lines = append(lines, generated...)
	dir := b.TempDir()
	path := filepath.Join(dir, fixtureSessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		b.Fatalf("write the generated transcript: %v", err)
	}
	return onboarding.SessionListing{SessionID: fixtureSessionID, Harness: "claude-code",
		Source: onboarding.SessionSource{Path: path, Root: dir, Origin: onboarding.SessionSourceOriginFile}}
}

func median(values []uint64) uint64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}
