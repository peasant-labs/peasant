package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// This file proves the store-side read contract of latest-attempt
// coexistence: which generation each read answers for, which counts it
// reports, and which selection surfaces treat the latest attempt as settled.
// The same seams have other homes, and this file is not a second copy of them:
// the mounted detail payload (BuildSnapshotPreviewBytes, with the attempt's own
// counts) is proven in internal/api/snapshot_detail_test.go; the mounted
// kickstart pane and the share-wizard source read in cmd/peasant; the harvest's
// literal summary and durable-log observables in internal/ingest. A store row
// read here is not mounted or end-to-end proof.

// authorityStats and latestAttemptStats are the two DISTINCT literal producer-stat
// documents the counts-scope assertions use, so a read that reports the wrong
// generation's counts cannot pass by coincidence. Only the count fields are
// replaced: a complete generation must keep its input-submission count.
var (
	authorityStats     = countsFor(11, 22, 33, 66000)
	latestAttemptStats = countsFor(44, 55, 66, 132000)
)

// countsFor builds a producer-stat document with the supplied counts.
func countsFor(turnCount, toolCallCount, tokensOut int, durationMs int64) schema.SessionStats {
	return schema.SessionStats{
		TurnCount:     turnCount,
		ToolCallCount: toolCallCount,
		TokensOut:     tokensOut,
		DurationMs:    durationMs,
	}
}

// applyCounts copies the count fields onto a built candidate's own stats,
// keeping the fields a complete generation must carry.
func applyCounts(base, counts schema.SessionStats) schema.SessionStats {
	base.TurnCount = counts.TurnCount
	base.ToolCallCount = counts.ToolCallCount
	base.TokensOut = counts.TokensOut
	base.DurationMs = counts.DurationMs
	return base
}

// seedFullAuthorityWithStats seeds the session and commits a complete full
// generation as the read authority with the supplied producer stats.
func seedFullAuthorityWithStats(t *testing.T, s *Store, sid schema.SessionID, stats schema.SessionStats) {
	t.Helper()
	seedGenerationSession(t, s, string(sid))
	prior, blobs := buildTestGeneration(t, sid, "gen_full_prior", "full text", "full input", "full output")
	prior.Generation.Metadata.Stats = applyCounts(prior.Generation.Metadata.Stats, stats)
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: prior, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1,
		ContentCapture: recoveryCapture(t, prior),
	}); err != nil {
		t.Fatalf("full prior activation: %v", err)
	}
}

// latestAttemptPreviewWithStats retains one preview attempt over the seeded authority,
// carrying the supplied producer stats and adapter revision.
func latestAttemptPreviewWithStats(t *testing.T, s *Store, sid schema.SessionID, genID string, stats schema.SessionStats, adapterVersion int, identity string) {
	t.Helper()
	setStoredArtifactHash(t, s, sid, identity)
	preview, blobs := buildTestGeneration(t, sid, genID, "preview text", "preview input", "preview output")
	preview.Generation.Metadata.Stats = applyCounts(preview.Generation.Metadata.Stats, stats)
	preview.Generation.Metadata.AdapterVersion = &adapterVersion
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: preview, Blobs: blobs, IndexerVersion: 18, IndexedAtMs: 2,
		ContentCapture: sourceOmittedPreviewWrite(),
	})
	if err != nil {
		t.Fatalf("latest-attempt write: %v", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("latest-attempt disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
	}
}

// seedLatestAttemptCoexistence seeds the canonical coexistence state: a managed full
// authority with distinct counts, plus one latest-attempt preview attempt with its
// own distinct counts and adapter revision.
func seedLatestAttemptCoexistence(t *testing.T, s *Store, sid schema.SessionID) string {
	t.Helper()
	seedFullAuthorityWithStats(t, s, sid, authorityStats)
	identity := hexIdentity('7')
	latestAttemptPreviewWithStats(t, s, sid, "gen_preview_b", latestAttemptStats, 2, identity)
	return identity
}

// TestLatestAttemptRefusesFullReadsUniformly proves the uniform refusal: an
// ordinary full snapshot and the full content read both return the latest-attempt
// sentinel before any representation is resolved, and an explicit last-good
// selection serves the active authority instead.
func TestLatestAttemptRefusesFullReadsUniformly(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("11112222-3333-4444-8555-666677778888")
	seedLatestAttemptCoexistence(t, s, sid)
	ctx := context.Background()

	callbackRan := false
	err := s.WithSessionSnapshot(ctx, sid, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("WithSessionSnapshot error = %v, want the latest-attempt sentinel", err)
	}
	if callbackRan {
		t.Fatal("WithSessionSnapshot invoked the callback for a session with a latest attempt")
	}
	if _, err := s.ReadSessionContent(ctx, string(sid)); !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("ReadSessionContent error = %v, want the latest-attempt sentinel", err)
	}

	// The explicit last-good selection serves the active authority: its
	// generation and its own counts, never the latest attempt's.
	served := ""
	if err := s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(snapshot indexformat.ReadSnapshot) error {
		served = snapshot.GenerationID
		return nil
	}); err != nil {
		t.Fatalf("WithSessionSnapshotSelected(LastGood): %v", err)
	}
	if served != "gen_full_prior" {
		t.Fatalf("last-good snapshot generation = %q, want the active authority", served)
	}
	snapshot, err := s.ReadSessionContentSelected(ctx, string(sid), ingest.SessionReadSelection{LastGood: true})
	if err != nil {
		t.Fatalf("ReadSessionContentSelected(LastGood): %v", err)
	}
	if snapshot.Detail.TurnCount != authorityStats.TurnCount || snapshot.Detail.ToolCalls != authorityStats.ToolCallCount {
		t.Fatalf("last-good counts = %d/%d, want the authority's %d/%d", snapshot.Detail.TurnCount, snapshot.Detail.ToolCalls, authorityStats.TurnCount, authorityStats.ToolCallCount)
	}
	if want := float64(authorityStats.DurationMs) / 60000.0; snapshot.Detail.DurationMinutes != want {
		t.Fatalf("last-good duration minutes = %v, want the authority's %v", snapshot.Detail.DurationMinutes, want)
	}
}

