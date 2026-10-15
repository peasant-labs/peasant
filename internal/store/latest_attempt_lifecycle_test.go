package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
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

// TestLatestAttemptSweepKeepsTheLatestAttempt proves the shared {active, latest attempt}
// reference: the ordinary sweep keeps the latest attempt's rows and objects.
func TestLatestAttemptSweepKeepsTheLatestAttempt(t *testing.T) {
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

// TestLatestAttemptCleanupInactiveGenerationRefusesTheLatestAttempt proves the latest-attempt
// pointer is protected from the inactive-generation cleanup.
func TestLatestAttemptCleanupInactiveGenerationRefusesTheLatestAttempt(t *testing.T) {
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

// seedReadyCapturedPublication seeds a session whose publication capture is
// current and bound to its index, so the session is ready to publish before any
// attempt is kept. It is the same seeding idiom the publication fixtures use.
func seedReadyCapturedPublication(t *testing.T, s *Store, sid schema.SessionID) int64 {
	t.Helper()
	ctx := context.Background()
	entry := makeStoreEntry(t, string(sid), strings.Repeat("a", 64), "github.com-example-project", defaults.HarnessClaudeCode, 1700000000000, 100, 200)
	entry.PublicationCapture = true
	entry.CWDProvenance = ingest.CWDSourceAbsent
	entry.Session.Origin = sessionorigin.User
	entry.Metadata.ContentHash = schema.ComputeTranscriptHash([]byte("captured transcript"))
	entry.Metadata.MetadataHash = schema.ComputeMetadataHash(entry.Metadata)
	revisions, err := s.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{entry})
	if err != nil {
		t.Fatalf("seed the ready publication row: %v", err)
	}
	revision := revisions[ingest.SessionID(sid)]
	result := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:          sid,
		Result:             indexformat.V1{},
		IndexVersion:       1,
		RequireFullContent: true,
		CaptureRevision:    revision,
		IndexerVersion:     ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
		IndexedAtMs:        1700000000001,
	}})
	if len(result) != 1 || result[0].Err != nil {
		t.Fatalf("seed the ready publication index: %+v", result)
	}
	return revision
}

// TestLatestAttemptCompleteGenerationDisposesTheAttempt proves the full
// lifecycle of a kept latest attempt beside a ready publication: the session is
// ready, keeping the attempt alone makes it not ready and names the pending
// attempt, a later complete generation clears the pointer in the same commit,
// the ordinary sweep disposes the attempt's rows, and readiness returns from
// the completed authority.
func TestLatestAttemptCompleteGenerationDisposesTheAttempt(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("c9c9c9c9-c9c9-49c9-89c9-c9c9c9c9c9c9")
	ctx := context.Background()
	revision := seedReadyCapturedPublication(t, s, sid)

	ready, err := s.LoadPublicationInput(ctx, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("read the seeded publication input: %v", err)
	}
	if ready.Readiness != ingest.PublicationReady {
		t.Fatalf("seeded readiness = %q, want ready before any attempt is kept", ready.Readiness)
	}

	setStoredArtifactHash(t, s, sid, hexIdentity('6'))
	outcome, err := s.ActivateGeneration(ctx, previewCandidate(t, sid, "gen_preview_b"))
	if err != nil {
		t.Fatalf("keep the latest attempt: %v", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
	}

	// The discriminating pair: the seed was ready, so a readiness that stops
	// being ready here is caused by the kept attempt and nothing else.
	kept, err := s.LoadPublicationInput(ctx, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("read the publication input with a kept attempt: %v", err)
	}
	if !kept.LatestAttemptPending {
		t.Fatal("the kept attempt was not named as pending")
	}
	if kept.Readiness == ingest.PublicationReady {
		t.Fatalf("readiness with a kept attempt = %q, want not ready", kept.Readiness)
	}

	// The later complete generation binds to the same publication capture, so
	// the completed authority can be ready again.
	next, nextBlobs := buildTestGeneration(t, sid, "gen_complete_c", "complete text", "complete input", "complete output")
	complete, err := s.ActivateGeneration(ctx, GenerationActivation{
		Generation: next, Blobs: nextBlobs, IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 5,
		ContentCapture: recoveryCapture(t, next), CaptureRevision: revision,
	})
	if err != nil {
		t.Fatalf("complete generation activation: %v", err)
	}
	if complete.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("complete disposition = %v, want ActivationCommittedNow", complete.Disposition)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want it cleared by the active move", pointer)
	}

	if _, err := s.SweepSession(ctx, sid); err != nil {
		t.Fatalf("sweep the superseded attempt: %v", err)
	}
	if got := generationRowCount(t, s, sid); got != 1 {
		t.Fatalf("generation rows after the sweep = %d, want the completed authority alone", got)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v after the sweep, want none", pointer)
	}
	after, err := s.LoadPublicationInput(ctx, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("read the publication input after the complete generation: %v", err)
	}
	if after.LatestAttemptPending {
		t.Fatal("the completed generation still reports a pending attempt")
	}
	if after.Readiness != ingest.PublicationReady {
		t.Fatalf("readiness after the complete generation = %q, want ready again", after.Readiness)
	}
}

