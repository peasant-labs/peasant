package store

import (
	"context"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

func testActivation(t *testing.T, sid schema.SessionID, genID string) GenerationActivation {
	t.Helper()
	generation, blobs := buildTestGeneration(t, sid, genID, "prestage text", "prestage input", "prestage output")
	return GenerationActivation{
		Generation:     filledCandidateForValidation(t, generation, blobs),
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

// TestStageGenerationPreparesThenActivationInstalls proves the split: the
// preparation stages the candidate's objects idempotently with no lock and
// no generation row, and the activation commits those same objects (no
// generation row exists before it, the active pointer is unset) with the
// caller stamps.
func TestStageGenerationPreparesThenActivationInstalls(t *testing.T) {
	sid, err := schema.NewSessionID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	activation := testActivation(t, sid, "gen_prestage")
	ctx := context.Background()

	prepared, err := s.StageGeneration(ctx, activation)
	if err != nil {
		t.Fatalf("StageGeneration: %v", err)
	}
	if rowPresent(t, s, sid, "gen_prestage") {
		t.Fatal("preparation wrote a generation row; staging installs no generation")
	}
	if active, err := s.activeGenerationID(ctx, sid); err != nil || active != "" {
		t.Fatalf("active after preparation = %q (err=%v), want none", active, err)
	}
	assertStagedObjects(t, s, sid, activation)

	activation.Prepared = prepared
	outcome, err := s.ActivateGeneration(ctx, activation)
	if err != nil {
		t.Fatalf("ActivateGeneration with prepared objects: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("disposition = %v, want CommittedNow", outcome.Disposition)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_prestage" {
		t.Fatalf("active after activation = %q, want gen_prestage", got)
	}
	state := readIndexStateForTest(t, s, sid)
	if state.IndexerVersion != 18 || state.IndexedAt == nil || *state.IndexedAt != 4242 {
		t.Fatalf("stamps = (%d,%v), want (18,4242)", state.IndexerVersion, state.IndexedAt)
	}
}

// TestStageGenerationAbandonedIsNeverRecovered is the refusal regression: a
// caller that prepares a candidate and then refuses it (for example because
// its input changed) drops the handle. No generation row records it, so no
// recovery can activate it; the prior generation and its stamps stay
// authoritative, and a later activation for the session proceeds normally.
func TestStageGenerationAbandonedIsNeverRecovered(t *testing.T) {
	sid, err := schema.NewSessionID("abababab-abab-4bab-8bab-abababababab")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
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

	if active, err := s.activeGenerationID(ctx, sid); err != nil || active != "gen_prior" {
		t.Fatalf("active after abandoned preparation = %q (err=%v), want gen_prior", active, err)
	}
	if rowPresent(t, s, sid, "gen_refused") {
		t.Fatal("abandoned preparation wrote a generation row; nothing must be installed")
	}
	after := readIndexStateForTest(t, s, sid)
	if after.IndexerVersion != before.IndexerVersion || after.IndexedAt == nil || before.IndexedAt == nil || *after.IndexedAt != *before.IndexedAt {
		t.Fatalf("stamps changed after abandoned preparation: before (%d,%v) after (%d,%v)", before.IndexerVersion, before.IndexedAt, after.IndexerVersion, after.IndexedAt)
	}

	// A later activation for the session reuses the staged objects and
	// commits normally.
	nextGen, nextBlobs := buildTestGeneration(t, sid, "gen_next", "next text", "next input", "next output")
	if _, err := s.ActivateGeneration(ctx, GenerationActivation{
		Generation:     filledCandidateForValidation(t, nextGen, nextBlobs),
		Blobs:          nextBlobs,
		IndexerVersion: 20,
		IndexedAtMs:    10000,
	}); err != nil {
		t.Fatalf("next activation: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_next" {
		t.Fatalf("after next activation visible = %q, want gen_next", got)
	}
}

// TestStageGenerationMissingBlobLeavesNothing stages a candidate with one
// non-emitted content record missing its bytes: the preparation reports
// the missing bytes and stages nothing committable — no generation row and
// no sweep flag, because S0 refuses before any write.
func TestStageGenerationMissingBlobLeavesNothing(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid, err := schema.NewSessionID("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, string(sid))
	generation, blobs := buildTestGeneration(t, sid, "gen_missing_blob", "text", "input", "output")
	retained := schema.SourceEntryRef("e_retained")
	generation.Generation.Content = append(generation.Generation.Content, indexformat.ContentRecord{Ref: retained})
	blobs[retained] = []byte("retained bytes")
	generation = filledCandidateForValidation(t, generation, blobs)
	delete(blobs, retained)

	_, err = s.StageGeneration(context.Background(), GenerationActivation{Generation: generation, Blobs: blobs})
	if err == nil {
		t.Fatal("StageGeneration with missing blob bytes succeeded; it must be refused")
	}
	if rowPresent(t, s, sid, "gen_missing_blob") {
		t.Fatal("refused preparation wrote a generation row; nothing must be installed")
	}
	if flag := readSweepFlag(t, s, sid); flag {
		t.Fatal("refused preparation set the sweep flag; S0 refuses before any write")
	}
}

// TestStageGenerationConcurrentSessions proves independent sessions can
// prepare in parallel without locks and still commit through the ordinary
// activation; the race detector is the second assertion.
func TestStageGenerationConcurrentSessions(t *testing.T) {
	ids := []string{
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	}
	s, _ := openGenerationStore(t)
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
	}
}

// assertStagedObjects proves the preparation staged the candidate's bodies:
// every entry maps to a stored body by digest. Non-emitted blobs are
// asserted by the callers that supply them.
func assertStagedObjects(t *testing.T, s *Store, sid schema.SessionID, activation GenerationActivation) {
	t.Helper()
	prepared, err := prepareHarmonizedCandidate(sid, activation.Generation.Generation, activation.Blobs)
	if err != nil {
		t.Fatalf("prepare expected digests: %v", err)
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	for _, digest := range prepared.bodyDigests {
		found := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_entry_bodies WHERE session_id = ? AND body_digest = ? LIMIT 1`, &sqlitex.ExecOptions{
			Args:       []any{string(sid), string(digest)},
			ResultFunc: func(*sqlite.Stmt) error { found = true; return nil },
		}); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("staged body %q missing after preparation", digest)
		}
	}
}

// readSweepFlag reads the session's crash flag.
func readSweepFlag(t *testing.T, s *Store, sid schema.SessionID) bool {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	flag := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			flag = stmt.ColumnInt64(0) == 1
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return flag
}
