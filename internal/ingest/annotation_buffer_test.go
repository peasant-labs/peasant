package ingest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/testwait"
)

type intervalBufferedClassifier struct {
	mu                sync.Mutex
	secondStarted     chan struct{}
	unblockSecond     chan struct{}
	firstFlush        chan struct{}
	secondStartedOnce sync.Once
	firstFlushOnce    sync.Once
	prepareCalls      int
	flushes           [][]SessionID
}

var _ BufferedSessionClassifier = (*intervalBufferedClassifier)(nil)

func newIntervalBufferedClassifier() *intervalBufferedClassifier {
	return &intervalBufferedClassifier{
		secondStarted: make(chan struct{}),
		unblockSecond: make(chan struct{}),
		firstFlush:    make(chan struct{}),
	}
}

func (c *intervalBufferedClassifier) Annotate(_ context.Context, _ SessionID) error {
	return nil
}

func (c *intervalBufferedClassifier) PrepareAnnotations(ctx context.Context, sessionID SessionID, _ *IndexProfiler) (SessionAnnotationBatch, error) {
	c.mu.Lock()
	c.prepareCalls++
	call := c.prepareCalls
	c.mu.Unlock()

	if call == 2 {
		c.secondStartedOnce.Do(func() { close(c.secondStarted) })
		select {
		case <-c.unblockSecond:
		case <-ctx.Done():
			return SessionAnnotationBatch{SessionID: sessionID}, ctx.Err()
		}
	}

	return SessionAnnotationBatch{
		SessionID: sessionID,
		Writes: []SessionAnnotationWrite{{
			TypeID:     "test.annotation",
			Value:      "prepared",
			TargetKind: AnnotationProfileTargetSession,
		}},
	}, nil
}

func (c *intervalBufferedClassifier) FlushAnnotationBatches(_ context.Context, batches []SessionAnnotationBatch, _ *IndexProfiler) []SessionAnnotationBatchResult {
	ids := make([]SessionID, len(batches))
	results := make([]SessionAnnotationBatchResult, len(batches))
	for i, batch := range batches {
		ids[i] = batch.SessionID
		results[i] = SessionAnnotationBatchResult{
			SessionID: batch.SessionID,
			Results: []ClassifierAnnotationWriteResult{{
				Dedup:        DedupCreate,
				AnnotationID: "buffered-annotation",
			}},
		}
	}
	c.mu.Lock()
	c.flushes = append(c.flushes, ids)
	c.firstFlushOnce.Do(func() { close(c.firstFlush) })
	c.mu.Unlock()
	return results
}

func TestStageAnnotateBuffered_FlushesAtRegularInterval(t *testing.T) {
	t.Parallel()
	sidA := mustAnnotationBufferSessionID(t, "00000000-0000-4000-8000-000000000001")
	sidB := mustAnnotationBufferSessionID(t, "00000000-0000-4000-8000-000000000002")
	classifier := newIntervalBufferedClassifier()
	progress := NewProgressState()
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- pipeline.stageAnnotateBuffered(ctx, []SessionID{sidA, sidB}, progress, classifier)
	}()

	waitCtx := testwait.Context(t)
	select {
	case <-classifier.secondStarted:
	case err := <-done:
		t.Fatalf("stageAnnotateBuffered returned before second prepare blocked: %v", err)
	case <-waitCtx.Done():
		t.Fatal("timed out waiting for second prepare to block")
	}

	select {
	case <-classifier.firstFlush:
	case err := <-done:
		t.Fatalf("stageAnnotateBuffered returned before interval flush: %v", err)
	case <-waitCtx.Done():
		t.Fatal("timed out waiting for interval flush")
	}
	// The advance lands in the result goroutine after the flush, and no push
	// signal exists for it.
	waitForStageProgress(t, progress, StageAnnotate, 1)

	close(classifier.unblockSecond)
	if err := testwait.Receive(t, done, "buffered annotate stage to finish"); err != nil {
		t.Fatalf("stageAnnotateBuffered: %v", err)
	}
	if got := progress.Snapshot()[StageAnnotate].Done; got < 2 {
		t.Fatalf("ANNOTATE progress done = %d, want at least 2", got)
	}

	classifier.mu.Lock()
	defer classifier.mu.Unlock()
	if len(classifier.flushes) < 2 {
		t.Fatalf("flush count = %d, want at least 2", len(classifier.flushes))
	}
	if len(classifier.flushes[0]) != 1 || classifier.flushes[0][0] != sidA {
		t.Fatalf("first flush = %+v, want only first session", classifier.flushes[0])
	}
}

func mustAnnotationBufferSessionID(t *testing.T, raw string) SessionID {
	t.Helper()
	sessionID, err := NewSessionID(raw)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", raw, err)
	}
	return sessionID
}

