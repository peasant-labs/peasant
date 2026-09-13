package store

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/generation_id_safety.yaml
var generationIDSafetyYAML []byte

//go:embed testdata/generation_id_safety.manifest.yaml
var generationIDSafetyManifestYAML []byte

//go:embed testdata/generation_completeness.yaml
var generationCompletenessYAML []byte

//go:embed testdata/generation_completeness.manifest.yaml
var generationCompletenessManifestYAML []byte

//go:embed testdata/generation_mirrors.yaml
var generationMirrorsYAML []byte

//go:embed testdata/generation_mirrors.manifest.yaml
var generationMirrorsManifestYAML []byte

//go:embed testdata/generation_filesystem_safety.yaml
var generationFilesystemSafetyYAML []byte

//go:embed testdata/generation_filesystem_safety.manifest.yaml
var generationFilesystemSafetyManifestYAML []byte

type idSafetyFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		CompleteID string `yaml:"complete_id"`
		FailedID   string `yaml:"failed_id"`
	} `yaml:"generation"`
	Cases []struct {
		Name         string `yaml:"name"`
		GenerationID string `yaml:"generation_id"`
	} `yaml:"cases"`
}

func loadIDSafetyFixture(t *testing.T) idSafetyFixture {
	t.Helper()
	var fixture idSafetyFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(generationIDSafetyYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_id_safety.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationIDSafetyManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_id_safety manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "generation identifier safety"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestGenerationIDValidation rejects dot, dotdot and non-canonical generation
// identifiers before any filesystem operation, preserves the active generation
// on cleanup refusal, preserves intent-owned candidates, and refuses immutable
// identifier collisions unless the staged candidate is identical.
func TestGenerationIDValidation(t *testing.T) {
	fixture := loadIDSafetyFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, root := openGenerationStore(t)
			seedGenerationSession(t, s, fixture.Session.ID)
			complete, completeBlobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, "safe text G1", "safe input G1", "safe output G1")
			if err := activateTestGeneration(t, s, complete, completeBlobs); err != nil {
				t.Fatalf("activate G1: %v", err)
			}

			switch tc.Name {
			case "intent-owned-cleanup-preserved":
				// Leave a pending intent for G2 via a crash before the commit,
				// then prove cleanup of the intent-owned candidate is refused
				// and the candidate survives for recovery.
				installRecoveryFault(t, s, "after-rename-before-db")
				failed, failedBlobs := buildTestGeneration(t, id, fixture.Generation.FailedID, "safe text G2", "safe input G2", "safe output G2")
				if err := activateTestGeneration(t, s, failed, failedBlobs); err == nil {
					t.Fatal("activation across crash seam succeeded; expected interruption")
				}
				clearRecoveryFault(t, s, "after-rename-before-db")
				if err := s.CleanupInactiveGeneration(context.Background(), id, fixture.Generation.FailedID); err == nil {
					t.Fatal("cleanup removed an intent-owned candidate; it must be preserved for recovery")
				}
				if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID {
					t.Fatalf("after refused cleanup visible = %q, want G1", got)
				}
				if _, err := s.generationArtifacts.ReadManifest(context.Background(), id, fixture.Generation.FailedID); err != nil {
					t.Fatalf("intent-owned candidate manifest missing after refused cleanup: %v", err)
				}
				if err := s.RecoverGenerationActivation(context.Background(), id); err != nil {
					t.Fatalf("recover intent-owned candidate: %v", err)
				}
				if got := visibleGeneration(t, s, id); got != fixture.Generation.FailedID {
					t.Fatalf("after recovery visible = %q, want G2", got)
				}
			case "immutable-collision-refused":
				// A different candidate reusing the installed identifier must
				// be refused; the installed generation survives.
				colliding, collidingBlobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, "different text", "different input", "different output")
				colliding.Generation.SourceEvidenceDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				// Force a different digest by changing the text; the same ID
				// with different evidence is an immutable collision.
				if err := s.generationArtifacts.WriteIntent(context.Background(), GenerationIntent{
					SessionID:    id,
					GenerationID: colliding.Generation.ID,
					ManifestPath: "generations/" + colliding.Generation.ID + "/manifest.json",
					Completeness: string(colliding.Generation.Completeness),
				}); err != nil {
					t.Fatalf("write collision intent: %v", err)
				}
				if _, err := s.generationArtifacts.Stage(context.Background(), colliding.Generation, collidingBlobs); err == nil {
					_ = s.generationArtifacts.ClearIntent(context.Background(), id)
					t.Fatal("staging an immutable collision succeeded; it must be refused")
				}
				_ = s.generationArtifacts.ClearIntent(context.Background(), id)
				if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID {
					t.Fatalf("after refused collision visible = %q, want G1", got)
				}
				manifestPath := filepath.Join(root, fixture.Session.ID, "generations", fixture.Generation.CompleteID, "manifest.json")
				if _, err := os.Stat(manifestPath); err != nil {
					t.Fatalf("installed generation manifest missing after refused collision: %v", err)
				}
			default:
				// Dot, dotdot and non-canonical identifiers are rejected
				// before any filesystem operation; the active generation and
				// its manifest survive.
				if err := s.CleanupInactiveGeneration(context.Background(), id, tc.GenerationID); err == nil {
					t.Fatalf("cleanup of %q succeeded; dot and non-canonical identifiers must be rejected", tc.GenerationID)
				}
				if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID {
					t.Fatalf("after refused cleanup visible = %q, want G1", got)
				}
				manifestPath := filepath.Join(root, fixture.Session.ID, "generations", fixture.Generation.CompleteID, "manifest.json")
				if _, err := os.Stat(manifestPath); err != nil {
					t.Fatalf("active generation manifest missing after refused cleanup of %q: %v", tc.GenerationID, err)
				}
				// Staging the same identifier is also refused.
				bad := complete
				bad.Generation.ID = tc.GenerationID
				if tc.GenerationID != "" {
					if _, err := s.generationArtifacts.Stage(context.Background(), bad.Generation, completeBlobs); err == nil {
						t.Fatalf("staging of %q succeeded; it must be rejected", tc.GenerationID)
					}
				}
			}
		})
	}
}

type completenessFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		CompleteID       string `yaml:"complete_id"`
		IncompleteID     string `yaml:"incomplete_id"`
		SecondCompleteID string `yaml:"second_complete_id"`
	} `yaml:"generation"`
	Cases []struct {
		Name      string `yaml:"name"`
		Candidate string `yaml:"candidate"`
		Prior     string `yaml:"prior"`
		Allowed   bool   `yaml:"allowed"`
	} `yaml:"cases"`
}

func loadCompletenessFixture(t *testing.T) completenessFixture {
	t.Helper()
	var fixture completenessFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(generationCompletenessYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_completeness.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationCompletenessManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_completeness manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "generation completeness"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func buildIncompleteGeneration(t *testing.T, sid schema.SessionID, genID, text, toolInput, toolOutput string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	v2, blobs := buildTestGeneration(t, sid, genID, text, toolInput, toolOutput)
	v2.Generation.Completeness = indexformat.GenerationCompletenessIncompleteNew
	return v2, blobs
}

// TestGenerationCompletenessTransition proves incomplete_new never replaces a
// complete last-good generation, that success and indexed-at stamps stay unset
// for incomplete candidates, and that the same guard runs for direct format
// writes.
func TestGenerationCompletenessTransition(t *testing.T) {
	fixture := loadCompletenessFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			seedGenerationSession(t, s, fixture.Session.ID)

			activateComplete := func(genID, suffix string, version int, at int64) error {
				v2, blobs := buildTestGeneration(t, id, genID, "text "+suffix, "input "+suffix, "output "+suffix)
				return s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     v2,
					Blobs:          blobs,
					IndexerVersion: version,
					IndexedAtMs:    at,
				})
			}
			activateIncomplete := func(genID, suffix string, version int, at int64) error {
				v2, blobs := buildIncompleteGeneration(t, id, genID, "text "+suffix, "input "+suffix, "output "+suffix)
				return s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     v2,
					Blobs:          blobs,
					IndexerVersion: version,
					IndexedAtMs:    at,
				})
			}

			switch tc.Name {
			case "first-incomplete-allowed":
				if err := activateIncomplete(fixture.Generation.IncompleteID, "first-incomplete", 77, 777); err != nil {
					t.Fatalf("first incomplete activation was refused: %v", err)
				}
				if got := visibleGeneration(t, s, id); got != fixture.Generation.IncompleteID {
					t.Fatalf("visible = %q, want incomplete G2", got)
				}
			case "complete-replaces-incomplete-allowed":
				if err := activateIncomplete(fixture.Generation.IncompleteID, "prior-incomplete", 0, 0); err != nil {
					t.Fatalf("prior incomplete: %v", err)
				}
				if err := activateComplete(fixture.Generation.SecondCompleteID, "complete-replaces", 9, 999); err != nil {
					t.Fatalf("complete replacing incomplete was refused: %v", err)
				}
				if got := visibleGeneration(t, s, id); got != fixture.Generation.SecondCompleteID {
					t.Fatalf("visible = %q, want complete G3", got)
				}
			case "incomplete-replaces-complete-refused":
				if err := activateComplete(fixture.Generation.CompleteID, "prior-complete", 5, 500); err != nil {
					t.Fatalf("prior complete: %v", err)
				}
				if err := activateIncomplete(fixture.Generation.IncompleteID, "incomplete-over-complete", 77, 777); err == nil {
					t.Fatal("incomplete replacing a complete generation succeeded; it must be refused")
				}
				if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID {
					t.Fatalf("after refusal visible = %q, want complete G1", got)
				}
				// A direct format write with a positive stamp for an
				// incomplete candidate is also refused.
				v2, _ := buildIncompleteGeneration(t, id, "gen_direct_incomplete", "text", "input", "output")
				results := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{
					SessionID:      id,
					Result:         v2,
					IndexVersion:   2,
					Mode:           ingest.SessionEntryWriteReplaceAll,
					ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourceNone, CaptureFormat: ingest.ContentCaptureFormatPreviewOnly},
					IndexerVersion: 3,
					IndexedAtMs:    333,
				}})
				if len(results) != 1 || results[0].Err == nil {
					t.Fatal("direct incomplete write with a positive stamp succeeded; it must be refused")
				}
			case "incomplete-stamps-unset":
				// Caller-supplied positive stamps for an incomplete candidate
				// are suppressed: success stays unset and maintenance stays
				// required.
				if err := activateIncomplete(fixture.Generation.IncompleteID, "stamps-unset", 77, 777); err != nil {
					t.Fatalf("incomplete activation: %v", err)
				}
				stamps := readIndexStateForTest(t, s, id)
				if stamps.IndexerVersion != 0 {
					t.Fatalf("incomplete activation stamped index_version %d, want unset", stamps.IndexerVersion)
				}
				if stamps.IndexedAt != nil {
					t.Fatalf("incomplete activation stamped indexed_at %d, want unset", *stamps.IndexedAt)
				}
				stale, err := s.ListStaleIndexSessions(context.Background(), map[ingest.Harness]ingest.HarvesterVersions{
					ingest.Harness("claude-code"): {IndexerVersion: 1},
				})
				if err != nil {
					t.Fatalf("list stale: %v", err)
				}
				found := false
				for _, sid := range stale {
					if string(sid) == fixture.Session.ID {
						found = true
					}
				}
				if !found {
					t.Fatal("incomplete session is not stale; maintenance must stay required")
				}
			case "complete-stamps-recorded":
				if err := activateComplete(fixture.Generation.CompleteID, "stamps-recorded", 9, 999); err != nil {
					t.Fatalf("complete activation: %v", err)
				}
				stamps := readIndexStateForTest(t, s, id)
				if stamps.IndexerVersion != 9 {
					t.Fatalf("complete activation index_version = %d, want 9", stamps.IndexerVersion)
				}
				if stamps.IndexedAt == nil || *stamps.IndexedAt != 999 {
					t.Fatalf("complete activation indexed_at = %v, want 999", stamps.IndexedAt)
				}
			}
		})
	}
}

func readIndexStateForTest(t *testing.T, s *Store, sid schema.SessionID) *ingest.SessionIndexState {
	t.Helper()
	state, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatalf("ReadIndexState: %v", err)
	}
	if state == nil {
		t.Fatal("no index state; session metadata row is missing")
	}
	return state
}
