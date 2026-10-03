package store

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// countingGenerationArtifacts counts Stage invocations while delegating every
// other method to the real owned-artifact store.
type countingGenerationArtifacts struct {
	GenerationArtifactStore
	stageCalls atomic.Int64
}

func (c *countingGenerationArtifacts) Stage(ctx context.Context, generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte) (indexformat.Generation, error) {
	c.stageCalls.Add(1)
	return c.GenerationArtifactStore.Stage(ctx, generation, blobs)
}

// openCountingGenerationStore opens a real store whose artifact staging is
// counted, so a test can prove pre-staged bytes are reused by activation
// instead of rewritten.
func openCountingGenerationStore(t *testing.T) (*Store, *countingGenerationArtifacts) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	inner, err := NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingGenerationArtifacts{GenerationArtifactStore: inner}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "generations.db"), WithPoolSize(2), WithIndexFormats(generationIndexFormat{}), WithGenerationArtifacts(counted, locker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, counted
}

func testActivation(t *testing.T, sid schema.SessionID, genID string) GenerationActivation {
	t.Helper()
	generation, blobs := buildTestGeneration(t, sid, genID, "prestage text", "prestage input", "prestage output")
	return GenerationActivation{
		Generation:     generation,
		Blobs:          blobs,
		IndexerVersion: 18,
		IndexedAtMs:    4242,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:          ingest.ContentCaptureIncomplete,
			SourceAuthority: ingest.ContentSourceNone,
			CaptureFormat:   ingest.ContentCaptureFormatPreviewOnly,
		},
	}
}

// TestStageGenerationReusesPreStagedFiles proves the pre-staging split: the
// staging phase writes and fsyncs the candidate and records its intent without
// committing, and the activation that follows replays the recorded intent
// instead of staging the same bytes a second time.
func TestStageGenerationReusesPreStagedFiles(t *testing.T) {
	sid, err := schema.NewSessionID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	s, artifacts := openCountingGenerationStore(t)
	seedGenerationSession(t, s, string(sid))

	activation := testActivation(t, sid, "gen_prestage")
	ctx := context.Background()

	if err := s.StageGeneration(ctx, activation); err != nil {
		t.Fatalf("StageGeneration: %v", err)
	}
	if got := artifacts.stageCalls.Load(); got != 1 {
		t.Fatalf("Stage calls after pre-staging = %d, want 1", got)
	}
	intent, err := s.generationArtifacts.ReadIntent(ctx, sid)
	if err != nil {
		t.Fatalf("ReadIntent after pre-staging: %v", err)
	}
	if intent == nil || intent.GenerationID != activation.Generation.Generation.ID {
		t.Fatalf("intent after pre-staging = %+v, want generation %s", intent, activation.Generation.Generation.ID)
	}

	outcome, err := s.ActivateGeneration(ctx, activation)
	if err != nil {
		t.Fatalf("ActivateGeneration after pre-staging: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("disposition = %v, want CommittedNow", outcome.Disposition)
	}
	if got := artifacts.stageCalls.Load(); got != 1 {
		t.Fatalf("Stage calls after activation = %d, want 1 (pre-staged files must be reused)", got)
	}
	cleared, err := s.generationArtifacts.ReadIntent(ctx, sid)
	if err != nil {
		t.Fatalf("ReadIntent after activation: %v", err)
	}
	if cleared != nil {
		t.Fatalf("intent after activation = %+v, want cleared", cleared)
	}
	state := readIndexStateForTest(t, s, sid)
	if state.IndexerVersion != 18 || state.IndexedAt == nil || *state.IndexedAt != 4242 {
		t.Fatalf("stamps = (%d,%v), want (18,4242)", state.IndexerVersion, state.IndexedAt)
	}
}

// TestStageGenerationConcurrentSessions proves independent sessions can stage
// in parallel under their own locks and still commit through the ordinary
// activation; the race detector is the second assertion.
func TestStageGenerationConcurrentSessions(t *testing.T) {
	ids := []string{
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	}
	s, artifacts := openCountingGenerationStore(t)
	activations := make([]GenerationActivation, len(ids))
	for i, raw := range ids {
		sid, err := schema.NewSessionID(raw)
		if err != nil {
			t.Fatal(err)
		}
		seedGenerationSession(t, s, string(sid))
		activations[i] = testActivation(t, sid, "gen_parallel_"+string(rune('a'+i)))
	}

	var wg sync.WaitGroup
	errs := make([]error, len(activations))
	for i := range activations {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.StageGeneration(context.Background(), activations[i])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent StageGeneration %d: %v", i, err)
		}
	}
	if got := artifacts.stageCalls.Load(); got != int64(len(activations)) {
		t.Fatalf("Stage calls after concurrent pre-staging = %d, want %d", got, len(activations))
	}
	for i, activation := range activations {
		outcome, err := s.ActivateGeneration(context.Background(), activation)
		if err != nil {
			t.Fatalf("ActivateGeneration %d: %v", i, err)
		}
		if outcome.Disposition != ingest.ActivationCommittedNow {
			t.Fatalf("activation %d disposition = %v, want CommittedNow", i, outcome.Disposition)
		}
	}
	if got := artifacts.stageCalls.Load(); got != int64(len(activations)) {
		t.Fatalf("Stage calls after activations = %d, want %d (pre-staged files must be reused)", got, len(activations))
	}
}
