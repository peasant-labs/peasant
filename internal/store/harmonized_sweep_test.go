package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The sweep's API, recovery, concurrency, and reclaim-twin tests. The
// fixture-driven sweep family lives in content_gc_test.go; these tests prove
// the production entry points around it: SweepSession and
// SweepFlaggedSessions, the crash-recovery contract, the lock serialization
// behind the concurrency cases, the one-copy twin, and the reclaim twins
// (flag selection, orphan objects, the harmonized directory-only retry).
// Nothing here runs parallel: the sweep's crash seam is process-global.

// TestSweepSessionBatchBound proves bounded batches terminate exactly: with
// one row per batch the sweep still deletes the whole superseded set,
// clears the flag, and reports the exact counts.
func TestSweepSessionBatchBound(t *testing.T) {
	s, _ := openGenerationStoreWith(t, WithWriteConfig(ingest.WriteConfig{BatchBytes: 64, BatchSessions: 64, SweepRows: 1}.WithDefaults(1)))
	ctx := context.Background()
	sid := gcSession(t, s, "b1b1b1b1-b1b1-41b1-81b1-b1b1b1b1b1b1")
	oldTerms := []string{"gcbatcho0", "gcbatcho1", "gcbatcho2"}
	activeTerms := []string{"gcbatchn0", "gcbatchn1", "gcbatchn2"}
	gcActivate(t, s, sid, "gc_batch_old", oldTerms, nil)
	active, activeBlobs := gcActivate(t, s, sid, "gc_batch_active", activeTerms, nil)
	got, err := s.SweepSession(ctx, sid)
	if err != nil {
		t.Fatalf("bounded sweep: %v", err)
	}
	if got.RowsDeleted != 7 || got.BodiesDeleted != 3 || got.BlobsDeleted != 0 || got.DirectoriesRemoved != 0 {
		t.Fatalf("bounded sweep = %+v, want 7 rows, 3 bodies, no blobs or dirs", got)
	}
	if got.Rebuilt {
		t.Fatal("bounded sweep rebuilt the index with no untrusted delete")
	}
	if readSweepFlag(t, s, sid) {
		t.Fatal("bounded sweep left the flag set")
	}
	assertGCDigestsEqual(t, s, sid, "batch-bound", gcBodyDigests(t, sid, active, activeBlobs))
	for _, term := range activeTerms {
		if got := gcMatchCount(t, s, term); got == 0 {
			t.Fatalf("raw MATCH for surviving term %q returned no rows", term)
		}
	}
	for _, term := range oldTerms {
		if got := gcMatchCount(t, s, term); got != 0 {
			t.Fatalf("raw MATCH for swept term %q returned %d rows, want none", term, got)
		}
	}
}

// TestSweepSessionUnknownSession proves the sweep fails closed on an
// unknown session: no lock fiction, no flag write, an actionable error.
func TestSweepSessionUnknownSession(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid, err := schema.NewSessionID("c2c2c2c2-c2c2-42c2-82c2-c2c2c2c2c2c2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepSession(context.Background(), sid); err == nil {
		t.Fatal("sweeping an unknown session succeeded; it must refuse with no metadata row")
	}
}

// TestSweepFlaggedSessions proves the harvest-start recovery pass: every
// flagged session is swept under its own lock, an unflagged session with
// rows is never touched, and the pass reports what it swept.
func TestSweepFlaggedSessions(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	worked := gcSession(t, s, "d3d3d3d3-d3d3-43d3-83d3-d3d3d3d3d3d3")
	gcActivate(t, s, worked, "gc_flagged_old", []string{"gcflaggedo0", "gcflaggedo1", "gcflaggedo2"}, nil)
	gcActivate(t, s, worked, "gc_flagged_active", []string{"gcflaggedn0", "gcflaggedn1", "gcflaggedn2"}, nil)

	skipped := gcSession(t, s, "e4e4e4e4-e4e4-44e4-84e4-e4e4e4e4e4e4")
	gcActivate(t, s, skipped, "gc_skipped_old", []string{"gcskippedo0", "gcskippedo1", "gcskippedo2"}, nil)
	gcActivate(t, s, skipped, "gc_skipped_active", []string{"gcskippedn0", "gcskippedn1", "gcskippedn2"}, nil)
	execGenerationSQL(t, s, `UPDATE sessions SET content_sweep_pending = 0 WHERE session_id = 'e4e4e4e4-e4e4-44e4-84e4-e4e4e4e4e4e4';`)
	skippedGenerations := gcCount(t, s, "session_generations", skipped)

	report, err := s.SweepFlaggedSessions(ctx)
	if err != nil {
		t.Fatalf("flagged sweep: %v", err)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("flagged sweep warnings = %v, want none", report.Warnings)
	}
	if len(report.Sessions) != 1 || report.Sessions[0].SessionID != worked {
		t.Fatalf("flagged sweep swept %v, want only the flagged session", report.Sessions)
	}
	if readSweepFlag(t, s, worked) {
		t.Fatal("flagged session kept its flag after the sweep")
	}
	if got := gcCount(t, s, "session_generations", skipped); got != skippedGenerations {
		t.Fatalf("unflagged session holds %d generation rows after the pass, want the untouched %d", got, skippedGenerations)
	}
	if got := gcMatchCount(t, s, "gcskippedo0"); got == 0 {
		t.Fatal("unflagged session lost its postings to a pass that must never touch it")
	}
}

