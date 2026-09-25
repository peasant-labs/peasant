package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// TestGenerationEnvelopeRecovery proves recovery replays the persisted
// activation envelope: the producing revision and time, the captured
// compare-and-swap state, the input proof and the content-capture evidence. A
// stale candidate stays inactive for a verified retry instead of bypassing the
// refusal, and an already-committed candidate is repaired from committed
// database metadata.
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

	// Capture the current state for G2; the envelope must carry it.
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
	// Crash after the rename but before the database commit: the intent and
	// staged candidate survive, the database still shows G1.
	installRecoveryFault(t, s, "after-rename-before-db")
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
	clearRecoveryFault(t, s, "after-rename-before-db")

	// The persisted envelope carries the original revision, time, expected
	// state and capture evidence, not the G1 stamps.
	intent, err := s.generationArtifacts.ReadIntent(context.Background(), sid)
	if err != nil || intent == nil {
		t.Fatalf("pending intent missing: %+v (err %v)", intent, err)
	}
	if intent.IndexerVersion != 77 || intent.IndexedAtMs != 777 {
		t.Fatalf("intent stamps = (%d,%d), want (77,777)", intent.IndexerVersion, intent.IndexedAtMs)
	}
	if intent.ExpectedState == nil || intent.ExpectedState.IndexerVersion != 11 {
		t.Fatalf("intent expected state missing G1 revision: %+v", intent.ExpectedState)
	}
	if intent.ContentCapture.Status != ingest.ContentCaptureIncomplete || intent.ContentCapture.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly {
		t.Fatalf("intent capture = %+v, want incomplete/preview_only", intent.ContentCapture)
	}

	// Recovery replays the same guarded transaction and settles on G2 with
	// the original stamps, versions and capture eligibility.
	if _, err := s.RecoverGenerationActivation(context.Background(), sid); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_env_g2" {
		t.Fatalf("after recovery visible = %q, want gen_env_g2", got)
	}
	after := readIndexStateForTest(t, s, sid)
	if after.IndexerVersion != 77 || after.IndexedAt == nil || *after.IndexedAt != 777 {
		t.Fatalf("recovered stamps = (%d,%v), want (77,777)", after.IndexerVersion, after.IndexedAt)
	}
	err = s.WithSessionSnapshot(context.Background(), sid, func(snapshot indexformat.ReadSnapshot) error {
		if snapshot.Session.StartTime.UnixMilli() != 3000 || snapshot.Session.EndTime.UnixMilli() != 4000 {
			return fmt.Errorf("snapshot timestamps = (%d,%d), want (3000,4000)", snapshot.Session.StartTime.UnixMilli(), snapshot.Session.EndTime.UnixMilli())
		}
		for _, entry := range snapshot.Main.Entries {
			if entry.ContentPreview != nil && strings.Contains(*entry.ContentPreview, "G2") {
				return nil
			}
		}
		return fmt.Errorf("recovered snapshot carries no G2 content")
	})
	if err != nil {
		t.Fatal(err)
	}
	if intent, err := s.generationArtifacts.ReadIntent(context.Background(), sid); err != nil || intent != nil {
		t.Fatalf("intent not cleared after recovery: %+v (err %v)", intent, err)
	}
}

// TestGenerationStaleRecoveryRefused proves a candidate whose compare-and-swap
// precondition failed is not committed by a later recovery: it stays inactive
// pending a verified retry with current state.
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

	// Attempt G2 with the stale precondition: the activation is refused and
	// the synced candidate is retained, but recovery must refuse it again
	// rather than bypassing the stale comparison.
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
	// The failed activation staged its candidate and recorded its intent
	// before the guarded transaction refused it. Recovery replays the same
	// stale envelope and is refused again; the prior generation stays visible.
	if _, err := s.RecoverGenerationActivation(context.Background(), sid); err == nil {
		t.Fatal("stale recovery succeeded; it must stay inactive pending a verified retry")
	} else if !isStaleError(err) {
		t.Fatalf("stale recovery error is not a stale refusal: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_stale_mid" {
		t.Fatalf("after stale recovery visible = %q, want mid", got)
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