// TestLatestAttemptRefusalIsUniformForCertificateAuthority proves the refusal
// does not depend on the authority's representation: with the active pointer
// cleared — the certificate or file-backed authority's state — the ordinary
// read still refuses, and last-good serves the preserved legacy read.
func TestLatestAttemptRefusalIsUniformForCertificateAuthority(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("22223333-4444-4555-8666-777788889999")
	seedLatestAttemptCoexistence(t, s, sid)
	ctx := context.Background()
	// Clear the active pointer: the session keeps its certificate and its
	// latest attempt, exactly the state a V1-certificate or file-backed
	// authority has while a managed preview is kept beside it.
	execGenerationSQL(t, s, `UPDATE sessions SET active_generation_id = NULL WHERE session_id = '`+string(sid)+`';`)

	err := s.WithSessionSnapshot(ctx, sid, func(indexformat.ReadSnapshot) error {
		t.Fatal("the callback must not run for a session with a latest attempt")
		return nil
	})
	if !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("WithSessionSnapshot error = %v, want the latest-attempt sentinel", err)
	}
	indexVersion := 0
	if err := s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(snapshot indexformat.ReadSnapshot) error {
		indexVersion = snapshot.IndexVersion
		return nil
	}); err != nil {
		t.Fatalf("last-good read without an active generation: %v", err)
	}
	if indexVersion != 1 {
		t.Fatalf("last-good snapshot index version = %d, want the preserved legacy read", indexVersion)
	}
}

