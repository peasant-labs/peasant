package ingest_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/managed_generation_validation.yaml
var validationFixtures []byte

//go:embed testdata/managed_generation_validation.manifest.yaml
var validationManifest []byte

func TestValidationOutcomeBoundary(t *testing.T) {
	for _, want := range ingest.AllValidationOutcomes {
		got, err := ingest.NewValidationOutcome(string(want))
		if err != nil || got != want {
			t.Fatalf("outcome %q: got %q, %v", want, got, err)
		}
	}
	const unknown = "unexpected-validation-value"
	if _, err := ingest.NewValidationOutcome(unknown); err == nil || !strings.Contains(err.Error(), unknown) {
		t.Fatalf("unknown outcome error = %v; want it to name %q", err, unknown)
	}
}

func TestManagedGenerationValidatorStub(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name                  string `yaml:"name"`
			SessionID             string `yaml:"sessionID"`
			GenerationID          string `yaml:"generationID"`
			InputDigest           string `yaml:"inputDigest"`
			IndexerTarget         int    `yaml:"indexerTarget"`
			FormatTarget          int    `yaml:"formatTarget"`
			MaxConcurrent         int    `yaml:"maxConcurrent"`
			ChunkSessions         int    `yaml:"chunkSessions"`
			MaxBytesPerValidation int64  `yaml:"maxBytesPerValidation"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(validationFixtures, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(validationManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	// A nil store proves the stub cannot take a connection or writer lane.
	var s *store.Store
	seen := make(map[string]bool)
	for _, tc := range fixture.Cases {
		if seen[tc.Name] {
			t.Fatalf("duplicate case %q", tc.Name)
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			request := ingest.ManagedGenerationValidationRequest{
				SessionID: ingest.SessionID(tc.SessionID), GenerationID: tc.GenerationID, InputDigest: tc.InputDigest,
				IndexerTarget: tc.IndexerTarget, FormatTarget: tc.FormatTarget, Budget: ingest.ValidationBudget{
					MaxConcurrent: tc.MaxConcurrent, ChunkSessions: tc.ChunkSessions, MaxBytesPerValidation: tc.MaxBytesPerValidation,
				}}
			result, err := s.ValidateManagedGenerationUnchanged(t.Context(), request)
			if err != nil || result.Outcome != ingest.ValidationUnsupported || result.Mismatch != ingest.UnchangedMismatchNone {
				t.Fatalf("validation = %+v, %v", result, err)
			}
		})
	}
	for _, name := range manifest.RequiredNames {
		if !seen[name] {
			t.Errorf("missing required case %q", name)
		}
	}
}

func TestValidationResultRejectsInconsistentMismatch(t *testing.T) {
	if _, err := ingest.NewManagedGenerationValidationResult(ingest.ValidationProven, ingest.UnchangedNotProvenSeal, ""); err == nil {
		t.Fatal("proven with mismatch was accepted")
	}
	if _, err := ingest.NewManagedGenerationValidationResult(ingest.ValidationNotProven, ingest.UnchangedMismatchNone, ""); err == nil {
		t.Fatal("not-proven without mismatch was accepted")
	}
}

func TestValidationProfileFoldsAttempts(t *testing.T) {
	var profiler ingest.IndexProfiler
	profiler.RecordStage(ingest.StageRefreshValidate, 0, 0, 1)
	profiler.RecordStage(ingest.StageRefreshValidate, 0, 0, 1)
	profiler.RecordStage(ingest.StageRefreshValidateUnsupported, 0, 0, 1)
	profiler.RecordStage(ingest.StageRefreshValidateUnsupported, 0, 0, 1)
	found := 0
	for _, stage := range profiler.StageTotals() {
		if stage.Stage == ingest.StageRefreshValidate || stage.Stage == ingest.StageRefreshValidateUnsupported {
			if stage.Done != 0 || stage.Total != 2 {
				t.Fatalf("folded validation stage = %+v", stage)
			}
			found++
		}
	}
	if found != 2 {
		t.Fatalf("found %d validation stages, want two", found)
	}
}
