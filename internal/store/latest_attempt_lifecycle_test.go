package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// latestAttemptPreviewForTest seeds a full authority with a stored pair identity and
// retains one preview attempt, returning the latest-attempt generation identifier.
func latestAttemptPreviewForTest(t *testing.T, s *Store, sid schema.SessionID, identity string) {
	t.Helper()
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, identity)
	outcome, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b"))
	if err != nil {
		t.Fatalf("latest-attempt write: %v", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("latest-attempt disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
	}
}

// TestLatestAttemptPointerClearsOnActiveMove proves the first pointer-clear site:
// installing a new active authority clears the latest-attempt pointer in the same
// transaction.
func TestLatestAttemptPointerClearsOnActiveMove(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("b0b0b0b0-b0b0-40b0-80b0-b0b0b0b0b0b0")
	latestAttemptPreviewForTest(t, s, sid, hexIdentity('1'))

	next, nextBlobs := buildTestGeneration(t, sid, "gen_complete_c", "complete text", "complete input", "complete output")
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: next, Blobs: nextBlobs, IndexerVersion: 1, IndexedAtMs: 5,
		ContentCapture: recoveryCapture(t, next),
	})
	if err != nil {
		t.Fatalf("complete activation: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("disposition = %v, want ActivationCommittedNow", outcome.Disposition)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want it cleared by the active move", pointer)
	}
	if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != "gen_complete_c" {
		t.Fatalf("active = %q (err=%v), want gen_complete_c", active, err)
	}
}

// TestLatestAttemptPointerClearsOnSkipFinalization proves the third pointer-clear
// site: an in-place skip finalization of the active authority clears the
// latest-attempt pointer through the shared bookkeeping.
func TestLatestAttemptPointerClearsOnSkipFinalization(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("c1c1c1c1-c1c1-41c1-81c1-c1c1c1c1c1c1")
	latestAttemptPreviewForTest(t, s, sid, hexIdentity('2'))

	// A fresh identifier carrying the active authority's exact projection:
	// the skip comparison finds it unchanged and finalizes the active
	// authority in place without moving the pointer.
	refresh, refreshBlobs := buildTestGeneration(t, sid, "gen_full_prior_refresh", "full text", "full input", "full output")
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: refresh, Blobs: refreshBlobs, IndexerVersion: 1, IndexedAtMs: 6,
		ContentCapture: recoveryCapture(t, refresh),
	})
	if err != nil {
		t.Fatalf("skip refresh: %v", err)
	}
	if outcome.Disposition != ingest.ActivationSkipped {
		t.Fatalf("disposition = %v, want ActivationSkipped", outcome.Disposition)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want it cleared by the skip finalization", pointer)
	}
	if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the unchanged full authority", active, err)
	}
}

// TestLatestAttemptPointerClearsOnRepresentationReplacement proves the second
// pointer-clear site: a representation replacement clears the latest-attempt pointer
// alongside the active pointer.
func TestLatestAttemptPointerClearsOnRepresentationReplacement(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("d2d2d2d2-d2d2-42d2-82d2-d2d2d2d2d2d2")
	latestAttemptPreviewForTest(t, s, sid, hexIdentity('3'))

	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	deleteErr := generationIndexFormat{}.Delete(context.Background(), conn, sid)
	s.pool.Put(conn)
	if deleteErr != nil {
		t.Fatalf("representation replacement: %v", deleteErr)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want it cleared by the representation replacement", pointer)
	}
	if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != "" {
		t.Fatalf("active = %q (err=%v), want none after the representation replacement", active, err)
	}
}

// TestLatestAttemptSweepKeepsIt proves the shared {active, latest attempt}
// reference: the ordinary sweep keeps the latest attempt's rows and objects.
func TestLatestAttemptSweepKeepsIt(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("e3e3e3e3-e3e3-43e3-83e3-e3e3e3e3e3e3")
	latestAttemptPreviewForTest(t, s, sid, hexIdentity('4'))

	if _, err := s.SweepSession(context.Background(), sid); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.generationID != "gen_preview_b" {
		t.Fatalf("latest-attempt generation = %q, want the sweep to keep gen_preview_b", pointer.generationID)
	}
	if rows := generationRowCount(t, s, sid); rows != 2 {
		t.Fatalf("generation rows = %d, want 2 (the authority and the latest attempt)", rows)
	}
	if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the full authority", active, err)
	}
}

