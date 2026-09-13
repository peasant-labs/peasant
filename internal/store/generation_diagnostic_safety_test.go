package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
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
			s, root := openGenerationStore(t)
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
				// An installed inactive candidate whose content reference is a
				// private path and whose digest is cleared cannot be bound, so
				// restaging the same identifier is refused without echoing the
				// path and without rewriting the installed bytes.
				installed, installedBlobs := buildTestGeneration(t, id, fixture.Generation.InstalledID, "diag text installed", "diag input installed", "diag output installed")
				if _, err := s.generationArtifacts.Stage(context.Background(), installed.Generation, installedBlobs); err != nil {
					t.Fatalf("stage installed candidate: %v", err)
				}
				manifestPath := filepath.Join(root, fixture.Session.ID, "generations", fixture.Generation.InstalledID, "manifest.json")
				installedBytes, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatalf("read installed manifest: %v", err)
				}
				var decoded indexformat.Generation
				if err := json.Unmarshal(installedBytes, &decoded); err != nil {
					t.Fatalf("decode installed manifest: %v", err)
				}
				decoded.Content[0].Ref = schema.SourceEntryRef(fixture.PrivateIdentity)
				decoded.Content[0].Digest = ""
				mutated, err := json.Marshal(decoded)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manifestPath, mutated, 0o600); err != nil {
					t.Fatal(err)
				}
				collision, collisionBlobs := buildTestGeneration(t, id, fixture.Generation.InstalledID, "diag text retry", "diag input retry", "diag output retry")
				restageErr := activateTestGeneration(t, s, collision, collisionBlobs)
				assertPrivateDetailRefused(t, restageErr, fixture.PrivateIdentity, "installed-candidate verification")
				afterBytes, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatalf("installed manifest missing after refused restage: %v", err)
				}
				if string(afterBytes) != string(mutated) {
					t.Fatal("refused restage rewrote the installed candidate bytes; they must be unchanged")
				}
				assertLastGoodRetained(t, s, id, before, fixture.Generation.CompleteID)
			case "recovery":
				// Interrupt a real activation after the atomic rename, then
				// rewrite only the staged manifest's content reference to a
				// private path and clear its digest. Recovery must refuse the
				// unverifiable binding without echoing the path, keep the
				// last-good generation, and retain the candidate and intent.
				installRecoveryFault(t, s, "after-rename-before-db")
				failed, failedBlobs := buildTestGeneration(t, id, fixture.Generation.CandidateID, "diag text G2", "diag input G2", "diag output G2")
				if err := activateTestGeneration(t, s, failed, failedBlobs); err == nil {
					t.Fatal("activation across the crash seam succeeded; expected interruption")
				}
				clearRecoveryFault(t, s, "after-rename-before-db")
				manifestPath := filepath.Join(root, fixture.Session.ID, "generations", fixture.Generation.CandidateID, "manifest.json")
				manifest, err := s.generationArtifacts.ReadManifest(context.Background(), id, fixture.Generation.CandidateID)
				if err != nil {
					t.Fatalf("read staged candidate manifest: %v", err)
				}
				manifest.Content[0].Ref = schema.SourceEntryRef(fixture.PrivateIdentity)
				manifest.Content[0].Digest = ""
				data, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
				recoveryErr := s.RecoverGenerationActivation(context.Background(), id)
				assertPrivateDetailRefused(t, recoveryErr, fixture.PrivateIdentity, "recovery")
				assertLastGoodRetained(t, s, id, before, fixture.Generation.CompleteID)
				retained, err := s.generationArtifacts.ReadManifest(context.Background(), id, fixture.Generation.CandidateID)
				if err != nil {
					t.Fatalf("staged candidate was not retained after refused recovery: %v", err)
				}
				if retained.ID != fixture.Generation.CandidateID {
					t.Fatalf("retained candidate manifest = %q, want %q", retained.ID, fixture.Generation.CandidateID)
				}
				pending, err := s.generationArtifacts.ReadIntent(context.Background(), id)
				if err != nil {
					t.Fatalf("read pending intent after refused recovery: %v", err)
				}
				if pending == nil || pending.GenerationID != fixture.Generation.CandidateID {
					t.Fatalf("pending intent was not retained after refused recovery: %+v", pending)
				}
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
