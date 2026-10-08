package store

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/generation_diagnostic_safety.yaml
var generationDiagnosticSafetyYAML []byte

//go:embed testdata/generation_diagnostic_safety.manifest.yaml
var generationDiagnosticSafetyManifestYAML []byte

type diagnosticSafetyFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		CompleteID  string `yaml:"complete_id"`
		CandidateID string `yaml:"candidate_id"`
		InstalledID string `yaml:"installed_id"`
	} `yaml:"generation"`
	PrivateIdentity string `yaml:"private_identity"`
	Cases           []struct {
		Name     string `yaml:"name"`
		Boundary string `yaml:"boundary"`
	} `yaml:"cases"`
}

func loadDiagnosticSafetyFixture(t *testing.T) diagnosticSafetyFixture {
	t.Helper()
	var fixture diagnosticSafetyFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(generationDiagnosticSafetyYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_diagnostic_safety.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationDiagnosticSafetyManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_diagnostic_safety manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "generation diagnostic safety"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestGenerationDiagnosticSafety proves the activation, installed-candidate
// verification and recovery boundaries refuse an untrusted candidate whose
// detail carries a private path without echoing that path. Each refusal keeps
// the last-good generation and its success stamps as the read authority, and
// the installed or staged candidate bytes are retained unchanged.
func TestGenerationDiagnosticSafety(t *testing.T) {
	fixture := loadDiagnosticSafetyFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			seedGenerationSession(t, s, fixture.Session.ID)
			complete, completeBlobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, "diag text G1", "diag input G1", "diag output G1")
			if err := activateTestGeneration(t, s, complete, completeBlobs); err != nil {
				t.Fatalf("activate G1: %v", err)
			}
			before := readIndexStateForTest(t, s, id)

			switch tc.Boundary {
			case "activation":
				// A staged candidate whose title reference is a private path
				// fails generation validation after staging. The refusal must
				// not echo the path and the last-good generation must remain the
				// read authority.
				candidate, candidateBlobs := buildTestGeneration(t, id, fixture.Generation.CandidateID, "diag text G2", "diag input G2", "diag output G2")
				candidate.Generation.TitleRefs = []schema.SourceEntryRef{schema.SourceEntryRef(fixture.PrivateIdentity)}
				activationErr := activateTestGeneration(t, s, candidate, candidateBlobs)
				assertPrivateDetailRefused(t, activationErr, fixture.PrivateIdentity, "activation")
				assertLastGoodRetained(t, s, id, before, fixture.Generation.CompleteID)
			case "installed-candidate":
				// Install a candidate, then restage the same identifier with
				// a mutated content reference (a private path) and a cleared
				// digest. The immutable identifier refuses the collision
				// without echoing the path and without rewriting the
				// installed rows.
				installed, installedBlobs := buildTestGeneration(t, id, fixture.Generation.InstalledID, "diag text installed", "diag input installed", "diag output installed")
				if err := activateTestGeneration(t, s, installed, installedBlobs); err != nil {
					t.Fatalf("activate installed candidate: %v", err)
				}
				installedState := readIndexStateForTest(t, s, id)
				beforeInstalled := countMappingRows(t, s, id, fixture.Generation.InstalledID)
				collision, collisionBlobs := buildTestGeneration(t, id, fixture.Generation.InstalledID, "diag text retry", "diag input retry", "diag output retry")
				collision.Generation.Content[0].Ref = schema.SourceEntryRef(fixture.PrivateIdentity)
				collision.Generation.Content[0].Digest = ""
				restageErr := activateTestGeneration(t, s, collision, collisionBlobs)
				assertPrivateDetailRefused(t, restageErr, fixture.PrivateIdentity, "installed-candidate verification")
				if got := countMappingRows(t, s, id, fixture.Generation.InstalledID); got != beforeInstalled {
					t.Fatal("refused restage changed the installed mapping rows; they must be unchanged")
				}
				assertLastGoodRetained(t, s, id, installedState, fixture.Generation.InstalledID)
			case "recovery":
				// Interrupt a real activation before the commit, then retry
				// with a mutated content reference (a private path) and a
				// cleared digest. The retry must refuse the unverifiable
				// binding without echoing the path and keep the last-good
				// generation; the interrupted staging wrote no generation
				// row, so there is nothing to replay.
				installHarmonizedFault(t, harmonizedSeamBeforeCommit)
				failed, failedBlobs := buildTestGeneration(t, id, fixture.Generation.CandidateID, "diag text G2", "diag input G2", "diag output G2")
				if err := activateTestGeneration(t, s, failed, failedBlobs); err == nil {
					t.Fatal("activation across the crash seam succeeded; expected interruption")
				}
				clearHarmonizedFault()
				mutated, mutatedBlobs := buildTestGeneration(t, id, fixture.Generation.CandidateID, "diag text G2", "diag input G2", "diag output G2")
				mutated.Generation.Content[0].Ref = schema.SourceEntryRef(fixture.PrivateIdentity)
				mutated.Generation.Content[0].Digest = ""
				_, recoveryErr := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     mutated,
					Blobs:          mutatedBlobs,
					IndexerVersion: 1,
					IndexedAtMs:    1,
				})
				assertPrivateDetailRefused(t, recoveryErr, fixture.PrivateIdentity, "recovery")
				assertLastGoodRetained(t, s, id, before, fixture.Generation.CompleteID)
				if rowPresent(t, s, id, fixture.Generation.CandidateID) {
					t.Fatal("refused retry wrote a generation row; nothing must be installed")
				}
			case "recovery-replay":
				// A refused activation stages objects but writes no
				// generation row and records no intent. A later attempt with
				// the same invalid candidate must refuse again without
				// echoing the private title reference it carries.
				candidate, candidateBlobs := buildTestGeneration(t, id, fixture.Generation.CandidateID, "diag text G2", "diag input G2", "diag output G2")
				candidate.Generation.TitleRefs = []schema.SourceEntryRef{schema.SourceEntryRef(fixture.PrivateIdentity)}
				if err := activateTestGeneration(t, s, candidate, candidateBlobs); err == nil {
					t.Fatal("activation of a candidate with a private title ref succeeded; it must be refused")
				}
				_, replayErr := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     candidate,
					Blobs:          candidateBlobs,
					IndexerVersion: 1,
					IndexedAtMs:    1,
				})
				assertPrivateDetailRefused(t, replayErr, fixture.PrivateIdentity, "recovery replay")
				assertLastGoodRetained(t, s, id, before, fixture.Generation.CompleteID)
			default:
				t.Fatalf("unknown generation diagnostic boundary %q; add it to the fixture, the required-names manifest and this runner", tc.Boundary)
			}
		})
	}
}

