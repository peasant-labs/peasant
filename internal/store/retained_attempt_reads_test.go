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

// This file proves the store-side read contract of retained latest-preview
// coexistence: which generation each read answers for, which counts it
// reports, and which selection surfaces treat the retained attempt as settled.
// The mounted and end-to-end evidence over the same seams belongs to the
// integration family, not here.

// authorityStats and retainedStats are the two DISTINCT literal producer-stat
// documents the counts-scope assertions use, so a read that reports the wrong
// generation's counts cannot pass by coincidence. Only the count fields are
// replaced: a complete generation must keep its input-submission count.
var (
	authorityStats = countsFor(11, 22, 33, 66000)
	retainedStats  = countsFor(44, 55, 66, 132000)
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

// retainPreviewWithStats retains one preview attempt over the seeded authority,
// carrying the supplied producer stats and adapter revision.
func retainPreviewWithStats(t *testing.T, s *Store, sid schema.SessionID, genID string, stats schema.SessionStats, adapterVersion int, identity string) {
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
		t.Fatalf("retention: %v", err)
	}
	if outcome.Disposition != ingest.ActivationRetainedPreview {
		t.Fatalf("retention disposition = %v, want ActivationRetainedPreview", outcome.Disposition)
	}
}

// seedRetainedCoexistence seeds the canonical coexistence state: a managed full
// authority with distinct counts, plus one retained preview attempt with its
// own distinct counts and adapter revision.
func seedRetainedCoexistence(t *testing.T, s *Store, sid schema.SessionID) string {
	t.Helper()
	seedFullAuthorityWithStats(t, s, sid, authorityStats)
	identity := hexIdentity('7')
	retainPreviewWithStats(t, s, sid, "gen_preview_b", retainedStats, 2, identity)
	return identity
}