// TestLatestAttemptLastGoodRefusesActivePreview proves the explicit last-good
// selection refuses an active authority that is only a preview, in both shapes:
// with the latest-attempt pointer set and the certificate flipped to a preview, and in
// the explicit-rebuild shape where the pointer is cleared and the preview is the
// active authority.
func TestLatestAttemptLastGoodRefusesActivePreview(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("aaaabbbb-cccc-4ddd-8eee-ffff00001111")
	seedLatestAttemptCoexistence(t, s, sid)
	ctx := context.Background()

	// Shape 1: the latest-attempt pointer stays set; only the certificate becomes an
	// incomplete preview, so no full generation remains to serve as last-good.
	execGenerationSQL(t, s, `UPDATE session_content_captures SET status='incomplete', capture_format='preview_only', failure_code='source_records_omitted' WHERE session_id = '`+string(sid)+`';`)

	callbackRan := false
	err := s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("last-good snapshot error = %v, want the latest-attempt sentinel", err)
	}
	if callbackRan {
		t.Fatal("last-good served a snapshot for an active preview")
	}
	if _, err := s.ReadSessionContentSelected(ctx, string(sid), ingest.SessionReadSelection{LastGood: true}); !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("last-good content error = %v, want the latest-attempt sentinel", err)
	}

	// Shape 2: an explicit rebuild cleared the latest-attempt pointer and installed
	// the preview as the active authority. The snapshot lane still refuses; the
	// content lane refuses through its own capture authority, so the two lanes
	// are independently guarded.
	execGenerationSQL(t, s, `UPDATE sessions SET active_generation_id = 'gen_preview_b', latest_attempt_generation_id = NULL, latest_attempt_artifact_hash = NULL, latest_attempt_indexer_version = NULL, latest_attempt_failure_code = NULL, latest_attempt_failure_message = NULL, latest_attempt_captured_at_ms = NULL, latest_attempt_stats_json = NULL WHERE session_id = '`+string(sid)+`';`)

	callbackRan = false
	err = s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("state-9 last-good snapshot error = %v, want the latest-attempt sentinel", err)
	}
	if callbackRan {
		t.Fatal("state-9 last-good served a snapshot for an active preview")
	}
	if _, err := s.ReadSessionContentSelected(ctx, string(sid), ingest.SessionReadSelection{LastGood: true}); !errors.Is(err, ErrContentCaptureIncomplete) {
		t.Fatalf("state-9 last-good content error = %v, want the incomplete-capture refusal", err)
	}
}

// TestLatestAttemptAvailableRefusesMissingGenerationRow proves the available
// read refuses when the latest-attempt pointer names a generation row that is gone,
// instead of serving a partial-flagged transcript with nothing in it, while the
// other reads of the same session keep answering.
func TestLatestAttemptAvailableRefusesMissingGenerationRow(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("bbbbcccc-dddd-4eee-8fff-000011112222")
	seedLatestAttemptCoexistence(t, s, sid)
	ctx := context.Background()
	execGenerationSQL(t, s, `DELETE FROM session_generations WHERE session_id = '`+string(sid)+`' AND generation_id = 'gen_preview_b';`)

	if _, err := s.ReadSessionAvailable(ctx, ingest.SessionID(sid)); err == nil {
		t.Fatal("ReadSessionAvailable served a latest-attempt pointer whose generation row is gone")
	} else if !strings.Contains(err.Error(), string(sid)) || !strings.Contains(err.Error(), "gen_preview_b") {
		t.Fatalf("available refusal = %v, want it to name the session and the latest-attempt generation", err)
	}
	if err := s.WithSessionSnapshot(ctx, sid, func(indexformat.ReadSnapshot) error {
		t.Fatal("the callback must not run for a session with a latest attempt")
		return nil
	}); !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("default snapshot error = %v, want the latest-attempt sentinel", err)
	}
	served := ""
	if err := s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(snapshot indexformat.ReadSnapshot) error {
		served = snapshot.GenerationID
		return nil
	}); err != nil {
		t.Fatalf("last-good read after the latest-attempt row was removed: %v", err)
	}
	if served != "gen_full_prior" {
		t.Fatalf("last-good generation = %q, want the full authority", served)
	}
}

