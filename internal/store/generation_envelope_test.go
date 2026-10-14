package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestGenerationEnvelopeRecovery proves an interrupted activation keeps
// the caller's envelope intact for the retry: the producing revision and
// time, the captured compare-and-swap state, the input proof and the
// content-capture evidence all ride the retried activation (nothing is
// persisted between attempts), and the retry settles on G2 with the
// original stamps, versions and capture eligibility. A crash between
// staging and the commit leaves the old generation, the old stamps, and
// possibly orphan objects with the flag set; the retry re-stages
// idempotently and commits.
func TestGenerationEnvelopeRecovery(t *testing.T) {
	sid, err := schema.NewSessionID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))

	// G1 establishes the last-good generation with known stamps.
	g1, g1Blobs := buildTestGeneration(t, sid, "gen_env_g1", "env text G1", "env input G1", "env output G1")
	g1.Generation.Metadata.Timestamp.Start = 1000
	g1.Generation.Metadata.Timestamp.End = 2000
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     g1,
		Blobs:          g1Blobs,
		IndexerVersion: 11,
		IndexedAtMs:    111,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:          ingest.ContentCaptureIncomplete,
			SourceAuthority: ingest.ContentSourceNone,
			CaptureFormat:   ingest.ContentCaptureFormatPreviewOnly,
		},
	}); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	before := readIndexStateForTest(t, s, sid)
	if before.IndexerVersion != 11 || before.IndexedAt == nil || *before.IndexedAt != 111 {
		t.Fatalf("G1 stamps = (%d,%v), want (11,111)", before.IndexerVersion, before.IndexedAt)
	}

	// Capture the current state for G2; the retry must carry it.
	current, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	g2, g2Blobs := buildTestGeneration(t, sid, "gen_env_g2", "env text G2", "env input G2", "env output G2")
	g2.Generation.Metadata.Timestamp.Start = 3000
	g2.Generation.Metadata.Timestamp.End = 4000
	capture := ingest.SessionContentCaptureWrite{
		Status:          ingest.ContentCaptureIncomplete,
		SourceAuthority: ingest.ContentSourceNone,
		CaptureFormat:   ingest.ContentCaptureFormatPreviewOnly,
	}
	// Crash between staging and the commit: the objects may be staged but
	// no generation row exists, so the database still shows G1.
	installHarmonizedFault(t, harmonizedSeamBeforeCommit)
	failedActivation := GenerationActivation{
		Generation:     g2,
		Blobs:          g2Blobs,
		IndexerVersion: 77,
		IndexedAtMs:    777,
		ExpectedState:  current,
		ContentCapture: capture,
	}
	if _, err := s.ActivateGeneration(context.Background(), failedActivation); err == nil {
		t.Fatal("activation across crash seam succeeded; expected interruption")
	}
	clearHarmonizedFault()
	if got := visibleGeneration(t, s, sid); got != "gen_env_g1" {
		t.Fatalf("after interruption visible = %q, want G1", got)
	}

	// The retry carries the original revision, time, expected state and
	// capture evidence, and settles on G2 with those stamps.
	if _, err := s.ActivateGeneration(context.Background(), failedActivation); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_env_g2" {
		t.Fatalf("after retry visible = %q, want gen_env_g2", got)
	}
	after := readIndexStateForTest(t, s, sid)
	if after.IndexerVersion != 77 || after.IndexedAt == nil || *after.IndexedAt != 777 {
		t.Fatalf("retried stamps = (%d,%v), want (77,777)", after.IndexerVersion, after.IndexedAt)
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var start, end int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT ts_start, ts_end FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), "gen_env_g2"},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			start, end = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if start != 3000 || end != 4000 {
		t.Fatalf("committed timestamps = (%d,%d), want (3000,4000)", start, end)
	}
}

// TestGenerationStaleRecoveryRefused proves a candidate whose compare-and-swap
// precondition failed is never committed by a later attempt with the same
// stale envelope: it stays inactive pending a verified retry with current
// state. The refused attempt stages objects but writes no generation row,
// so a second stale attempt is refused again instead of finding phantom
// authority.
func TestGenerationStaleRecoveryRefused(t *testing.T) {
	sid, err := schema.NewSessionID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))

	g1, g1Blobs := buildTestGeneration(t, sid, "gen_stale_g1", "stale text G1", "stale input G1", "stale output G1")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: g1, Blobs: g1Blobs, IndexerVersion: 5, IndexedAtMs: 500}); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	staleState, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	// Advance the state so the captured precondition goes stale: activate an
	// intermediate complete generation.
	gMid, gMidBlobs := buildTestGeneration(t, sid, "gen_stale_mid", "stale text mid", "stale input mid", "stale output mid")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: gMid, Blobs: gMidBlobs, IndexerVersion: 6, IndexedAtMs: 600}); err != nil {
		t.Fatalf("activate mid: %v", err)
	}

	// Attempt G2 with the stale precondition: the activation is refused.
	g2, g2Blobs := buildTestGeneration(t, sid, "gen_stale_g2", "stale text G2", "stale input G2", "stale output G2")
	staleActivation := GenerationActivation{
		Generation:     g2,
		Blobs:          g2Blobs,
		IndexerVersion: 7,
		IndexedAtMs:    700,
		ExpectedState:  staleState,
		ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourceNone, CaptureFormat: ingest.ContentCaptureFormatPreviewOnly},
	}
	if _, err := s.ActivateGeneration(context.Background(), staleActivation); err == nil {
		t.Fatal("stale activation succeeded; it must be refused")
	} else if !isStaleError(err) {
		t.Fatalf("stale activation error is not a stale refusal: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_stale_mid" {
		t.Fatalf("after stale refusal visible = %q, want mid", got)
	}
	// A second attempt with the same stale envelope is refused again: the
	// refused staging wrote no generation row, so there is nothing to
	// replay and the prior generation stays visible.
	if _, err := s.ActivateGeneration(context.Background(), staleActivation); err == nil {
		t.Fatal("second stale activation succeeded; it must stay inactive pending a verified retry")
	} else if !isStaleError(err) {
		t.Fatalf("second stale error is not a stale refusal: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_stale_mid" {
		t.Fatalf("after second stale refusal visible = %q, want mid", got)
	}

	// A verified retry with current state commits.
	current, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	retry := staleActivation
	retry.ExpectedState = current
	if _, err := s.ActivateGeneration(context.Background(), retry); err != nil {
		t.Fatalf("verified retry: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_stale_g2" {
		t.Fatalf("after verified retry visible = %q, want G2", got)
	}
}
func isStaleError(err error) bool {
	if err == nil {
		return false
	}
	var stale *ingest.StaleIndexWorkError
	if asStale(err, &stale) {
		return true
	}
	return strings.Contains(err.Error(), "changed before replacement") || strings.Contains(err.Error(), "Stale")
}

func asStale(err error, target **ingest.StaleIndexWorkError) bool {
	return errors.As(err, target)
}