// TestRetainedAttemptRefusesFullReadsUniformly proves the uniform refusal: an
// ordinary full snapshot and the full content read both return the retained
// sentinel before any representation is resolved, and an explicit last-good
// selection serves the active authority instead.
func TestRetainedAttemptRefusesFullReadsUniformly(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("11112222-3333-4444-8555-666677778888")
	seedRetainedCoexistence(t, s, sid)
	ctx := context.Background()

	callbackRan := false
	err := s.WithSessionSnapshot(ctx, sid, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, indexformat.ErrRetainedAttemptIncomplete) {
		t.Fatalf("WithSessionSnapshot error = %v, want the retained-attempt sentinel", err)
	}
	if callbackRan {
		t.Fatal("WithSessionSnapshot invoked the callback for a retained session")
	}
	if _, err := s.ReadSessionContent(ctx, string(sid)); !errors.Is(err, indexformat.ErrRetainedAttemptIncomplete) {
		t.Fatalf("ReadSessionContent error = %v, want the retained-attempt sentinel", err)
	}

	// The explicit last-good selection serves the active authority: its
	// generation and its own counts, never the retained attempt's.
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

// TestRetainedAttemptRefusalIsUniformForCertificateAuthority proves the refusal
// does not depend on the authority's representation: with the active pointer
// cleared — the certificate or file-backed authority's state — the ordinary
// read still refuses, and last-good serves the preserved legacy read.
func TestRetainedAttemptRefusalIsUniformForCertificateAuthority(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("22223333-4444-4555-8666-777788889999")
	seedRetainedCoexistence(t, s, sid)
	ctx := context.Background()
	// Clear the active pointer: the session keeps its certificate and its
	// retained attempt, exactly the state a V1-certificate or file-backed
	// authority has while a managed preview is retained beside it.
	execGenerationSQL(t, s, `UPDATE sessions SET active_generation_id = NULL WHERE session_id = '`+string(sid)+`';`)

	err := s.WithSessionSnapshot(ctx, sid, func(indexformat.ReadSnapshot) error {
		t.Fatal("the callback must not run for a retained session")
		return nil
	})
	if !errors.Is(err, indexformat.ErrRetainedAttemptIncomplete) {
		t.Fatalf("WithSessionSnapshot error = %v, want the retained-attempt sentinel", err)
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

// TestRetainedAttemptLastGoodRefusesActivePreview proves the explicit last-good
// selection refuses an active authority that is only a preview, in both shapes:
// with the retained pointer set and the certificate flipped to a preview, and in
// the explicit-rebuild shape where the pointer is cleared and the preview is the
// active authority.
func TestRetainedAttemptLastGoodRefusesActivePreview(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("aaaabbbb-cccc-4ddd-8eee-ffff00001111")
	seedRetainedCoexistence(t, s, sid)
	ctx := context.Background()

	// Shape 1: the retained pointer stays set; only the certificate becomes an
	// incomplete preview, so no full generation remains to serve as last-good.
	execGenerationSQL(t, s, `UPDATE session_content_captures SET status='incomplete', capture_format='preview_only', failure_code='source_records_omitted' WHERE session_id = '`+string(sid)+`';`)

	callbackRan := false
	err := s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, indexformat.ErrRetainedAttemptIncomplete) {
		t.Fatalf("last-good snapshot error = %v, want the retained-attempt sentinel", err)
	}
	if callbackRan {
		t.Fatal("last-good served a snapshot for an active preview")
	}
	if _, err := s.ReadSessionContentSelected(ctx, string(sid), ingest.SessionReadSelection{LastGood: true}); !errors.Is(err, indexformat.ErrRetainedAttemptIncomplete) {
		t.Fatalf("last-good content error = %v, want the retained-attempt sentinel", err)
	}

	// Shape 2: an explicit rebuild cleared the retained pointer and installed
	// the preview as the active authority. The snapshot lane still refuses; the
	// content lane refuses through its own capture authority, so the two lanes
	// are independently guarded.
	execGenerationSQL(t, s, `UPDATE sessions SET active_generation_id = 'gen_preview_b', latest_attempt_generation_id = NULL, latest_attempt_artifact_hash = NULL, latest_attempt_indexer_version = NULL, latest_attempt_failure_code = NULL, latest_attempt_failure_message = NULL, latest_attempt_captured_at_ms = NULL, latest_attempt_stats_json = NULL WHERE session_id = '`+string(sid)+`';`)

	callbackRan = false
	err = s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, indexformat.ErrRetainedAttemptIncomplete) {
		t.Fatalf("state-9 last-good snapshot error = %v, want the retained-attempt sentinel", err)
	}
	if callbackRan {
		t.Fatal("state-9 last-good served a snapshot for an active preview")
	}
	if _, err := s.ReadSessionContentSelected(ctx, string(sid), ingest.SessionReadSelection{LastGood: true}); !errors.Is(err, ErrContentCaptureIncomplete) {
		t.Fatalf("state-9 last-good content error = %v, want the incomplete-capture refusal", err)
	}
}

// TestRetainedAttemptAvailableRefusesMissingGenerationRow proves the available
// read refuses when the retained pointer names a generation row that is gone,
// instead of serving a partial-flagged transcript with nothing in it, while the
// other reads of the same session keep answering.
func TestRetainedAttemptAvailableRefusesMissingGenerationRow(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("bbbbcccc-dddd-4eee-8fff-000011112222")
	seedRetainedCoexistence(t, s, sid)
	ctx := context.Background()
	execGenerationSQL(t, s, `DELETE FROM session_generations WHERE session_id = '`+string(sid)+`' AND generation_id = 'gen_preview_b';`)

	if _, err := s.ReadSessionAvailable(ctx, ingest.SessionID(sid)); err == nil {
		t.Fatal("ReadSessionAvailable served a retained pointer whose generation row is gone")
	} else if !strings.Contains(err.Error(), string(sid)) || !strings.Contains(err.Error(), "gen_preview_b") {
		t.Fatalf("available refusal = %v, want it to name the session and the retained generation", err)
	}
	if err := s.WithSessionSnapshot(ctx, sid, func(indexformat.ReadSnapshot) error {
		t.Fatal("the callback must not run for a retained session")
		return nil
	}); !errors.Is(err, indexformat.ErrRetainedAttemptIncomplete) {
		t.Fatalf("default snapshot error = %v, want the retained-attempt sentinel", err)
	}
	served := ""
	if err := s.WithSessionSnapshotSelected(ctx, sid, ingest.SessionReadSelection{LastGood: true}, func(snapshot indexformat.ReadSnapshot) error {
		served = snapshot.GenerationID
		return nil
	}); err != nil {
		t.Fatalf("last-good read after the retained row was removed: %v", err)
	}
	if served != "gen_full_prior" {
		t.Fatalf("last-good generation = %q, want the full authority", served)
	}
}

// TestRetainedAttemptAvailableReadBindsRetainedAttempt proves the available
// read serves the retained attempt: its generation-scoped entries, its
// synthesized capture with the partial state, and its own counts.
func TestRetainedAttemptAvailableReadBindsRetainedAttempt(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("33334444-5555-4666-8777-888899990000")
	seedRetainedCoexistence(t, s, sid)

	snapshot, err := s.ReadSessionAvailable(context.Background(), ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("ReadSessionAvailable: %v", err)
	}
	if !PartialPreviewNeeded(snapshot.Capture) {
		t.Fatalf("available capture = %s/%s, want the retained partial state", snapshot.Capture.Status, snapshot.Capture.CaptureFormat)
	}
	if snapshot.Capture.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly || snapshot.Capture.Status != ingest.ContentCaptureIncomplete {
		t.Fatalf("available capture = %s/%s, want preview_only/incomplete", snapshot.Capture.CaptureFormat, snapshot.Capture.Status)
	}
	if snapshot.Capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		t.Fatalf("available capture failure code = %q, want source_records_omitted", snapshot.Capture.FailureCode)
	}
	if len(snapshot.Entries) != 3 {
		t.Fatalf("available entries = %d, want the retained attempt's 3", len(snapshot.Entries))
	}
	if first := snapshot.Entries[0]; first.ContentPreview == nil || !strings.Contains(*first.ContentPreview, "preview text") {
		t.Fatalf("available entry 0 = %+v, want the retained attempt's own content", first.ContentPreview)
	}
	if snapshot.Detail.TurnCount != retainedStats.TurnCount || snapshot.Detail.ToolCalls != retainedStats.ToolCallCount || snapshot.Detail.OutputTokens != retainedStats.TokensOut {
		t.Fatalf("available counts = %d/%d/%d, want the retained attempt's %d/%d/%d",
			snapshot.Detail.TurnCount, snapshot.Detail.ToolCalls, snapshot.Detail.OutputTokens,
			retainedStats.TurnCount, retainedStats.ToolCallCount, retainedStats.TokensOut)
	}
	// The retained producer document records milliseconds while this detail
	// column carries minutes, so the available read converts like every other
	// detail row rather than reporting the raw value.
	if want := float64(retainedStats.DurationMs) / 60000.0; snapshot.Detail.DurationMinutes != want {
		t.Fatalf("available duration minutes = %v, want %v", snapshot.Detail.DurationMinutes, want)
	}
}