// TestLatestAttemptAvailableReadBindsLatestAttempt proves the available
// read serves the latest attempt: its generation-scoped entries, its
// synthesized capture with the partial state, and its own counts.
func TestLatestAttemptAvailableReadBindsLatestAttempt(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("33334444-5555-4666-8777-888899990000")
	seedLatestAttemptCoexistence(t, s, sid)

	snapshot, err := s.ReadSessionAvailable(context.Background(), ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("ReadSessionAvailable: %v", err)
	}
	if !PartialPreviewNeeded(snapshot.Capture) {
		t.Fatalf("available capture = %s/%s, want the latest-attempt partial state", snapshot.Capture.Status, snapshot.Capture.CaptureFormat)
	}
	if snapshot.Capture.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly || snapshot.Capture.Status != ingest.ContentCaptureIncomplete {
		t.Fatalf("available capture = %s/%s, want preview_only/incomplete", snapshot.Capture.CaptureFormat, snapshot.Capture.Status)
	}
	if snapshot.Capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		t.Fatalf("available capture failure code = %q, want source_records_omitted", snapshot.Capture.FailureCode)
	}
	if len(snapshot.Entries) != 3 {
		t.Fatalf("available entries = %d, want the latest attempt's 3", len(snapshot.Entries))
	}
	if first := snapshot.Entries[0]; first.ContentPreview == nil || !strings.Contains(*first.ContentPreview, "preview text") {
		t.Fatalf("available entry 0 = %+v, want the latest attempt's own content", first.ContentPreview)
	}
	if snapshot.Detail.TurnCount != latestAttemptStats.TurnCount || snapshot.Detail.ToolCalls != latestAttemptStats.ToolCallCount || snapshot.Detail.OutputTokens != latestAttemptStats.TokensOut {
		t.Fatalf("available counts = %d/%d/%d, want the latest attempt's %d/%d/%d",
			snapshot.Detail.TurnCount, snapshot.Detail.ToolCalls, snapshot.Detail.OutputTokens,
			latestAttemptStats.TurnCount, latestAttemptStats.ToolCallCount, latestAttemptStats.TokensOut)
	}
	// The latest-attempt producer document records milliseconds while this detail
	// column carries minutes, so the available read converts like every other
	// detail row rather than reporting the raw value.
	if want := float64(latestAttemptStats.DurationMs) / 60000.0; snapshot.Detail.DurationMinutes != want {
		t.Fatalf("available duration minutes = %v, want %v", snapshot.Detail.DurationMinutes, want)
	}
}

// TestLatestAttemptPreviewSnapshotReportsLatestAttemptStats proves the
// preview-snapshot read resolves the latest attempt's generation and its
// counts from the latest attempt's own producer stats document.
func TestLatestAttemptPreviewSnapshotReportsLatestAttemptStats(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("44445555-6666-4777-8888-999900001111")
	seedLatestAttemptCoexistence(t, s, sid)

	var snapshot indexformat.ReadSnapshot
	if err := s.WithSessionPreviewSnapshot(context.Background(), sid, func(s indexformat.ReadSnapshot) error {
		snapshot = s
		return nil
	}); err != nil {
		t.Fatalf("WithSessionPreviewSnapshot: %v", err)
	}
	if snapshot.GenerationID != "gen_preview_b" {
		t.Fatalf("preview snapshot generation = %q, want the latest attempt", snapshot.GenerationID)
	}
	if snapshot.Metadata.Stats.TurnCount != latestAttemptStats.TurnCount || snapshot.Metadata.Stats.ToolCallCount != latestAttemptStats.ToolCallCount {
		t.Fatalf("preview stats = %d/%d, want the latest attempt's %d/%d",
			snapshot.Metadata.Stats.TurnCount, snapshot.Metadata.Stats.ToolCallCount,
			latestAttemptStats.TurnCount, latestAttemptStats.ToolCallCount)
	}
	if snapshot.Session.TurnCount != latestAttemptStats.TurnCount {
		t.Fatalf("preview session turns = %d, want the latest attempt's %d", snapshot.Session.TurnCount, latestAttemptStats.TurnCount)
	}
}

// TestLatestAttemptCaptureReadIsDisplayEffective proves the display-effective
// capture read reports the latest attempt, so the mounted partial notice is
// reachable, while the authority read keeps the active certificate.
func TestLatestAttemptCaptureReadIsDisplayEffective(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("55556666-7777-4888-8999-000011112222")
	seedLatestAttemptCoexistence(t, s, sid)
	ctx := context.Background()

	capture, found, err := s.GetSessionContentCapture(ctx, ingest.SessionID(sid))
	if err != nil || !found {
		t.Fatalf("GetSessionContentCapture: %v (found=%v)", err, found)
	}
	if !PartialPreviewNeeded(capture) || capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		t.Fatalf("display capture = %s/%s/%s, want the latest-attempt partial state", capture.Status, capture.CaptureFormat, capture.FailureCode)
	}
	// The authority certificate is untouched and still readable through the
	// store's own authority read.
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	authority, found, authorityErr := readCapture(conn, ingest.SessionID(sid))
	s.pool.Put(conn)
	if authorityErr != nil || !found || authority.CaptureFormat != ingest.ContentCaptureFormatFull {
		t.Fatalf("authority capture = %s (found=%v, err=%v), want the full certificate", authority.CaptureFormat, found, authorityErr)
	}
}

