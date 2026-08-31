package ingest

import (
	"context"
	"sync"
	"testing"
	"time"
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

	select {
	case <-classifier.secondStarted:
	case err := <-done:
		t.Fatalf("stageAnnotateBuffered returned before second prepare blocked: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second prepare to block")
	}

	select {
	case <-classifier.firstFlush:
	case err := <-done:
		t.Fatalf("stageAnnotateBuffered returned before interval flush: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for interval flush")
	}
	waitForAnnotationBufferProgress(t, progress, 1)

	close(classifier.unblockSecond)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stageAnnotateBuffered: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for buffered annotate stage to finish")
	}
	waitForAnnotationBufferProgress(t, progress, 2)

	classifier.mu.Lock()
	defer classifier.mu.Unlock()
	if len(classifier.flushes) < 2 {
		t.Fatalf("flush count = %d, want at least 2", len(classifier.flushes))
	}
	if len(classifier.flushes[0]) != 1 || classifier.flushes[0][0] != sidA {
		t.Fatalf("first flush = %+v, want only first session", classifier.flushes[0])
	}
}

func waitForAnnotationBufferProgress(t *testing.T, progress *ProgressState, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if got := progress.Snapshot()[StageAnnotate].Done; got >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("ANNOTATE progress did not reach %d", want)
		case <-tick.C:
		}
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
