package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

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

// tempGenerationDirs lists the owned temporary staging directories of one
// session.
func tempGenerationDirs(t *testing.T, root string, sid schema.SessionID) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, string(sid), "generations"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-gen-") {
			dirs = append(dirs, filepath.Join(root, string(sid), "generations", entry.Name()))
		}
	}
	return dirs
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("inode identity is unavailable on this platform")
	}
	return stat.Ino
}

func requireNoIntent(t *testing.T, s *Store, sid schema.SessionID) {
	t.Helper()
	intent, err := s.generationArtifacts.ReadIntent(context.Background(), sid)
	if err != nil {
		t.Fatalf("ReadIntent: %v", err)
	}
	if intent != nil {
		t.Fatalf("intent = %+v, want none", intent)
	}
}

func requireNotInstalled(t *testing.T, s *Store, sid schema.SessionID, genID string) {
	t.Helper()
	if _, err := s.generationArtifacts.ReadManifest(context.Background(), sid, genID); !errors.Is(err, ErrStagedGenerationAbsent) {
		t.Fatalf("ReadManifest(%s) err = %v, want ErrStagedGenerationAbsent", genID, err)
	}
}

// TestStageGenerationPreparesThenActivationInstalls proves the split: the
// preparation writes the candidate only into a temporary directory, with no
// intent and nothing installed, and the activation installs those same files
// (the renamed directory keeps its identity) instead of writing them again.
func TestStageGenerationPreparesThenActivationInstalls(t *testing.T) {
	sid, err := schema.NewSessionID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	s, root := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	activation := testActivation(t, sid, "gen_prestage")
	ctx := context.Background()

	prepared, err := s.StageGeneration(ctx, activation)
	if err != nil {
		t.Fatalf("StageGeneration: %v", err)
	}
	requireNoIntent(t, s, sid)
	requireNotInstalled(t, s, sid, "gen_prestage")
	temps := tempGenerationDirs(t, root, sid)
	if len(temps) != 1 {
		t.Fatalf("temporary directories after preparation = %v, want exactly one", temps)
	}
	preparedInode := inodeOf(t, temps[0])
	if active, err := s.activeGenerationID(ctx, sid); err != nil || active != "" {
		t.Fatalf("active after preparation = %q (err=%v), want none", active, err)
	}

	activation.Prepared = prepared
	outcome, err := s.ActivateGeneration(ctx, activation)
	if err != nil {
		t.Fatalf("ActivateGeneration with prepared files: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("disposition = %v, want CommittedNow", outcome.Disposition)
	}
	if got := inodeOf(t, filepath.Join(root, string(sid), "generations", "gen_prestage")); got != preparedInode {
		t.Fatal("activation rewrote the candidate instead of installing the prepared directory")
	}
	if left := tempGenerationDirs(t, root, sid); len(left) != 0 {
		t.Fatalf("temporary directories after activation = %v, want none", left)
	}
	requireNoIntent(t, s, sid)
	state := readIndexStateForTest(t, s, sid)
	if state.IndexerVersion != 18 || state.IndexedAt == nil || *state.IndexedAt != 4242 {
		t.Fatalf("stamps = (%d,%v), want (18,4242)", state.IndexerVersion, state.IndexedAt)
	}
}

// TestStageGenerationAbandonedIsNeverRecovered is the refusal regression: a
// caller that prepares a candidate and then refuses it (for example because
// its input changed) drops the handle. Recovery must find nothing to replay,
// and the prior generation and its stamps stay authoritative.
func TestStageGenerationAbandonedIsNeverRecovered(t *testing.T) {
	sid, err := schema.NewSessionID("abababab-abab-4bab-8bab-abababababab")
	if err != nil {
		t.Fatal(err)
	}
	s, root := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	ctx := context.Background()
	prior := testActivation(t, sid, "gen_prior")
	if _, err := s.ActivateGeneration(ctx, prior); err != nil {
		t.Fatalf("prior activation: %v", err)
	}
	before := readIndexStateForTest(t, s, sid)

	candidate := testActivation(t, sid, "gen_refused")
	candidate.IndexerVersion = 19
	candidate.IndexedAtMs = 9999
	if _, err := s.StageGeneration(ctx, candidate); err != nil {
		t.Fatalf("StageGeneration: %v", err)
	}
	// The handle is dropped: the caller refused the candidate.

	outcome, err := s.RecoverGenerationActivation(ctx, sid)
	if err != nil {
		t.Fatalf("RecoverGenerationActivation: %v", err)
	}
	if outcome.Disposition != ingest.ActivationNotCommitted || outcome.CandidateID != "" {
		t.Fatalf("recovery outcome = %+v, want nothing recovered", outcome)
	}
	if active, err := s.activeGenerationID(ctx, sid); err != nil || active != "gen_prior" {
		t.Fatalf("active after recovery = %q (err=%v), want gen_prior", active, err)
	}
	requireNoIntent(t, s, sid)
	requireNotInstalled(t, s, sid, "gen_refused")
	after := readIndexStateForTest(t, s, sid)
	if after.IndexerVersion != before.IndexerVersion || after.IndexedAt == nil || before.IndexedAt == nil || *after.IndexedAt != *before.IndexedAt {
		t.Fatalf("stamps changed after recovery: before (%d,%v) after (%d,%v)", before.IndexerVersion, before.IndexedAt, after.IndexerVersion, after.IndexedAt)
	}

	// The next preparation for the session removes the abandoned files.
	next := testActivation(t, sid, "gen_next")
	if _, err := s.StageGeneration(ctx, next); err != nil {
		t.Fatalf("next StageGeneration: %v", err)
	}
	temps := tempGenerationDirs(t, root, sid)
	if len(temps) != 1 || !strings.Contains(filepath.Base(temps[0]), "gen_next") {
		t.Fatalf("temporary directories after the next preparation = %v, want only gen_next", temps)
	}
}

// TestStageGenerationLeavesPendingIntentForActivation proves preparation is
// file-only: a pending intent recorded for another candidate is neither
// replayed nor cleared, so the commit and its disposition stay with
// activation.
func TestStageGenerationLeavesPendingIntentForActivation(t *testing.T) {
	sid, err := schema.NewSessionID("acacacac-acac-4cac-8cac-acacacacacac")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	ctx := context.Background()
	pending := testActivation(t, sid, "gen_pending")
	digest, err := computeActivationBinding(pending.Generation.Generation, bindingFromBlobs(pending.Blobs))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.generationArtifacts.Stage(ctx, pending.Generation.Generation, pending.Blobs); err != nil {
		t.Fatalf("stage pending: %v", err)
	}
	if err := s.generationArtifacts.WriteIntent(ctx, GenerationIntent{
		SessionID: sid, GenerationID: "gen_pending",
		ManifestPath: "generations/gen_pending/manifest.json",
		Completeness: string(pending.Generation.Generation.Completeness), StagedAtMs: 1,
		ContentCapture: pending.ContentCapture, CandidateDigest: digest,
	}); err != nil {
		t.Fatalf("write pending intent: %v", err)
	}

	if _, err := s.StageGeneration(ctx, testActivation(t, sid, "gen_other")); err != nil {
		t.Fatalf("StageGeneration: %v", err)
	}
	intent, err := s.generationArtifacts.ReadIntent(ctx, sid)
	if err != nil || intent == nil || intent.GenerationID != "gen_pending" {
		t.Fatalf("pending intent after preparation = %+v (err=%v), want gen_pending untouched", intent, err)
	}
	if active, err := s.activeGenerationID(ctx, sid); err != nil || active != "" {
		t.Fatalf("active after preparation = %q (err=%v), want none", active, err)
	}
}

// TestStageGenerationMissingBlobLeavesNothing stages a multi-blob candidate
// with the production writer counts and one blob missing in the middle of the
// sorted order: the preparation reports the missing blob, cancels the other
// writers, and leaves no temporary directory and no intent.
func TestStageGenerationMissingBlobLeavesNothing(t *testing.T) {
	s, root := openGenerationStore(t)
	generation, blobs := benchGeneration(t, 5*defaultBlobWriteWorkers)
	generation.ID = "gen_missing_blob"
	sid := generation.Metadata.SessionID
	refs := make([]string, 0, len(generation.Content))
	for _, record := range generation.Content {
		refs = append(refs, string(record.Ref))
	}
	sort.Strings(refs)
	delete(blobs, schema.SourceEntryRef(refs[len(refs)/2]))

	_, err := s.StageGeneration(context.Background(), GenerationActivation{Generation: indexformat.V2{Generation: generation}, Blobs: blobs})
	if err == nil || !strings.Contains(err.Error(), "is missing; the generation is not self-contained") {
		t.Fatalf("StageGeneration err = %v, want the missing-blob refusal", err)
	}
	if left := tempGenerationDirs(t, root, sid); len(left) != 0 {
		t.Fatalf("temporary directories after the refusal = %v, want none", left)
	}
	requireNoIntent(t, s, sid)
	requireNotInstalled(t, s, sid, generation.ID)
}

// TestStageGenerationConcurrentSessions proves independent sessions can
// prepare in parallel under their own locks and still commit through the
// ordinary activation; the race detector is the second assertion.
func TestStageGenerationConcurrentSessions(t *testing.T) {
	ids := []string{
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	}
	s, root := openGenerationStore(t)
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
	prepared := make([]*PreparedGeneration, len(activations))
	for i := range activations {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			prepared[i], errs[i] = s.StageGeneration(context.Background(), activations[i])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent StageGeneration %d: %v", i, err)
		}
	}
	for i, activation := range activations {
		activation.Prepared = prepared[i]
		outcome, err := s.ActivateGeneration(context.Background(), activation)
		if err != nil {
			t.Fatalf("ActivateGeneration %d: %v", i, err)
		}
		if outcome.Disposition != ingest.ActivationCommittedNow {
			t.Fatalf("activation %d disposition = %v, want CommittedNow", i, outcome.Disposition)
		}
		sid := activation.Generation.Generation.Metadata.SessionID
		if left := tempGenerationDirs(t, root, sid); len(left) != 0 {
			t.Fatalf("session %d temporary directories after activation = %v, want none", i, left)
		}
	}
}