// TestLatestAttemptIndexStateFields proves the three latest-attempt fields are
// populated from the pointer and the latest-attempt generation row, and stay nil
// without a latest attempt.
func TestLatestAttemptIndexStateFields(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("66667777-8888-4999-8000-111122223333")
	identity := seedLatestAttemptCoexistence(t, s, sid)

	state, err := s.ReadIndexState(context.Background(), ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("ReadIndexState: %v", err)
	}
	if state.LatestAttemptArtifactHash == nil || *state.LatestAttemptArtifactHash != identity {
		t.Fatalf("latest-attempt pair identity = %v, want the recorded identity", state.LatestAttemptArtifactHash)
	}
	if state.LatestAttemptIndexerVersion == nil || *state.LatestAttemptIndexerVersion != 18 {
		t.Fatalf("latest-attempt producer revision = %v, want 18", state.LatestAttemptIndexerVersion)
	}
	if state.LatestAttemptAdapterVersion == nil || *state.LatestAttemptAdapterVersion != 2 {
		t.Fatalf("latest-attempt adapter revision = %v, want 2", state.LatestAttemptAdapterVersion)
	}

	// A session with no latest attempt reports all three as NULL.
	other := schema.SessionID("77778888-9999-4000-8111-222233334444")
	seedFullAuthorityWithStats(t, s, other, authorityStats)
	empty, err := s.ReadIndexState(context.Background(), ingest.SessionID(other))
	if err != nil {
		t.Fatalf("ReadIndexState without a latest attempt: %v", err)
	}
	if empty.LatestAttemptArtifactHash != nil || empty.LatestAttemptIndexerVersion != nil || empty.LatestAttemptAdapterVersion != nil {
		t.Fatalf("latest-attempt fields = %v/%v/%v, want all NULL without a latest attempt",
			empty.LatestAttemptArtifactHash, empty.LatestAttemptIndexerVersion, empty.LatestAttemptAdapterVersion)
	}
}

// TestLatestAttemptPublicationReadNamesPendingAttempt proves the committed
// publication read names the latest attempt and never reports the session
// ready, and that the explicit last-good selection clears only that refusal.
func TestLatestAttemptPublicationReadNamesPendingAttempt(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("99990000-1111-4222-8333-444455556666")
	seedLatestAttemptCoexistence(t, s, sid)
	// The publication read parses the stored project identity, so the session
	// needs a well-formed one: the seeding helper's placeholder is not.
	projectHash := strings.Repeat("1", 64)
	execGenerationSQL(t, s, `
INSERT OR IGNORE INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES('`+projectHash+`','/tmp/gen','github.com/gen/gen');
UPDATE sessions SET project_hash = '`+projectHash+`' WHERE session_id = '`+string(sid)+`';`)
	ctx := context.Background()

	bundle, err := s.LoadPublicationInput(ctx, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("LoadPublicationInput: %v", err)
	}
	if !bundle.LatestAttemptPending {
		t.Fatal("LoadPublicationInput did not name the pending latest attempt")
	}
	// The refusal is the pending fact, not a bare "not ready": a session that
	// is not ready for some unrelated reason would satisfy a readiness check
	// alone. The discriminating transition (ready -> pending -> not ready ->
	// complete -> ready again) is proven by
	// TestLatestAttemptCompleteGenerationDisposesTheAttempt; the actionable
	// coexistence reason is proven at the push seam by
	// TestPublicationReadRefusesWithCoexistenceReasonAndLastGoodServesAuthority.

	var selected ingest.PublicationInputBundle
	if err := s.WithCommittedPublicationInputSelected(ctx, ingest.SessionID(sid), ingest.SessionReadSelection{LastGood: true}, func(input ingest.PublicationInputBundle) error {
		selected = input
		return nil
	}); err != nil {
		t.Fatalf("WithCommittedPublicationInputSelected(LastGood): %v", err)
	}
	if selected.LatestAttemptPending {
		t.Fatal("the explicit last-good read kept the latest-attempt refusal")
	}
}