// TestSweepFlaggedSessionsWarnsAndKeepsFlag proves a failing session does
// not fail the pass: an unowned orphan directory refuses removal, the pass
// reports the warning, the session keeps its flag, and the other sessions
// still sweep clean.
func TestSweepFlaggedSessionsWarnsAndKeepsFlag(t *testing.T) {
	s, root := openGenerationStore(t)
	ctx := context.Background()
	bad := gcSession(t, s, "f5f5f5f5-f5f5-45f5-85f5-f5f5f5f5f5f5")
	gcActivate(t, s, bad, "gc_bad_active", []string{"gcbado0", "gcbado1", "gcbado2"}, nil)
	if err := os.MkdirAll(filepath.Join(root, string(bad), "generations", "gc_unowned_dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := gcSession(t, s, "a6a6a6a6-a6a6-46a6-86a6-a6a6a6a6a6a6")
	gcActivate(t, s, good, "gc_good_old", []string{"gcgoodo0", "gcgoodo1", "gcgoodo2"}, nil)
	gcActivate(t, s, good, "gc_good_active", []string{"gcgoodn0", "gcgoodn1", "gcgoodn2"}, nil)

	report, err := s.SweepFlaggedSessions(ctx)
	if err != nil {
		t.Fatalf("flagged sweep: %v", err)
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("flagged sweep warnings = %v, want the 1 unowned-directory failure", report.Warnings)
	}
	if len(report.Sessions) != 1 || report.Sessions[0].SessionID != good {
		t.Fatalf("flagged sweep swept %v, want only the clean session", report.Sessions)
	}
	if !readSweepFlag(t, s, bad) {
		t.Fatal("failing session lost its flag; the next pass must retry it")
	}
	if readSweepFlag(t, s, good) {
		t.Fatal("clean session kept its flag after the sweep")
	}
}

// TestSweepCrashAfterCommitRecoversByReharvest proves the crash-recovery
// contract for a crash between the commit and the sweep (content_crash_seams
// after-commit-before-sweep): the new generation is active with the flag
// set, and the next harvest — the flagged sweep alone for unchanged input,
// a fresh activation plus the sweep for changed input — leaves zero
// orphans, a clear flag, and no stale search match.
func TestSweepCrashAfterCommitRecoversByReharvest(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	sid, err := schema.NewSessionID("b7b7b7b7-b7b7-47b7-87b7-b7b7b7b7b7b7")
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, string(sid))
	g1, g1Blobs := crashCandidate(t, sid, "gen_crash_g1", "G1")
	if err := activateTestGeneration(t, s, g1, g1Blobs); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	installHarmonizedFault(t, harmonizedSeamAfterCommit)
	g2, g2Blobs := crashCandidate(t, sid, "gen_crash_g2", "G2")
	outcome, interrupted := s.ActivateGeneration(ctx, GenerationActivation{
		Generation:     filledCandidateForValidation(t, g2, g2Blobs),
		Blobs:          g2Blobs,
		IndexerVersion: 1,
		IndexedAtMs:    1,
	})
	clearHarmonizedFault()
	if interrupted == nil {
		t.Fatal("activation across the after-commit seam succeeded; the crash must interrupt it")
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("interrupted disposition = %v, want CommittedNow: the commit is durable", outcome.Disposition)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_crash_g2" {
		t.Fatalf("visible generation = %q after the crash, want the committed G2", got)
	}
	if !readSweepFlag(t, s, sid) {
		t.Fatal("committed generation left the sweep flag unset")
	}

	// Unchanged input recovers through the flagged sweep alone: no new
	// generation, no re-stage, just the sweep the harvest start runs.
	recovered, err := s.SweepFlaggedSessions(ctx)
	if err != nil {
		t.Fatalf("flagged recovery sweep: %v", err)
	}
	if len(recovered.Sessions) != 1 || len(recovered.Warnings) != 0 {
		t.Fatalf("flagged recovery swept %v with warnings %v, want the 1 crashed session cleanly", recovered.Sessions, recovered.Warnings)
	}
	assertSweepRecovered(t, s, sid, "gen_crash_g2", g2, g2Blobs)

	// Changed input recovers through re-selection and re-stage: a fresh
	// candidate commits over the reusable store, and its sweep settles.
	g3, g3Blobs := crashCandidate(t, sid, "gen_crash_g3", "G3")
	if _, err := s.ActivateGeneration(ctx, GenerationActivation{
		Generation:     filledCandidateForValidation(t, g3, g3Blobs),
		Blobs:          g3Blobs,
		IndexerVersion: 2,
		IndexedAtMs:    2,
	}); err != nil {
		t.Fatalf("re-activate changed input: %v", err)
	}
	swept, err := s.SweepSession(ctx, sid)
	if err != nil {
		t.Fatalf("sweep after the changed-input commit: %v", err)
	}
	if swept.RowsDeleted == 0 {
		t.Fatal("changed-input sweep deleted no rows; the superseded generations must go")
	}
	assertSweepRecovered(t, s, sid, "gen_crash_g3", g3, g3Blobs)
}

// assertSweepRecovered proves one recovered session holds zero orphans, a
// clear flag, the committed content, and no stale search match: the crash
// terms are gone and the committed terms match.
func assertSweepRecovered(t *testing.T, s *Store, sid schema.SessionID, generationID string, candidate indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
	t.Helper()
	if readSweepFlag(t, s, sid) {
		t.Fatalf("session %s kept its flag after recovery", sid)
	}
	if got := visibleGeneration(t, s, sid); got != generationID {
		t.Fatalf("visible generation = %q after recovery, want %q", got, generationID)
	}
	assertHarmonizedContent(t, s, sid, generationID, candidate, blobs)
}

// TestSweepCorruptBodyRebuildsIndex proves the sweep's verified delete:
// a body whose columns no longer hash to its digest flags the search index
// instead of trusting the delete values, and the step-5 whole-index rebuild
// clears the stale postings: raw MATCH finds the surviving terms exactly
// once and the production query stays clean.
func TestSweepCorruptBodyRebuildsIndex(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	sid := gcSession(t, s, "b9b9b9b9-b9b9-49b9-89b9-b9b9b9b9b9b9")
	oldTerms := []string{"gccorrupto0", "gccorrupto1", "gccorrupto2"}
	activeTerms := []string{"gccorruptn0", "gccorruptn1", "gccorruptn2"}
	old, oldBlobs := gcActivate(t, s, sid, "gc_corrupt_old", oldTerms, nil)
	active, activeBlobs := gcActivate(t, s, sid, "gc_corrupt_active", activeTerms, nil)

	// Corrupt one superseded body under the immutability trigger (dropped
	// and recreated here; the trigger DDL is pinned by schema_v62.go): its
	// columns no longer hash to its stored digest. Bodies are
	// generation-scoped only by reference, so the corrupt write addresses
	// the old body by its digest and leaves the active bodies intact.
	oldPrepared, err := prepareHarmonizedCandidate(sid, old.Generation, oldBlobs)
	if err != nil {
		t.Fatalf("prepare corrupt target: %v", err)
	}
	corruptDigest := string(oldPrepared.bodyDigests[0])
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DROP TRIGGER session_entry_bodies_immutable`, nil); err != nil {
		s.pool.Put(conn)
		t.Fatalf("drop immutability trigger: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entry_bodies SET content_preview = content_preview || 'corrupted' WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), corruptDigest},
	}); err != nil {
		s.pool.Put(conn)
		t.Fatalf("corrupt a body column: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `CREATE TRIGGER session_entry_bodies_immutable BEFORE UPDATE ON session_entry_bodies BEGIN SELECT RAISE(ABORT, 'session_entry_bodies rows are immutable; insert a new entry instead'); END`, nil); err != nil {
		s.pool.Put(conn)
		t.Fatalf("recreate immutability trigger: %v", err)
	}
	s.pool.Put(conn)

	got, err := s.SweepSession(ctx, sid)
	if err != nil {
		t.Fatalf("sweep with a corrupt body: %v", err)
	}
	if !got.Rebuilt {
		t.Fatal("sweep of an untrusted delete did not run the whole-index rebuild")
	}
	if got.BodiesDeleted != 3 {
		t.Fatalf("sweep deleted %d bodies, want all 3 old ones", got.BodiesDeleted)
	}
	if readSweepFlag(t, s, sid) {
		t.Fatal("sweep left the flag set after the rebuild")
	}
	assertGCDigestsEqual(t, s, sid, "corrupt-body-rebuilds-index", gcBodyDigests(t, sid, active, activeBlobs))
	for _, term := range activeTerms {
		if got := gcMatchCount(t, s, term); got != 1 {
			t.Fatalf("raw MATCH for surviving term %q returned %d rows, want exactly 1 after the rebuild", term, got)
		}
	}
	for _, term := range oldTerms {
		if got := gcMatchCount(t, s, term); got != 0 {
			t.Fatalf("raw MATCH for swept term %q returned %d rows, want none", term, got)
		}
	}
}

// runSweepMidSweepBatch proves a crash between the sweep's bounded batches
// (content_crash_seams mid-sweep-batch) leaves a partial, safe deletion
// with the flag set, and the next pass completes it: the remaining rows go,
// the flag clears, and raw MATCH finds only the committed terms.
func runSweepMidSweepBatch(t *testing.T) {
	t.Helper()
	s, _ := openGenerationStoreWith(t, WithWriteConfig(ingest.WriteConfig{BatchBytes: 64, BatchSessions: 64, SweepRows: 1}.WithDefaults(1)))
	ctx := context.Background()
	sid := gcSession(t, s, "c8c8c8c8-c8c8-48c8-88c8-c8c8c8c8c8c8")
	oldTerms := []string{"gcmidbatcho0", "gcmidbatcho1", "gcmidbatcho2"}
	activeTerms := []string{"gcmidbatchn0", "gcmidbatchn1", "gcmidbatchn2"}
	gcActivate(t, s, sid, "gc_midbatch_old", oldTerms, nil)
	active, activeBlobs := gcActivate(t, s, sid, "gc_midbatch_active", activeTerms, nil)

	var fired atomic.Int64
	contentSweepSeam = func(stage string) error {
		if stage == contentSweepSeamMidBatch && fired.Add(1) == 1 {
			return errors.New("injected crash mid-sweep-batch")
		}
		return nil
	}
	defer func() { contentSweepSeam = nil }()
	_, interrupted := s.SweepSession(ctx, sid)
	if interrupted == nil {
		t.Fatal("sweep across the mid-batch seam succeeded; the crash must interrupt it")
	}
	if !readSweepFlag(t, s, sid) {
		t.Fatal("interrupted sweep cleared the flag; the next pass must resume it")
	}
	// The interruption lands mid-step-1: some old catalog rows are gone
	// while the orphan bodies are all still stored.
	remainingMapping := gcGenerationRows(t, s, sid, "gc_midbatch_old", "session_generation_entries")
	if remainingMapping == 0 || remainingMapping == 3 {
		t.Fatalf("interrupted sweep left %d old mapping rows, want a partial pass (1 or 2)", remainingMapping)
	}
	if got := len(gcRemainingDigests(t, s, sid)); got != 6 {
		t.Fatalf("interrupted sweep left %d bodies, want all 6: the object steps never ran", got)
	}
	if got := visibleGeneration(t, s, sid); got != "gc_midbatch_active" {
		t.Fatalf("visible generation = %q after the interrupted sweep, want the active one", got)
	}

	contentSweepSeam = nil
	got, err := s.SweepSession(ctx, sid)
	if err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if got.RowsDeleted != 7-1 || got.BodiesDeleted != 3 {
		t.Fatalf("retry sweep = %+v, want the remaining 6 rows and 3 bodies", got)
	}
	if readSweepFlag(t, s, sid) {
		t.Fatal("retry sweep left the flag set")
	}
	assertGCDigestsEqual(t, s, sid, "mid-sweep-batch", gcBodyDigests(t, sid, active, activeBlobs))
	for _, term := range activeTerms {
		if got := gcMatchCount(t, s, term); got == 0 {
			t.Fatalf("raw MATCH for surviving term %q returned no rows", term)
		}
	}
	for _, term := range oldTerms {
		if got := gcMatchCount(t, s, term); got != 0 {
			t.Fatalf("raw MATCH for swept term %q returned %d rows, want none", term, got)
		}
	}
}

// gcGenerationRows counts one generation's rows in one table.
func gcGenerationRows(t *testing.T, s *Store, sid schema.SessionID, generationID, table string) int64 {
	t.Helper()
	return countGenerationRows(t, s, table, sid, generationID)
}

// TestContentConcurrencySweepVsStage proves the sweep-vs-stage contract
// (content_concurrency sweep-vs-stage): a sweep that deletes
// staged-but-uncommitted objects surfaces as a commit refusal, and the
// retry re-stages and commits cleanly. Lock-free staging and the locked
// sweep serialize only through the commit's foreign keys, which fail
// closed.
func TestContentConcurrencySweepVsStage(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	sid := gcSession(t, s, "d9d9d9d9-d9d9-49d9-89d9-d9d9d9d9d9d9")
	terms := []string{"gcstageo0", "gcstageo1", "gcstageo2"}
	v2, blobs := gcBuildCandidate(t, sid, "gc_stage_gen", gcTermTexts("gc_stage_gen", terms), nil)
	filled := filledCandidateForValidation(t, v2, blobs)
	if _, err := s.StageGeneration(ctx, GenerationActivation{Generation: filled, Blobs: blobs}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	swept, err := s.SweepSession(ctx, sid)
	if err != nil {
		t.Fatalf("sweep of staged objects: %v", err)
	}
	if swept.BodiesDeleted != 3 {
		t.Fatalf("sweep deleted %d staged bodies, want 3", swept.BodiesDeleted)
	}
	// The commit runs without re-staging, straight at the swept store: the
	// mapping foreign key refuses the missing bodies.
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: sid, Result: filled, IndexVersion: 2,
		IndexerVersion: 1, IndexedAtMs: 1,
		RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 1,
		},
	}})
	if len(results) != 1 {
		t.Fatalf("batch results = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("commit over swept staging succeeded; the missing bodies must refuse it")
	}
	if !strings.Contains(results[0].Err.Error(), "FOREIGN KEY") {
		t.Fatalf("commit refusal = %v, want the foreign-key failure", results[0].Err)
	}
	if got := visibleGeneration(t, s, sid); got != "" {
		t.Fatalf("visible generation = %q after the refused commit, want nothing committed", got)
	}
	// The retry re-stages and commits cleanly, and its sweep settles.
	if err := activateTestGeneration(t, s, v2, blobs); err != nil {
		t.Fatalf("retry after the refused commit: %v", err)
	}
	final, err := s.SweepSession(ctx, sid)
	if err != nil {
		t.Fatalf("settle sweep: %v", err)
	}
	if final.BodiesDeleted != 0 || readSweepFlag(t, s, sid) {
		t.Fatalf("settle sweep = %+v with flag set, want clean and clear", final)
	}
	if got := visibleGeneration(t, s, sid); got != "gc_stage_gen" {
		t.Fatalf("visible generation = %q after retry, want gc_stage_gen", got)
	}
}

// TestContentConcurrencyHarvestVsReclaim proves the harvest-vs-reclaim
// contract (content_concurrency harvest-vs-reclaim): an activation racing a
// reclaim pass serializes on the per-session lock, so every order ends with
// the committed generation, zero orphans, and a clear flag.
func TestContentConcurrencyHarvestVsReclaim(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	sid := gcSession(t, s, "e0e0e0e0-e0e0-40e0-80e0-e0e0e0e0e0e0")
	gcActivate(t, s, sid, "gc_race_g1", []string{"gcraceo0", "gcraceo1", "gcraceo2"}, nil)
	active, activeBlobs := gcBuildCandidate(t, sid, "gc_race_g2", gcTermTexts("gc_race_g2", []string{"gcracen0", "gcracen1", "gcracen2"}), nil)
	filled := filledCandidateForValidation(t, active, activeBlobs)

	activated := make(chan error, 1)
	go func() {
		_, err := s.ActivateGeneration(ctx, GenerationActivation{
			Generation: filled, Blobs: activeBlobs, IndexerVersion: 1, IndexedAtMs: 1,
		})
		activated <- err
	}()
	if _, err := s.ReclaimSupersededGenerations(ctx, 0); err != nil {
		t.Fatalf("reclaim racing activation: %v", err)
	}
	if err := <-activated; err != nil {
		// The reclaim swept staged-but-uncommitted objects mid-flight.
		// The commit refuses fail-closed — at staging verification when
		// the bodies are already gone, or at the mapping foreign key when
		// the sweep lands between verification and commit — and the next
		// harvest retries. The retry re-stages over the settled store and
		// must succeed.
		if !strings.Contains(err.Error(), "is missing after staging") &&
			!strings.Contains(err.Error(), "FOREIGN KEY") {
			t.Fatalf("activation racing reclaim refused unexpectedly: %v", err)
		}
		t.Logf("first activation refused on swept staging (the designed race): %v", err)
		if err := activateTestGeneration(t, s, active, activeBlobs); err != nil {
			t.Fatalf("retry activation racing reclaim: %v", err)
		}
	}
	swept, err := s.SweepSession(ctx, sid)
	if err != nil {
		t.Fatalf("settle sweep: %v", err)
	}
	_ = swept
	if got := visibleGeneration(t, s, sid); got != "gc_race_g2" {
		t.Fatalf("visible generation = %q after the race, want gc_race_g2", got)
	}
	if readSweepFlag(t, s, sid) {
		t.Fatal("race left the flag set after the settle sweep")
	}
	assertGCDigestsEqual(t, s, sid, "harvest-vs-reclaim", gcBodyDigests(t, sid, active, activeBlobs))
}

// TestContentConcurrencyHarvestVsMigrate proves the harvest-vs-migrate
// contract (content_concurrency harvest-vs-migrate) at the lock both
// writers share: while the migration holds the session (here the test
// itself, the way migrate Phase 2 does), an activation waits past its
// context instead of interleaving, and succeeds once the holder releases.
func TestContentConcurrencyHarvestVsMigrate(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "f1f1f1f1-f1f1-41f1-81f1-f1f1f1f1f1f1")
	release, err := s.sessionLocker.LockExclusive(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	v2, blobs := gcBuildCandidate(t, sid, "gc_migrate_race", gcTermTexts("gc_migrate_race", []string{"gcmigr0", "gcmigr1", "gcmigr2"}), nil)
	filled := filledCandidateForValidation(t, v2, blobs)
	waitCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := s.ActivateGeneration(waitCtx, GenerationActivation{
		Generation: filled, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1,
	}); err == nil {
		_ = release()
		t.Fatal("activation during the migration's lock succeeded; writers must serialize")
	} else if !strings.Contains(err.Error(), "no lock was taken") {
		_ = release()
		t.Fatalf("contended activation refused without the lock error: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := activateTestGeneration(t, s, v2, blobs); err != nil {
		t.Fatalf("activation after the migration released: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gc_migrate_race" {
		t.Fatalf("visible generation = %q, want gc_migrate_race", got)
	}
}

// TestContentOneCopySupersededCopyGoneAfterSweep proves the one-copy twin
// (content_one_copy superseded-copy-gone-after-sweep): after the sweep no
// table holds a row for the superseded generation, the shared bodies exist
// exactly once, and the activation wrote no generation files.
func TestContentOneCopySupersededCopyGoneAfterSweep(t *testing.T) {
	s, root := openGenerationStore(t)
	ctx := context.Background()
	sid := gcSession(t, s, "a2a2a2a2-a2a2-42a2-82a2-a2a2a2a2a2a2")
	oldTexts := gcTermTexts("gc_copy_old", []string{"gccopyo0", "gccopyo1", "gccopyo2"})
	gcActivateTexts(t, s, sid, "gc_copy_old", oldTexts, nil, false)
	activeTexts := gcTermTexts("gc_copy_active", []string{"gccopyn0", "gccopyn1", "gccopyn2"})
	activeTexts[0] = oldTexts[0]
	active, activeBlobs := gcActivateTexts(t, s, sid, "gc_copy_active", activeTexts, nil, false)
	got, err := s.SweepSession(ctx, sid)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got.RowsDeleted != 7 || got.BodiesDeleted != 2 {
		t.Fatalf("sweep = %+v, want the old copy's 7 rows and 2 bodies", got)
	}
	for _, table := range reclaimTableNames {
		if remaining := countGenerationRows(t, s, table, sid, "gc_copy_old"); remaining != 0 {
			t.Fatalf("%s still holds %d superseded row(s)", table, remaining)
		}
	}
	assertGCDigestsEqual(t, s, sid, "superseded-copy-gone-after-sweep", gcBodyDigests(t, sid, active, activeBlobs))
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "generations" {
			t.Fatalf("activation wrote a generation directory at %s; content lives in rows, not files", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReclaimSelectsByFlag proves the reclaim twin
// (content_cli_surface reclaim-selects-by-flag): candidates come through
// the partial flag index, so a session with superseded rows but a clear
// flag is never a candidate, while flagged sessions with rows or staged
// orphans are reclaimed.
func TestReclaimSelectsByFlag(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	flagged := gcSession(t, s, "b3b3b3b3-b3b3-43b3-83b3-b3b3b3b3b3b3")
	gcActivate(t, s, flagged, "gc_selflag_old", []string{"gcselflago0", "gcselflago1", "gcselflago2"}, nil)
	gcActivate(t, s, flagged, "gc_selflag_active", []string{"gcselflagn0", "gcselflagn1", "gcselflagn2"}, nil)

	unflagged := gcSession(t, s, "c4c4c4c4-c4c4-44c4-84c4-c4c4c4c4c4c4")
	gcActivate(t, s, unflagged, "gc_unflag_old", []string{"gcunflago0", "gcunflago1", "gcunflago2"}, nil)
	gcActivate(t, s, unflagged, "gc_unflag_active", []string{"gcunflagn0", "gcunflagn1", "gcunflagn2"}, nil)
	execGenerationSQL(t, s, `UPDATE sessions SET content_sweep_pending = 0 WHERE session_id = 'c4c4c4c4-c4c4-44c4-84c4-c4c4c4c4c4c4';`)
	unflaggedRows := nonActiveReclaimCounts(t, s, unflagged, "gc_unflag_active")
	if unflaggedRows.Total() == 0 {
		t.Fatal("unflagged session holds no superseded rows; the selection proves nothing")
	}

	staged := gcSession(t, s, "d5d5d5d5-d5d5-45d5-85d5-d5d5d5d5d5d5")
	gcActivate(t, s, staged, "gc_staged_g1", []string{"gcstagedo0", "gcstagedo1", "gcstagedo2"}, nil)
	stagedV2, stagedBlobs := gcBuildCandidate(t, staged, "gc_staged_g2", gcTermTexts("gc_staged_g2", []string{"gcstagedn0", "gcstagedn1", "gcstagedn2"}), nil)
	stagedFilled := filledCandidateForValidation(t, stagedV2, stagedBlobs)
	if _, err := s.StageGeneration(ctx, GenerationActivation{Generation: stagedFilled, Blobs: stagedBlobs}); err != nil {
		t.Fatalf("stage: %v", err)
	}

	plan, err := s.PlanSupersededGenerationReclaim(ctx, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, session := range plan.Sessions {
		if session.SessionID == unflagged {
			t.Fatal("flag-clear session with superseded rows is a reclaim candidate; selection runs through the flag")
		}
	}
	result, err := s.ReclaimSupersededGenerations(ctx, 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Sessions != 2 {
		t.Fatalf("reclaim sessions = %d, want the 2 flagged sessions", result.Sessions)
	}
	if got := nonActiveReclaimCounts(t, s, unflagged, "gc_unflag_active"); got != unflaggedRows {
		t.Fatalf("unflagged session rows changed across the reclaim: before=%+v after=%+v", unflaggedRows, got)
	}
	if readSweepFlag(t, s, flagged) || readSweepFlag(t, s, staged) {
		t.Fatal("flagged session kept its flag after the reclaim")
	}
	if readSweepFlag(t, s, unflagged) {
		t.Fatal("unflagged session gained a flag from a pass that must never touch it")
	}
}

// TestReclaimSweepsOrphans proves the reclaim twin
// (content_cli_surface reclaim-sweeps-orphans): reclaiming a harmonized
// session deletes its superseded rows, sweeps the orphan bodies and blob
// the row deletes uncover, reports the orphan counts, clears the flag,
// and leaves no stale search match.
func TestReclaimSweepsOrphans(t *testing.T) {
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	sid := gcSession(t, s, "e6e6e6e6-e6e6-46e6-86e6-e6e6e6e6e6e6")
	oldTerms := []string{"gcoreclamo0", "gcoreclamo1", "gcoreclamo2"}
	activeTerms := []string{"gcoreclamn0", "gcoreclamn1", "gcoreclamn2"}
	gcActivate(t, s, sid, "gc_reclaim_old", oldTerms, map[string]string{"e_gcblobReclaim": "gc reclaim orphan blob bytes"})
	active, activeBlobs := gcActivate(t, s, sid, "gc_reclaim_active", activeTerms, nil)
	blobDigest := gcDigestOf([]byte("gc reclaim orphan blob bytes"))
	if got := gcBlobChunks(t, s, sid, blobDigest); got != 1 {
		t.Fatalf("seeded %d chunks for the orphan blob, want 1", got)
	}

	result, err := s.ReclaimSupersededGenerations(ctx, 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Sessions != 1 || result.Generations != 1 {
		t.Fatalf("reclaim = %d sessions, %d generations, want 1 and 1", result.Sessions, result.Generations)
	}
	if result.Rows.Total() != 10 {
		t.Fatalf("reclaim rows total = %d, want the old generation's 10", result.Rows.Total())
	}
	if result.BodiesDeleted != 3 || result.BlobsDeleted != 1 {
		t.Fatalf("reclaim orphans = %d bodies, %d blobs, want 3 and 1", result.BodiesDeleted, result.BlobsDeleted)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("reclaim warnings = %v, want none", result.Warnings)
	}
	assertGCDigestsEqual(t, s, sid, "reclaim-sweeps-orphans", gcBodyDigests(t, sid, active, activeBlobs))
	if got := gcCount(t, s, "session_content", sid); got != 0 {
		t.Fatalf("%d blob objects remain, want none", got)
	}
	if got := gcBlobChunks(t, s, sid, blobDigest); got != 0 {
		t.Fatalf("%d chunks survive the swept blob, want none", got)
	}
	if readSweepFlag(t, s, sid) {
		t.Fatal("reclaimed session kept its flag")
	}
	for _, term := range activeTerms {
		if got := gcMatchCount(t, s, term); got == 0 {
			t.Fatalf("raw MATCH for surviving term %q returned no rows", term)
		}
	}
	for _, term := range oldTerms {
		if got := gcMatchCount(t, s, term); got != 0 {
			t.Fatalf("raw MATCH for reclaimed term %q returned %d rows, want none", term, got)
		}
	}
}

// TestReclaimHarmonizedDirectoryOnlyRetry proves the harmonized
// directory-only retry: an orphan directory with no rows behind it is
// removed with no row deletes, and the session settles with its flag
// clear.
func TestReclaimHarmonizedDirectoryOnlyRetry(t *testing.T) {
	s, root := openGenerationStore(t)
	ctx := context.Background()
	sid := gcSession(t, s, "f7f7f7f7-f7f7-47f7-87f7-f7f7f7f7f7f7")
	gcActivate(t, s, sid, "gc_dironly_active", []string{"gcdironly0", "gcdironly1", "gcdironly2"}, nil)
	orphanID := "gc_dironly_orphan"
	staged, stagedBlobs := gcBuildCandidate(t, sid, orphanID, []string{"gc dironly orphan zero", "gc dironly orphan one", "gc dironly orphan two"}, nil)
	encoded, err := marshalGenerationForDir(t, filledCandidateForValidation(t, staged, stagedBlobs))
	if err != nil {
		t.Fatal(err)
	}
	genDir := filepath.Join(root, string(sid), "generations", orphanID)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := s.ReclaimSupersededGenerations(ctx, 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Sessions != 1 || result.Generations != 1 {
		t.Fatalf("reclaim = %d sessions, %d generations, want the 1 orphan directory", result.Sessions, result.Generations)
	}
	if result.Rows.Total() != 0 {
		t.Fatalf("reclaim rows total = %d, want no row deletes", result.Rows.Total())
	}
	if result.DirectoriesRemoved != 1 {
		t.Fatalf("directories removed = %d, want the 1 orphan", result.DirectoriesRemoved)
	}
	if _, err := os.Stat(genDir); !os.IsNotExist(err) {
		t.Fatal("orphan directory survived the retry")
	}
	if readSweepFlag(t, s, sid) {
		t.Fatal("session kept its flag after the directory-only retry")
	}
}

// marshalGenerationForDir encodes one filled candidate for an orphan
// directory manifest: the ownership proof the cleanup path verifies.
func marshalGenerationForDir(t *testing.T, v2 indexformat.V2) ([]byte, error) {
	t.Helper()
	encoded, err := json.Marshal(v2.Generation)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// TestReclaimTableInventoryMatchesFormatDelete pins the reclaim alignment:
// the shared generation-scoped inventory holds exactly the seventeen
// tables, every one has a report field, and the representation replacement
// clears every one of them behaviorally — reclaim never leaves rows for a
// new table.
func TestReclaimTableInventoryMatchesFormatDelete(t *testing.T) {
	want := []string{
		"session_context_segment_refs",
		"session_context_segments",
		"session_generation_associations",
		"session_generation_commits",
		"session_generation_content",
		"session_generation_diagnostics",
		"session_generation_entries",
		"session_generation_subagents",
		"session_generation_title_refs",
		"session_generations",
		"session_projection_aliases",
		"session_projection_content",
		"session_projection_entries",
		"session_projection_generations",
		"session_projection_sections",
		"session_relationship_evidence",
		"session_section_native_metadata",
	}
	got := append([]string(nil), reclaimTableNames...)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("reclaim inventory holds %d tables, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reclaim inventory = %v, want exactly %v", got, want)
		}
	}
	var counts ReclaimTableCounts
	for _, table := range reclaimTableNames {
		if reclaimCountField(&counts, table) == nil {
			t.Fatalf("table %s has no reclaim report field; every inventoried table must be counted", table)
		}
	}

	// Behavioral alignment: the format Delete clears every inventoried
	// table for a replaced session, so no new table keeps rows behind.
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	sid := gcSession(t, s, "a8a8a8a8-a8a8-48a8-88a8-a8a8a8a8a8a8")
	gcActivate(t, s, sid, "gc_align_old", []string{"gcaligno0", "gcaligno1", "gcaligno2"}, map[string]string{"e_gcblobAlign": "gc alignment blob bytes"})
	gcActivate(t, s, sid, "gc_align_active", []string{"gcalignmentn0", "gcalignmentn1", "gcalignmentn2"}, nil)
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := (generationIndexFormat{}).Delete(ctx, conn, sid); err != nil {
		s.pool.Put(conn)
		t.Fatalf("representation replacement delete: %v", err)
	}
	s.pool.Put(conn)
	for _, table := range reclaimTableNames {
		if remaining := gcCount(t, s, table, sid); remaining != 0 {
			t.Fatalf("replacement left %d row(s) in %s; the delete must clear every inventoried table", remaining, table)
		}
	}
}