// TestLatestAttemptCleanupInactiveGenerationRefusesIt proves the latest-attempt
// pointer is protected from the inactive-generation cleanup.
func TestLatestAttemptCleanupInactiveGenerationRefusesIt(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("f4f4f4f4-f4f4-44f4-84f4-f4f4f4f4f4f4")
	latestAttemptPreviewForTest(t, s, sid, hexIdentity('5'))

	err := s.CleanupInactiveGeneration(context.Background(), sid, "gen_preview_b")
	if err == nil || !strings.Contains(err.Error(), "latest attempt") {
		t.Fatalf("cleanup error = %v, want a refusal naming the latest attempt", err)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.generationID != "gen_preview_b" {
		t.Fatalf("latest-attempt generation = %q, want it left in place", pointer.generationID)
	}
}

// TestLatestAttemptSavepointFailureRollsBack proves the latest-attempt transaction is
// atomic: a failure at the mid-activation seam rolls the savepoint back and
// leaves the pointer and the active authority unchanged.
func TestLatestAttemptSavepointFailureRollsBack(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("a5a5a5a5-a5a5-45a5-85a5-a5a5a5a5a5a5")
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, hexIdentity('6'))
	rowsBefore := generationRowCount(t, s, sid)
	captureBefore, foundBefore, err := s.GetSessionContentCapture(context.Background(), ingest.SessionID(sid))
	if err != nil || !foundBefore {
		t.Fatalf("read capture before: %v (found=%v)", err, foundBefore)
	}

	installHarmonizedFault(t, harmonizedSeamMidActivationBatch)
	outcome, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b"))
	if err == nil {
		t.Fatal("latest attempt must fail at the injected seam")
	}
	if outcome.Disposition != ingest.ActivationNotCommitted {
		t.Fatalf("disposition = %v, want NotCommitted", outcome.Disposition)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want it rolled back", pointer)
	}
	if rows := generationRowCount(t, s, sid); rows != rowsBefore {
		t.Fatalf("generation rows = %d, want %d after the rollback", rows, rowsBefore)
	}
	if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the unchanged full authority", active, err)
	}
	captureAfter, foundAfter, err := s.GetSessionContentCapture(context.Background(), ingest.SessionID(sid))
	if err != nil || !foundAfter {
		t.Fatalf("read capture after: %v (found=%v)", err, foundAfter)
	}
	if captureAfter != captureBefore {
		t.Fatalf("active capture changed across the rollback: %+v -> %+v", captureBefore, captureAfter)
	}
}

// TestLatestAttemptSkipsRepairForCorruptAuthority proves a latest-attempt-eligible
// candidate never enters repair even when the active authority matches the
// repair predicate, so A's objects are never rewritten from preview bytes.
func TestLatestAttemptSkipsRepairForCorruptAuthority(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("b6b6b6b6-b6b6-46b6-86b6-b6b6b6b6b6b6")
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, hexIdentity('c'))

	// Drive the repair predicate: an artifact identity with a cleared input
	// proof and a stored object that fails self-verification. The authority
	// still reads; only the mapped body is damaged.
	damageBodyPreview(t, s, sid, 0)
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	repair, repairErr := repairNeedsHarmonized(conn, sid)
	s.pool.Put(conn)
	if repairErr != nil {
		t.Fatalf("repair predicate: %v", repairErr)
	}
	if !repair {
		t.Fatal("the repair predicate must be active for this case")
	}

	outcome, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b"))
	if err != nil {
		t.Fatalf("latest attempt over a repair-predicate authority: %v", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("disposition = %v, want ActivationLatestAttemptPreview (never repair)", outcome.Disposition)
	}
	if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the untouched full authority", active, err)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.generationID != "gen_preview_b" {
		t.Fatalf("latest-attempt generation = %q, want gen_preview_b", pointer.generationID)
	}
}

// TestLatestAttemptInvalidCandidateRefusesOnBothRoutes proves an identifier reuse
// with a different candidate binding is refused on the single route and on the
// batch route, leaving the installed generation unchanged.
func TestLatestAttemptInvalidCandidateRefusesOnBothRoutes(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("c7c7c7c7-c7c7-47c7-87c7-c7c7c7c7c7c7")
	latestAttemptPreviewForTest(t, s, sid, hexIdentity('d'))

	// The same identifier as the latest attempt, carrying different content:
	// the immutable-identity check must refuse it.
	different, differentBlobs := buildTestGeneration(t, sid, "gen_preview_b", "different text", "different input", "different output")
	candidate := GenerationActivation{
		Generation: different, Blobs: differentBlobs, IndexerVersion: 18, IndexedAtMs: 8,
		ContentCapture: sourceOmittedPreviewWrite(),
	}
	outcome, err := s.ActivateGeneration(context.Background(), candidate)
	if err == nil || !strings.Contains(err.Error(), "different candidate binding") {
		t.Fatalf("inline refusal = %v, want the immutable-identity refusal", err)
	}
	if outcome.Disposition != ingest.ActivationNotCommitted {
		t.Fatalf("inline disposition = %v, want NotCommitted", outcome.Disposition)
	}

	results := s.ActivateGenerationBatch(context.Background(), []GenerationActivation{candidate})
	if len(results) != 1 || results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "different candidate binding") {
		t.Fatalf("batch refusal = %v, want the immutable-identity refusal", results)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.generationID != "gen_preview_b" {
		t.Fatalf("latest-attempt generation = %q, want the unchanged gen_preview_b", pointer.generationID)
	}
}