// TestLatestAttemptSettleGatesSelection proves the three selection surfaces:
// a fully settled latest attempt stops being selected, a moved facet keeps it
// selected, and the repair publication arm keeps listing regardless.
func TestLatestAttemptSettleGatesSelection(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("88889999-0000-4111-8222-333344445555")
	seedLatestAttemptCoexistence(t, s, sid)
	ctx := context.Background()
	settled := map[ingest.Harness]ingest.HarvesterVersions{
		ingest.Harness("claude-code"): {IndexerVersion: 18, AdapterVersion: 2},
	}
	bumped := map[ingest.Harness]ingest.HarvesterVersions{
		ingest.Harness("claude-code"): {IndexerVersion: 18, AdapterVersion: 3},
	}

	assertSelected := func(label string, ids []ingest.SessionID, want bool) {
		t.Helper()
		got := false
		for _, id := range ids {
			if string(id) == string(sid) {
				got = true
			}
		}
		if got != want {
			t.Fatalf("%s selected=%v, want %v (ids=%v)", label, got, want, ids)
		}
	}

	ids, err := s.ListStaleIndexSessions(ctx, settled)
	if err != nil {
		t.Fatalf("ListStaleIndexSessions: %v", err)
	}
	assertSelected("stale index (settled)", ids, false)
	ids, err = s.ListStaleIndexSessions(ctx, bumped)
	if err != nil {
		t.Fatalf("ListStaleIndexSessions bumped: %v", err)
	}
	assertSelected("stale index (adapter target bumped)", ids, true)

	ids, err = s.ListStaleAdapterSessions(ctx, settled, nil)
	if err != nil {
		t.Fatalf("ListStaleAdapterSessions: %v", err)
	}
	assertSelected("stale adapter (settled)", ids, false)
	ids, err = s.ListStaleAdapterSessions(ctx, bumped, nil)
	if err != nil {
		t.Fatalf("ListStaleAdapterSessions bumped: %v", err)
	}
	assertSelected("stale adapter (adapter target bumped)", ids, true)

	ids, err = s.ListSessionsNeedingRepair(ctx, settled)
	if err != nil {
		t.Fatalf("ListSessionsNeedingRepair: %v", err)
	}
	assertSelected("repair artifact arm (settled)", ids, false)

	// A target that cannot express a settle decision must never hide work: the
	// settle clause is omitted, so the session stays listed exactly as it did
	// before the latest attempt existed.
	zeroAdapter := map[ingest.Harness]ingest.HarvesterVersions{
		ingest.Harness("claude-code"): {IndexerVersion: 18, AdapterVersion: 0},
	}
	zeroIndexer := map[ingest.Harness]ingest.HarvesterVersions{
		ingest.Harness("claude-code"): {IndexerVersion: 0, AdapterVersion: 2},
	}
	ids, err = s.ListStaleIndexSessions(ctx, zeroAdapter)
	if err != nil {
		t.Fatalf("ListStaleIndexSessions with a zero adapter target: %v", err)
	}
	assertSelected("stale index (zero adapter target)", ids, true)
	ids, err = s.ListStaleAdapterSessions(ctx, zeroIndexer, nil)
	if err != nil {
		t.Fatalf("ListStaleAdapterSessions with a zero producer target: %v", err)
	}
	assertSelected("stale adapter (zero producer target)", ids, true)
	ids, err = s.ListSessionsNeedingRepair(ctx, zeroAdapter)
	if err != nil {
		t.Fatalf("ListSessionsNeedingRepair with a zero adapter target: %v", err)
	}
	assertSelected("repair artifact arm (zero adapter target)", ids, true)

	// The publication arm keeps LISTING the pending binding even though the
	// latest attempt is settled: the pending fact is real, and only the PARSE
	// decision short-circuits.
	execGenerationSQL(t, s, `
UPDATE sessions SET publication_capture_revision = 7, indexed_publication_capture_revision = 0, cwd_provenance_kind = 'source_exact', session_cwd = '/tmp/gen' WHERE session_id = '`+string(sid)+`';
INSERT INTO session_publication_metadata(capture_revision, schema_version, metadata_json, metadata_hash, content_hash, captured_at, session_id)
VALUES (7, `+strconv.Itoa(int(ingest.CurrentSchemaVersion))+`, '{"seed":true}', '`+strings.Repeat("c", 64)+`', '`+strings.Repeat("d", 64)+`', 1700000000000, '`+string(sid)+`');`)
	ids, err = s.ListSessionsNeedingRepair(ctx, settled)
	if err != nil {
		t.Fatalf("ListSessionsNeedingRepair with a pending binding: %v", err)
	}
	assertSelected("repair publication arm (settled)", ids, true)
}