// assertPrivateDetailRefused proves the boundary rejected the candidate and
// that its diagnostic never echoed the private path carried by the untrusted
// candidate detail.
func assertPrivateDetailRefused(t *testing.T, err error, privateIdentity, boundary string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted a candidate whose untrusted detail carries a private path; it must be refused", boundary)
	}
	if strings.Contains(err.Error(), privateIdentity) {
		t.Fatalf("%s refusal echoed the private path: %v", boundary, err)
	}
}

// assertLastGoodRetained proves the refused candidate left the prior generation
// as the read authority with its success stamps unchanged.
func assertLastGoodRetained(t *testing.T, s *Store, id schema.SessionID, before *ingest.SessionIndexState, generationID string) {
	t.Helper()
	if got := visibleGeneration(t, s, id); got != generationID {
		t.Fatalf("visible generation = %q after refused candidate, want last-good %q", got, generationID)
	}
	after := readIndexStateForTest(t, s, id)
	if after.IndexerVersion != before.IndexerVersion {
		t.Fatalf("refused candidate changed index_version from %d to %d", before.IndexerVersion, after.IndexerVersion)
	}
	if (before.IndexedAt == nil) != (after.IndexedAt == nil) || (before.IndexedAt != nil && *before.IndexedAt != *after.IndexedAt) {
		t.Fatalf("refused candidate changed indexed_at from %v to %v", before.IndexedAt, after.IndexedAt)
	}
}