// TestLatestAttemptPointerClearsOnRepairFinalization proves the third
// pointer-clear site on its own route: a repair finalization of the active
// authority rewrites its objects in place, clears the kept attempt's pointer
// through the shared bookkeeping, and writes no new generation row. The
// disposition is the repair return (CommittedNow); a skip would report
// ActivationSkipped, which is the routing discrimination.
func TestLatestAttemptPointerClearsOnRepairFinalization(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("d3d3d3d3-d3d3-43d3-83d3-d3d3d3d3d3d3")
	ctx := context.Background()
	seedFullAuthority(t, s, sid)
	identity := hexIdentity('2')
	setStoredArtifactHash(t, s, sid, identity)

	outcome, err := s.ActivateGeneration(ctx, previewCandidate(t, sid, "gen_preview_b"))
	if err != nil {
		t.Fatalf("keep the latest attempt: %v", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.generationID != "gen_preview_b" {
		t.Fatalf("latest-attempt generation = %q, want gen_preview_b", pointer.generationID)
	}

	// The production marking: a damaged body fails verification and the repair
	// pass clears the consumed input proof, which is the repair predicate.
	damageBodyPreview(t, s, sid, 0)
	report, err := s.VerifyContent(ctx, true)
	if err != nil {
		t.Fatalf("verify content: %v", err)
	}
	if len(report.Damaged) != 1 || len(report.Repaired) != 1 {
		t.Fatalf("verify report damaged=%d repaired=%d, want one damaged and repaired session", len(report.Damaged), len(report.Repaired))
	}

	repair, repairBlobs := buildTestGeneration(t, sid, "gen_full_prior_refresh", "full text", "full input", "full output")
	proof := strings.Repeat("d", 64)
	repairOutcome := activateRepairCandidate(t, s, sid, repair, repairBlobs, identity, proof)
	if repairOutcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("repair disposition = %v, want ActivationCommittedNow", repairOutcome.Disposition)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want it cleared by the repair finalization", pointer)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_full_prior" {
		t.Fatalf("active generation = %q, want the unchanged authority", got)
	}
	if got := generationRowCount(t, s, sid); got != 2 {
		t.Fatalf("generation rows = %d, want the authority and the attempt; the repair writes no new row", got)
	}
}

// TestLatestAttemptFileBackedAuthorityRetainsOnBothRoutes proves a file-backed
// read authority is a valid coexistence partner on BOTH activation routes: the
// newer incomplete attempt is kept, the authority stays the file-backed
// generation, the mounted read serves the attempt's bounded preview, the
// explicit last-good read serves the file-backed authority, and the default
// full content read refuses.
func TestLatestAttemptFileBackedAuthorityRetainsOnBothRoutes(t *testing.T) {
	for _, route := range []string{"single", "batch"} {
		t.Run(route, func(t *testing.T) {
			s, root := openGenerationStore(t)
			sid := schema.SessionID("e4e4e4e4-e4e4-44e4-84e4-e4e4e4e4e4e4")
			ctx := context.Background()
			seedGenerationSession(t, s, string(sid))
			authority, authorityBlobs := buildTestGeneration(t, sid, "gen-file-authority", "file authority text", "file authority input", "file authority output")
			stampSeedMetadataHash(&authority)
			seedFileBackedGeneration(t, s, root, sid, authority, authorityBlobs, true)
			identity := hexIdentity('c')
			setStoredArtifactHash(t, s, sid, identity)

			candidate := previewCandidate(t, sid, "gen_preview_b")
			var disposition ingest.ActivationDisposition
			switch route {
			case "single":
				outcome, err := s.ActivateGeneration(ctx, candidate)
				if err != nil {
					t.Fatalf("single-route activation: %v", err)
				}
				disposition = outcome.Disposition
			case "batch":
				results := s.ActivateGenerationBatch(ctx, []GenerationActivation{candidate})
				if len(results) != 1 {
					t.Fatalf("batch results = %d, want 1", len(results))
				}
				if results[0].Err != nil {
					t.Fatalf("batch activation: %v", results[0].Err)
				}
				disposition = results[0].Outcome.Disposition
			}
			if disposition != ingest.ActivationLatestAttemptPreview {
				t.Fatalf("disposition = %v, want ActivationLatestAttemptPreview", disposition)
			}
			if pointer := latestAttemptPointerFor(t, s, sid); pointer.generationID != "gen_preview_b" || pointer.artifactHash != identity {
				t.Fatalf("latest-attempt pointer = %+v, want the attempt and the verified identity", pointer)
			}

			available, err := s.ReadSessionAvailable(ctx, ingest.SessionID(sid))
			if err != nil {
				t.Fatalf("ReadSessionAvailable: %v", err)
			}
			if !PartialPreviewNeeded(available.Capture) {
				t.Fatalf("available capture = %+v, want the attempt's partial preview state", available.Capture)
			}
			if available.Capture.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly || available.Capture.Status != ingest.ContentCaptureIncomplete {
				t.Fatalf("available capture = %s/%s, want preview_only/incomplete", available.Capture.CaptureFormat, available.Capture.Status)
			}
			if len(available.Entries) == 0 {
				t.Fatal("the available read served no attempt entries")
			}

			served := ""
			if err := s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(snapshot indexformat.ReadSnapshot) error {
				served = snapshot.GenerationID
				return nil
			}); err != nil {
				t.Fatalf("last-good read of the file-backed authority: %v", err)
			}
			if served != "gen-file-authority" {
				t.Fatalf("last-good generation = %q, want the file-backed authority", served)
			}
			if _, err := s.ReadSessionContent(ctx, string(sid)); !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
				t.Fatalf("default full content read = %v, want the latest-attempt sentinel", err)
			}
			if got := visibleGeneration(t, s, sid); got != "gen-file-authority" {
				t.Fatalf("active generation = %q, want the unchanged file-backed authority", got)
			}
		})
	}
}