// TestRetainedAttemptPreviewSnapshotReportsRetainedStats proves the
// preview-snapshot read resolves the retained attempt's generation and its
// counts from the retained attempt's own producer stats document.
func TestRetainedAttemptPreviewSnapshotReportsRetainedStats(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("44445555-6666-4777-8888-999900001111")
	seedRetainedCoexistence(t, s, sid)

	var snapshot indexformat.ReadSnapshot
	if err := s.WithSessionPreviewSnapshot(context.Background(), sid, func(s indexformat.ReadSnapshot) error {
		snapshot = s
		return nil
	}); err != nil {
		t.Fatalf("WithSessionPreviewSnapshot: %v", err)
	}
	if snapshot.GenerationID != "gen_preview_b" {
		t.Fatalf("preview snapshot generation = %q, want the retained attempt", snapshot.GenerationID)
	}
	if snapshot.Metadata.Stats.TurnCount != retainedStats.TurnCount || snapshot.Metadata.Stats.ToolCallCount != retainedStats.ToolCallCount {
		t.Fatalf("preview stats = %d/%d, want the retained attempt's %d/%d",
			snapshot.Metadata.Stats.TurnCount, snapshot.Metadata.Stats.ToolCallCount,
			retainedStats.TurnCount, retainedStats.ToolCallCount)
	}
	if snapshot.Session.TurnCount != retainedStats.TurnCount {
		t.Fatalf("preview session turns = %d, want the retained attempt's %d", snapshot.Session.TurnCount, retainedStats.TurnCount)
	}
}