// TestLatestAttemptStaleAttemptStillServesItsOwnContent proves the stale-attempt
// read: with the latest-attempt pointer's pair identity differing from the session's
// current artifact hash, the attempt is still the actual last attempted
// snapshot, so the mounted reads and the preview-snapshot read serve ITS own
// entries, counts and partial flag — never the authority and never the
// unindexed current source — while the full read keeps refusing.
func TestLatestAttemptStaleAttemptStillServesItsOwnContent(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("aaaabbbb-2222-4333-8444-555566667777")
	seedLatestAttemptCoexistence(t, s, sid)
	// The pair moved on after the attempt: the row now carries a different
	// identity than the one the latest-attempt pointer recorded.
	execGenerationSQL(t, s, `UPDATE sessions SET artifact_hash = '`+hexIdentity('9')+`' WHERE session_id = '`+string(sid)+`';`)
	ctx := context.Background()

	available, err := s.ReadSessionAvailable(ctx, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("ReadSessionAvailable on a stale attempt: %v", err)
	}
	if !PartialPreviewNeeded(available.Capture) || available.Capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		t.Fatalf("stale available capture = %+v, want the attempt's partial preview state", available.Capture)
	}
	if len(available.Entries) != 3 {
		t.Fatalf("stale available entries = %d, want the attempt's 3", len(available.Entries))
	}
	if first := available.Entries[0]; first.ContentPreview == nil || !strings.Contains(*first.ContentPreview, "preview text") {
		t.Fatalf("stale available entry 0 = %+v, want the attempt's own content", first.ContentPreview)
	}
	if available.Detail.TurnCount != latestAttemptStats.TurnCount || available.Detail.ToolCalls != latestAttemptStats.ToolCallCount {
		t.Fatalf("stale available counts = %d/%d, want the attempt's %d/%d",
			available.Detail.TurnCount, available.Detail.ToolCalls,
			latestAttemptStats.TurnCount, latestAttemptStats.ToolCallCount)
	}

	var preview indexformat.ReadSnapshot
	if err := s.WithSessionPreviewSnapshot(ctx, sid, func(snapshot indexformat.ReadSnapshot) error {
		preview = snapshot
		return nil
	}); err != nil {
		t.Fatalf("WithSessionPreviewSnapshot on a stale attempt: %v", err)
	}
	if preview.GenerationID != "gen_preview_b" {
		t.Fatalf("stale preview snapshot generation = %q, want the attempt", preview.GenerationID)
	}
	if preview.Metadata.Stats.TurnCount != latestAttemptStats.TurnCount {
		t.Fatalf("stale preview stats = %d, want the attempt's %d", preview.Metadata.Stats.TurnCount, latestAttemptStats.TurnCount)
	}

	callbackRan := false
	if err := s.WithSessionSnapshot(ctx, sid, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	}); !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("stale full read = %v, want the latest-attempt sentinel", err)
	}
	if callbackRan {
		t.Fatal("the stale full read ran the callback for a session with a latest attempt")
	}
}

// TestUnknownSessionReadsKeepNotFoundClassification proves an id with no
// metadata row is answered as not found by every snapshot selection: the
// zero-selection read and the explicit last-good read both return the
// indexformat not-found sentinel, run no callback, and name the session. The
// last-good authority check runs ahead of the representation for every
// selection, so it must carry the same classification the row read produces;
// losing it would turn an honest not-found into a server error at the API
// mapping that recognizes the sentinel.
func TestUnknownSessionReadsKeepNotFoundClassification(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("99990000-1111-4222-8333-444455556666")
	ctx := context.Background()

	callbackRan := false
	if err := s.WithSessionSnapshot(ctx, sid, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	}); !errors.Is(err, indexformat.ErrSnapshotNotFound) {
		t.Fatalf("zero-selection read error = %v, want the not-found sentinel", err)
	}
	if callbackRan {
		t.Fatal("the zero-selection read ran the callback for an unknown session")
	}

	callbackRan = false
	err := s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, indexformat.ErrSnapshotNotFound) {
		t.Fatalf("last-good read error = %v, want the not-found sentinel the zero-selection read keeps", err)
	}
	if callbackRan {
		t.Fatal("the last-good read ran the callback for an unknown session")
	}
	if !strings.Contains(err.Error(), string(sid)) {
		t.Fatalf("last-good read error = %v, want it to still name the unknown session", err)
	}
}
