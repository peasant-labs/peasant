package store

import (
	"context"
	_ "embed"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_crash_seams.yaml
var contentCrashSeamsYAML []byte

//go:embed testdata/content_crash_seams.manifest.yaml
var contentCrashSeamsManifestYAML []byte

type contentCrashSeamCase struct {
	Name                   string `yaml:"name"`
	Seam                   string `yaml:"seam,omitempty"`
	BatchBytes             int64  `yaml:"batch_bytes,omitempty"`
	ExpectedSweepFlag      *bool  `yaml:"expected_sweep_flag,omitempty"`
	ExpectedStagedBodies   *int64 `yaml:"expected_staged_bodies,omitempty"`
	ExpectedBatchCommitted *bool  `yaml:"expected_batch_committed,omitempty"`
}

type contentCrashSeamFixtures struct {
	AbandonedMarker  string                 `yaml:"abandonedMarker"`
	RecoveryMarker   string                 `yaml:"recoveryMarker"`
	BatchSessionIDs  []string               `yaml:"batchSessionIDs"`
	EarlierSessionID string                 `yaml:"earlierSessionID"`
	Cases            []contentCrashSeamCase `yaml:"cases"`
}

func loadContentCrashSeamFixtures(t *testing.T) contentCrashSeamFixtures {
	t.Helper()
	var fixtures contentCrashSeamFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(contentCrashSeamsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode content_crash_seams.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentCrashSeamsManifestYAML)
	if err != nil {
		t.Fatalf("decode content_crash_seams manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "content crash seams"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestContentCrashSeamsFixtureManifest pins the crash-seam case inventory:
// the loader compiles, the manifest loads, and every required name is
// present with no undeclared extra.
func TestContentCrashSeamsFixtureManifest(t *testing.T) {
	t.Parallel()
	loadContentCrashSeamFixtures(t)
}

// crashCandidate builds one small two-entry candidate for the seam matrix:
// small enough to stage in one transaction under the shipped budget, big
// enough to split under the tiny budget the between-transactions cases use.
func crashCandidate(t *testing.T, sid schema.SessionID, genID, marker string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	return buildTestGeneration(t, sid, genID, "crash text "+marker, "crash input "+marker, "crash output "+marker)
}

// TestContentCrashSeams drives the writer through its crash seams at small
// scale: interrupt G2 at the seam, assert exactly the old (or, past the
// commit, the new) generation is visible with the old stamps, then recover
// by retry and prove detail parity on the committed rows.
func TestContentCrashSeams(t *testing.T) {
	fixtures := loadContentCrashSeamFixtures(t)
	for _, tc := range fixtures.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			switch tc.Name {
			case "mid-activation-batch":
				runCrashMidActivationBatch(t, fixtures, tc)
				return
			case "savepoint-isolation":
				runCrashSavepointIsolation(t)
				return
			case "mid-sweep-batch":
				runSweepMidSweepBatch(t)
				return
			}
			if tc.Seam == "" {
				t.Fatalf("%s carries no seam; name the writer seam it interrupts", tc.Name)
			}
			var s *Store
			if tc.BatchBytes > 0 {
				s, _ = openGenerationStoreWith(t, WithWriteConfig(ingest.WriteConfig{BatchBytes: tc.BatchBytes, BatchSessions: 64}.WithDefaults(1)))
			} else {
				s, _ = openGenerationStore(t)
			}
			sid, err := schema.NewSessionID("d3d3d3d3-d3d3-43d3-83d3-d3d3d3d3d3d3")
			if err != nil {
				t.Fatal(err)
			}
			seedGenerationSession(t, s, string(sid))
			g1, g1Blobs := crashCandidate(t, sid, "gen_crash_g1", "G1")
			if err := activateTestGeneration(t, s, g1, g1Blobs); err != nil {
				t.Fatalf("activate G1: %v", err)
			}
			if _, err := s.SweepSession(context.Background(), sid); err != nil {
				t.Fatalf("finish G1 sweep before interruption: %v", err)
			}
			before := readIndexStateForTest(t, s, sid)

			installHarmonizedFault(t, tc.Seam)
			g2, g2Blobs := crashCandidate(t, sid, "gen_crash_g2", fixtures.AbandonedMarker)
			interrupted := activateTestGeneration(t, s, g2, g2Blobs)
			clearHarmonizedFault()
			if interrupted == nil {
				// The between-transactions seam only fires when staging
				// splits: without a split there is nothing to interrupt.
				if tc.Name == "between-stage-txns" {
					t.Fatalf("activation across seam %s succeeded; the tiny budget must split staging", tc.Seam)
				}
				t.Fatalf("activation across seam %s succeeded; a crash seam must interrupt it", tc.Seam)
			}

			want := "gen_crash_g1"
			if tc.Name == "after-commit-before-sweep" {
				want = "gen_crash_g2"
			}
			if got := visibleGeneration(t, s, sid); got != want {
				t.Fatalf("after seam %s visible = %q, want %q", tc.Seam, got, want)
			}
			if want == "gen_crash_g1" {
				after := readIndexStateForTest(t, s, sid)
				if after.IndexerVersion != before.IndexerVersion {
					t.Fatalf("interrupted activation stamped index_version %d, want the prior %d", after.IndexerVersion, before.IndexerVersion)
				}
			}
			if tc.ExpectedSweepFlag != nil {
				if got := readSweepFlag(t, s, sid); got != *tc.ExpectedSweepFlag {
					t.Fatalf("after seam %s sweep flag = %v, want %v", tc.Seam, got, *tc.ExpectedSweepFlag)
				}
			} else if tc.BatchBytes > 0 && !readSweepFlag(t, s, sid) {
				t.Fatalf("after seam %s the sweep flag is unset; the first staging transaction must set it", tc.Seam)
			}
			if tc.ExpectedStagedBodies != nil {
				got := queryMigrateInt(t, s, `SELECT COUNT(*) FROM session_entry_bodies b WHERE b.session_id = '`+string(sid)+`' AND NOT EXISTS (SELECT 1 FROM session_generation_entries e WHERE e.session_id=b.session_id AND e.body_digest=b.body_digest)`)
				if got != *tc.ExpectedStagedBodies {
					t.Fatalf("after seam %s new staged bodies = %d, want %d", tc.Seam, got, *tc.ExpectedStagedBodies)
				}
			}

			recovered, recoveredBlobs := crashCandidate(t, sid, "gen_crash_g2b", fixtures.RecoveryMarker)
			if err := activateTestGeneration(t, s, recovered, recoveredBlobs); err != nil {
				t.Fatalf("retry after seam %s: %v", tc.Seam, err)
			}
			if got := visibleGeneration(t, s, sid); got != "gen_crash_g2b" {
				t.Fatalf("after recovery visible = %q, want the different recovery candidate", got)
			}
			assertHarmonizedContent(t, s, sid, "gen_crash_g2b", recovered, recoveredBlobs)
			if _, warnings, err := s.SweepFlaggedSessionsForHarvest(context.Background()); err != nil || len(warnings) != 0 {
				t.Fatalf("recover flagged sweep: %v", err)
			}
			if readSweepFlag(t, s, sid) {
				t.Fatal("recovery sweep left its flag set")
			}
			if orphanContentCount(t, s, sid) != 0 {
				t.Fatal("recovery left unreferenced bodies or blobs")
			}
			assertSearchIndexMatchesNothing(t, s, "after different-candidate crash recovery", fixtures.AbandonedMarker)
		})
	}
}

// runCrashMidActivationBatch proves process loss aborts the outer batch:
// neither session commits, while a previously committed batch remains intact.
func runCrashMidActivationBatch(t *testing.T, fixtures contentCrashSeamFixtures, tc contentCrashSeamCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	missingExpectation := tc.ExpectedBatchCommitted == nil || tc.ExpectedSweepFlag == nil
	if missingExpectation {
		t.Fatal("outer rollback fixture needs batch-commit and sweep-flag expectations")
	}
	if *tc.ExpectedBatchCommitted || len(fixtures.BatchSessionIDs) < 2 {
		t.Fatal("outer rollback fixture needs at least two session IDs and expected_batch_committed false")
	}
	earlierID := gcSession(t, s, fixtures.EarlierSessionID)
	earlier, earlierBlobs := crashCandidate(t, earlierID, "gen_earlier", "earlierstable")
	if err := activateTestGeneration(t, s, earlier, earlierBlobs); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	harmonizedWriterSeam = func(stage string) error {
		if stage == harmonizedSeamMidActivationBatch && calls.Add(1) == 2 {
			return context.DeadlineExceeded
		}
		return nil
	}
	defer clearHarmonizedFault()
	sessions := make([]schema.SessionID, 0, 2)
	activations := make([]GenerationActivation, 0, len(fixtures.BatchSessionIDs))
	for i, raw := range fixtures.BatchSessionIDs {
		sid, err := schema.NewSessionID(raw)
		if err != nil {
			t.Fatal(err)
		}
		seedGenerationSession(t, s, string(sid))
		v2, blobs := crashCandidate(t, sid, "gen_batch", fixtures.AbandonedMarker+" "+string(rune('1'+i)))
		filled := filledCandidateForValidation(t, v2, blobs)
		if _, err := s.StageGeneration(ctx, GenerationActivation{Generation: filled, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1}); err != nil {
			t.Fatalf("stage session %d: %v", i, err)
		}
		sessions = append(sessions, sid)
		activations = append(activations, GenerationActivation{Generation: filled, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1})
	}
	results := s.ActivateGenerationBatch(ctx, activations)
	if len(results) != len(activations) {
		t.Fatalf("batch results = %d, want %d", len(results), len(activations))
	}
	for i, sid := range sessions {
		if results[i].Err == nil || results[i].Outcome.Disposition != ingest.ActivationNotCommitted {
			t.Fatalf("crashed batch session %d reported a commit: %+v", i, results[i])
		}
		if got := visibleGeneration(t, s, sid); got != "" {
			t.Fatalf("crashed batch session visible=%s; the outer transaction must roll back", got)
		}
		if readSweepFlag(t, s, sid) != *tc.ExpectedSweepFlag || orphanContentCount(t, s, sid) == 0 {
			t.Fatal("crashed batch lost its flag or durable staged orphans")
		}
	}
	assertHarmonizedContent(t, s, earlierID, "gen_earlier", earlier, earlierBlobs)
	clearHarmonizedFault()
	for _, sid := range sessions {
		v2, blobs := crashCandidate(t, sid, "gen_batch_recovered", fixtures.RecoveryMarker)
		if err := activateTestGeneration(t, s, v2, blobs); err != nil {
			t.Fatal(err)
		}
	}
	if _, warnings, err := s.SweepFlaggedSessionsForHarvest(ctx); err != nil || len(warnings) != 0 {
		t.Fatalf("crashed batch recovery: %v %v", err, warnings)
	}
	for _, sid := range sessions {
		if orphanContentCount(t, s, sid) != 0 || readSweepFlag(t, s, sid) {
			t.Fatal("crashed batch recovery did not clear all orphans and flags")
		}
	}
	assertSearchIndexMatchesNothing(t, s, "after outer-batch rollback and recovery", fixtures.AbandonedMarker)
}

// runCrashSavepointIsolation proves a hostile session rolls back to its own
// savepoint without discarding the rest of the batch: the good session
// commits while the malformed one is refused.
func runCrashSavepointIsolation(t *testing.T) {
	t.Helper()
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	goodID, err := schema.NewSessionID("a6a6a6a6-a6a6-46a6-86a6-a6a6a6a6a6a6")
	if err != nil {
		t.Fatal(err)
	}
	badID, err := schema.NewSessionID("b7b7b7b7-b7b7-47b7-87b7-b7b7b7b7b7b7")
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, string(goodID))
	seedGenerationSession(t, s, string(badID))
	good, goodBlobs := crashCandidate(t, goodID, "gen_good", "good")
	filledGood := filledCandidateForValidation(t, good, goodBlobs)
	if _, err := s.StageGeneration(ctx, GenerationActivation{Generation: filledGood, Blobs: goodBlobs, IndexerVersion: 1, IndexedAtMs: 1}); err != nil {
		t.Fatalf("stage good session: %v", err)
	}
	bad, _ := crashCandidate(t, badID, "gen_bad", "bad")
	filledBad := filledCandidateForValidation(t, bad, nil)
	badEntry := schema.SessionEntry{
		SessionID: badID, EntryIndex: 99, Harness: defaults.HarnessClaudeCode,
		EntryType: ingest.EntryTypeText, Role: ingest.RoleUser,
	}
	broken := "{not json"
	badEntry.Extra = &broken
	filledBad.Generation.Main.Entries = append(filledBad.Generation.Main.Entries, badEntry)
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{
		{SessionID: goodID, Result: filledGood, IndexVersion: 2, IndexerVersion: 1, IndexedAtMs: 1},
		{SessionID: badID, Result: filledBad, IndexVersion: 2, IndexerVersion: 1, IndexedAtMs: 1},
	})
	if len(results) != 2 {
		t.Fatalf("batch results = %d, want 2", len(results))
	}
	if results[0].Err != nil || !results[0].Written {
		t.Fatalf("good batch write: %+v", results[0])
	}
	if results[1].Err == nil {
		t.Fatal("hostile batch write succeeded; it must be refused")
	}
	if got := visibleGeneration(t, s, goodID); got != "gen_good" {
		t.Fatalf("good session visible = %q, want its generation", got)
	}
	if got := visibleGeneration(t, s, badID); got != "" {
		t.Fatalf("hostile session visible = %q, want nothing committed", got)
	}
}