// TestStageAnnotateBuffered_FlushesAtConfiguredInterval drives the same
// stalled-input shape as the shipped-interval test, but with the flush
// interval set to 50 ms: the lane reads the knob, so the stalled first batch
// flushes on the configured interval, carrying only the first session — a
// filling batch never flushes early.
func TestStageAnnotateBuffered_FlushesAtConfiguredInterval(t *testing.T) {
	t.Parallel()
	sidA := mustAnnotationBufferSessionID(t, "00000000-0000-4000-8000-000000000011")
	sidB := mustAnnotationBufferSessionID(t, "00000000-0000-4000-8000-000000000012")
	classifier := newIntervalBufferedClassifier()
	progress := NewProgressState()
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1, Write: WriteConfig{FlushIntervalMs: 50}}}
	if got := pipeline.writeConfig().FlushInterval(); got != 50*time.Millisecond {
		t.Fatalf("FlushInterval()=%s, want 50ms: the lane must read the configured knob", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- pipeline.stageAnnotateBuffered(ctx, []SessionID{sidA, sidB}, progress, classifier)
	}()

	waitCtx := testwait.Context(t)
	select {
	case <-classifier.secondStarted:
	case err := <-done:
		t.Fatalf("stageAnnotateBuffered returned before second prepare blocked: %v", err)
	case <-waitCtx.Done():
		t.Fatal("timed out waiting for second prepare to block")
	}

	select {
	case <-classifier.firstFlush:
	case err := <-done:
		t.Fatalf("stageAnnotateBuffered returned before interval flush: %v", err)
	case <-waitCtx.Done():
		t.Fatal("timed out waiting for interval flush at the configured 50 ms")
	}

	close(classifier.unblockSecond)
	if err := testwait.Receive(t, done, "buffered annotate stage to finish"); err != nil {
		t.Fatalf("stageAnnotateBuffered: %v", err)
	}

	classifier.mu.Lock()
	defer classifier.mu.Unlock()
	if len(classifier.flushes) < 2 {
		t.Fatalf("flush count = %d, want at least 2", len(classifier.flushes))
	}
	if len(classifier.flushes[0]) != 1 || classifier.flushes[0][0] != sidA {
		t.Fatalf("first flush = %+v, want only first session", classifier.flushes[0])
	}
}

// TestStageAnnotateBuffered_HonorsLongConfiguredInterval proves the lane does
// not flush on the shipped 500 ms once configured otherwise: with the
// interval at an hour, a stalled batch waits instead of flushing. The 1.5 s
// quiet window carries its own deadline and runs 3× past the literal, so only
// an extreme scheduling stall could hide a regression — and it can never fail
// a lane that honors the knob.
func TestStageAnnotateBuffered_HonorsLongConfiguredInterval(t *testing.T) {
	t.Parallel()
	sidA := mustAnnotationBufferSessionID(t, "00000000-0000-4000-8000-000000000021")
	sidB := mustAnnotationBufferSessionID(t, "00000000-0000-4000-8000-000000000022")
	classifier := newIntervalBufferedClassifier()
	progress := NewProgressState()
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1, Write: WriteConfig{FlushIntervalMs: 3600000}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- pipeline.stageAnnotateBuffered(ctx, []SessionID{sidA, sidB}, progress, classifier)
	}()

	waitCtx := testwait.Context(t)
	select {
	case <-classifier.secondStarted:
	case err := <-done:
		t.Fatalf("stageAnnotateBuffered returned before second prepare blocked: %v", err)
	case <-waitCtx.Done():
		t.Fatal("timed out waiting for second prepare to block")
	}

	quiet := time.NewTimer(1500 * time.Millisecond)
	defer quiet.Stop()
	select {
	case <-classifier.firstFlush:
		t.Fatal("the lane flushed on the shipped 500 ms instead of waiting on the configured hour interval")
	case err := <-done:
		t.Fatalf("stageAnnotateBuffered returned while the second prepare was blocked: %v", err)
	case <-quiet.C:
	}

	close(classifier.unblockSecond)
	if err := testwait.Receive(t, done, "buffered annotate stage to finish"); err != nil {
		t.Fatalf("stageAnnotateBuffered: %v", err)
	}

	classifier.mu.Lock()
	defer classifier.mu.Unlock()
	// One final flush carries both sessions: nothing flushed early on the
	// shipped interval, and nothing was lost waiting on the configured one.
	if len(classifier.flushes) != 1 {
		t.Fatalf("flush count = %d, want exactly the final flush", len(classifier.flushes))
	}
	seen := make(map[SessionID]bool, 2)
	for _, id := range classifier.flushes[0] {
		seen[id] = true
	}
	if !seen[sidA] || !seen[sidB] || len(seen) != 2 {
		t.Fatalf("final flush = %+v, want both sessions", classifier.flushes[0])
	}
}