// TestRetainedAttemptCaptureReadIsDisplayEffective proves the display-effective
// capture read reports the retained attempt, so the mounted partial notice is
// reachable, while the authority read keeps the active certificate.
func TestRetainedAttemptCaptureReadIsDisplayEffective(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("55556666-7777-4888-8999-000011112222")
	seedRetainedCoexistence(t, s, sid)
	ctx := context.Background()

	capture, found, err := s.GetSessionContentCapture(ctx, ingest.SessionID(sid))
	if err != nil || !found {
		t.Fatalf("GetSessionContentCapture: %v (found=%v)", err, found)
	}
	if !PartialPreviewNeeded(capture) || capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		t.Fatalf("display capture = %s/%s/%s, want the retained partial state", capture.Status, capture.CaptureFormat, capture.FailureCode)
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

// TestRetainedAttemptIndexStateFields proves the three latest-attempt fields are
// populated from the pointer and the retained generation row, and stay nil
// without a retained attempt.
func TestRetainedAttemptIndexStateFields(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("66667777-8888-4999-8000-111122223333")
	identity := seedRetainedCoexistence(t, s, sid)

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

	// A session with no retained attempt reports all three as NULL.
	other := schema.SessionID("77778888-9999-4000-8111-222233334444")
	seedFullAuthorityWithStats(t, s, other, authorityStats)
	empty, err := s.ReadIndexState(context.Background(), ingest.SessionID(other))
	if err != nil {
		t.Fatalf("ReadIndexState without a retained attempt: %v", err)
	}
	if empty.LatestAttemptArtifactHash != nil || empty.LatestAttemptIndexerVersion != nil || empty.LatestAttemptAdapterVersion != nil {
		t.Fatalf("latest-attempt fields = %v/%v/%v, want all NULL without a retained attempt",
			empty.LatestAttemptArtifactHash, empty.LatestAttemptIndexerVersion, empty.LatestAttemptAdapterVersion)
	}
}

// TestRetainedAttemptPublicationReadNamesPendingAttempt proves the committed
// publication read names the retained attempt and never reports the session
// ready, and that the explicit last-good selection clears only that refusal.
func TestRetainedAttemptPublicationReadNamesPendingAttempt(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("99990000-1111-4222-8333-444455556666")
	seedRetainedCoexistence(t, s, sid)
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
	if !bundle.RetainedAttemptPending {
		t.Fatal("LoadPublicationInput did not name the pending retained attempt")
	}
	if bundle.Readiness == ingest.PublicationReady {
		t.Fatal("LoadPublicationInput reported a retained session ready for publication")
	}

	var selected ingest.PublicationInputBundle
	if err := s.WithCommittedPublicationInputSelected(ctx, ingest.SessionID(sid), ingest.SessionReadSelection{LastGood: true}, func(input ingest.PublicationInputBundle) error {
		selected = input
		return nil
	}); err != nil {
		t.Fatalf("WithCommittedPublicationInputSelected(LastGood): %v", err)
	}
	if selected.RetainedAttemptPending {
		t.Fatal("the explicit last-good read kept the retained-attempt refusal")
	}
}

// TestRetainedAttemptSettleGatesSelection proves the three selection surfaces:
// a fully settled retained attempt stops being selected, a moved facet keeps it
// selected, and the repair publication arm keeps listing regardless.
func TestRetainedAttemptSettleGatesSelection(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("88889999-0000-4111-8222-333344445555")
	seedRetainedCoexistence(t, s, sid)
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

	// The publication arm keeps LISTING the pending binding even though the
	// retained attempt is settled: the pending fact is real, and only the PARSE
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
