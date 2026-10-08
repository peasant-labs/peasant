package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// sweepRecordingStore is a dependency fake for the harvest sweep wiring:
// every activation records its commit, every sweep records its session, and
// either side can fail on demand. It proves the pipeline calls the sweep
// after a commit and never on a refusal, without a database.
type sweepRecordingStore struct {
	MetricsStore

	mu          sync.Mutex
	activated   []SessionID
	swept       []SessionID
	flagged     int
	flaggedErr  error
	failCommit  bool
	failSweep   bool
	failFlagged bool
}

func (s *sweepRecordingStore) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	return results
}

func (s *sweepRecordingStore) ActivateNativeGeneration(_ context.Context, activation NativeGenerationActivation) (ActivationOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := activation.Generation.Generation.Metadata.SessionID
	s.activated = append(s.activated, id)
	if s.failCommit {
		return ActivationOutcome{Disposition: ActivationNotCommitted}, errors.New("injected activation refusal; nothing was committed")
	}
	return ActivationOutcome{Disposition: ActivationCommittedNow, CandidateID: activation.Generation.Generation.ID}, nil
}

func (s *sweepRecordingStore) SweepSessionForHarvest(_ context.Context, session SessionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.swept = append(s.swept, session)
	if s.failSweep {
		return errors.New("injected sweep failure; the commit stays durable and the flag stays set")
	}
	return nil
}

func (s *sweepRecordingStore) SweepFlaggedSessionsForHarvest(_ context.Context) (int, []error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flagged++
	if s.failFlagged {
		return 0, nil, errors.New("injected flagged-listing failure; no session was swept")
	}
	return 2, nil, nil
}

var (
	_ NativeGenerationActivator = (*sweepRecordingStore)(nil)
	_ ContentSweeper            = (*sweepRecordingStore)(nil)
)

// TestPipelineSweepsCommittedSession proves the after-commit wiring: a
// native candidate the store commits is swept through the harvest entry
// point before the flush reports it, so the staging flag cannot stay set
// past its own harvest.
func TestPipelineSweepsCommittedSession(t *testing.T) {
	t.Parallel()
	fake := &sweepRecordingStore{}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: fake}
	pipeline.flushIndexParseResults(context.Background(), []indexParseResult{benchNativeResult(t, 0, 2)}, IndexOutcomeIndexed, "test", nil)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.activated) != 1 {
		t.Fatalf("activations = %d, want the 1 committed candidate", len(fake.activated))
	}
	if len(fake.swept) != 1 || fake.swept[0] != fake.activated[0] {
		t.Fatalf("swept sessions = %v, want the committed session %v", fake.swept, fake.activated)
	}
}

// TestPipelineSkipsSweepOnRefusal proves a refused activation sweeps
// nothing: with no commit there is no superseded set and no flag the
// commit path owns.
func TestPipelineSkipsSweepOnRefusal(t *testing.T) {
	t.Parallel()
	fake := &sweepRecordingStore{failCommit: true}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: fake}
	pipeline.flushIndexParseResults(context.Background(), []indexParseResult{benchNativeResult(t, 1, 2)}, IndexOutcomeIndexed, "test", nil)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.activated) != 1 {
		t.Fatalf("activations = %d, want the 1 refused attempt", len(fake.activated))
	}
	if len(fake.swept) != 0 {
		t.Fatalf("swept sessions = %v, want none after a refusal", fake.swept)
	}
}

// TestPipelineSweepFailureKeepsCommit proves a sweep failure never fails
// the session: the commit is durable, the failure is a warning, and the
// flush still reports the session indexed.
func TestPipelineSweepFailureKeepsCommit(t *testing.T) {
	t.Parallel()
	fake := &sweepRecordingStore{failSweep: true}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: fake}
	flush := pipeline.flushIndexParseResults(context.Background(), []indexParseResult{benchNativeResult(t, 2, 2)}, IndexOutcomeIndexed, "test", nil)
	if len(flush.indexed) != 1 || !flush.indexed[0].indexed {
		t.Fatalf("flush reported %+v, want the committed session indexed despite the sweep failure", flush)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.swept) != 1 {
		t.Fatalf("swept sessions = %v, want the attempt that failed", fake.swept)
	}
}

// TestPipelineWithoutSweeperSkipsSweep proves an older store keeps working:
// without the sweep capability the harvest commits exactly as before and
// never observes the new entry points.
func TestPipelineWithoutSweeperSkipsSweep(t *testing.T) {
	t.Parallel()
	plain := newGatedNativeStore()
	close(plain.gate)
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: plain}
	pipeline.flushIndexParseResults(context.Background(), []indexParseResult{benchNativeResult(t, 3, 1)}, IndexOutcomeIndexed, "test", nil)
	plain.mu.Lock()
	defer plain.mu.Unlock()
	if len(plain.commits) != 1 {
		t.Fatalf("commits = %v, want the 1 candidate without any sweep capability", plain.commits)
	}
}

// TestPipelineSweepFlaggedSessionsAtStart proves the harvest-start wiring:
// a sweeping store is swept before discovery, per-session warnings do not
// fail the pass, a listing failure aborts it, and a store without the
// capability is skipped silently.
func TestPipelineSweepFlaggedSessionsAtStart(t *testing.T) {
	t.Parallel()
	fake := &sweepRecordingStore{}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: fake}
	if err := pipeline.sweepFlaggedSessionsAtStart(context.Background()); err != nil {
		t.Fatalf("harvest-start sweep: %v", err)
	}
	fake.mu.Lock()
	flagged := fake.flagged
	fake.mu.Unlock()
	if flagged != 1 {
		t.Fatalf("flagged passes = %d, want 1", flagged)
	}

	legacy := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: newGatedNativeStore()}
	if err := legacy.sweepFlaggedSessionsAtStart(context.Background()); err != nil {
		t.Fatalf("harvest-start sweep without the capability: %v", err)
	}

	broken := &sweepRecordingStore{failFlagged: true}
	brokenPipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: broken}
	if err := brokenPipeline.sweepFlaggedSessionsAtStart(context.Background()); err == nil {
		t.Fatal("harvest-start sweep with a listing failure succeeded; it must abort the harvest")
	}
}