// TestLatestAttemptRetentionRefusesMalformedEvidence proves a candidate whose
// selected evidence does not validate is refused on BOTH activation routes, with
// the authority intact and no attempt recorded: the latest-attempt path re-runs the
// same unknown-evidence preflight the ordinary content write runs, so it is
// never a bypass for a candidate the ordinary path would refuse.
func TestLatestAttemptRetentionRefusesMalformedEvidence(t *testing.T) {
	for _, route := range []string{"single", "batch"} {
		t.Run(route, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			sid := schema.SessionID("f6f6f6f6-f6f6-46f6-86f6-f6f6f6f6f6f6")
			ctx := context.Background()
			seedFullAuthority(t, s, sid)
			setStoredArtifactHash(t, s, sid, hexIdentity('d'))

			candidate := previewCandidate(t, sid, "gen_preview_b")
			corrupt := `{"retainedUnknown":{"harness":"claude-code","namespace":42}}`
			candidate.Generation.Generation.Main.Entries[0].Extra = &corrupt

			var routeErr error
			switch route {
			case "single":
				_, routeErr = s.ActivateGeneration(ctx, candidate)
			case "batch":
				results := s.ActivateGenerationBatch(ctx, []GenerationActivation{candidate})
				if len(results) != 1 {
					t.Fatalf("batch results = %d, want 1", len(results))
				}
				routeErr = results[0].Err
			}
			if routeErr == nil {
				t.Fatal("malformed evidence was retained; the store must refuse it")
			}
			for _, want := range []string{"store content evidence validation", "prior capture remains authoritative"} {
				if !strings.Contains(routeErr.Error(), want) {
					t.Fatalf("refusal = %v, want it to contain %q", routeErr, want)
				}
			}
			if got := visibleGeneration(t, s, sid); got != "gen_full_prior" {
				t.Fatalf("active generation = %q, want the untouched authority", got)
			}
			if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
				t.Fatalf("latest-attempt pointer = %+v, want none after the refusal", pointer)
			}
			if got := generationRowCount(t, s, sid); got != 1 {
				t.Fatalf("generation rows = %d, want the authority alone", got)
			}
		})
	}
}
