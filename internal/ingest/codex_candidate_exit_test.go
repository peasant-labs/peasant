package ingest_test

// Fixture-backed validation for the registered Codex candidate exit's safe
// refusal boundary. Every case drives the real indexer entry point over real
// OS-backed native input (or a real dependency failure) and asserts that the
// private locator, the native identity, and the wrapped cause never reach the
// refusal, while the validated harness/session identity and the fixed
// operation and effect categories do.

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_candidate_exit.yaml
var codexCandidateExitYAML []byte

//go:embed testdata/codex_candidate_exit.manifest.yaml
var codexCandidateExitManifestYAML []byte

type codexCandidateExitCase struct {
	Name                      string `yaml:"name"`
	SessionID                 string `yaml:"sessionID"`
	GenerationID              string `yaml:"generationID"`
	SourceFileName            string `yaml:"sourceFileName"`
	MissingSource             bool   `yaml:"missingSource"`
	Source                    string `yaml:"source"`
	PriorFailure              bool   `yaml:"priorFailure"`
	PrivatePathSentinel       string `yaml:"privatePathSentinel"`
	PrivateIdentitySentinel   string `yaml:"privateIdentitySentinel"`
	PrivateSessionSentinel    string `yaml:"privateSessionSentinel"`
	PrivateDependencySentinel string `yaml:"privateDependencySentinel"`
	Expected                  struct {
		Operation      string `yaml:"operation"`
		Harness        string `yaml:"harness"`
		SessionID      string `yaml:"sessionID"`
		EffectContains string `yaml:"effectContains"`
	} `yaml:"expected"`
}

type codexCandidateExitFixtureDoc struct {
	Cases []codexCandidateExitCase `yaml:"cases"`
}

func loadCodexCandidateExitFixture(t *testing.T) codexCandidateExitFixtureDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(codexCandidateExitYAML))
	decoder.KnownFields(true)
	var fixture codexCandidateExitFixtureDoc
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode codex candidate exit fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("codex candidate exit fixture must contain exactly one YAML document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(codexCandidateExitManifestYAML, "codex candidate exit")
	if err != nil {
		t.Fatalf("load codex candidate exit manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			t.Fatal("codex candidate exit fixture has an empty case name")
		}
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "codex candidate exit"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestCodexCandidateExitRefusalsAreSafe drives the registered indexer exit and
// proves a refusal never carries the native source locator, an unvalidated raw
// identity, a wrapped dependency cause, or the path-bearing V1 completion
// diagnostic. It still names the validated harness/session identity plus the
// fixed operation, effect, and recovery categories.
func TestCodexCandidateExitRefusalsAreSafe(t *testing.T) {
	for _, testCase := range loadCodexCandidateExitFixture(t).Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, testCase.SourceFileName)
			if !testCase.MissingSource {
				if err := os.WriteFile(path, []byte(testCase.Source), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			session := ingest.DiscoveredSession{
				SessionID:  schema.SessionID(testCase.SessionID),
				Harness:    ingest.HarnessCodex,
				SourcePath: ingest.ResolvedPath(path),
			}
			config := ingest.CodexProvenanceIndexerConfig{
				Enabled:      true,
				GenerationID: func(ingest.DiscoveredSession) string { return testCase.GenerationID },
			}
			if testCase.PriorFailure {
				config.Prior = func(ingest.DiscoveredSession) (*schema.UnifiedMetadata, ingest.ProjectionPriorState, error) {
					return nil, ingest.NewProjectionPriorState(), fmt.Errorf("the retained-state dependency failed for %s", testCase.PrivateDependencySentinel)
				}
			}
			indexer := ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexProvenanceCapture(config))
			result, err := indexer.IndexTranscriptResult(context.Background(), session)
			if err == nil {
				t.Fatalf("candidate exit accepted the case: result=%#v", result)
			}
			if result != nil {
				t.Fatalf("refusal returned a result: %#v", result)
			}
			message := err.Error()
			if !strings.HasPrefix(message, "ingest."+testCase.Expected.Operation+":") {
				t.Errorf("refusal operation is not the fixed %q category: %s", testCase.Expected.Operation, message)
			}
			identity := "harness " + testCase.Expected.Harness + " session " + testCase.Expected.SessionID + " "
			if !strings.Contains(message, identity) {
				t.Errorf("refusal does not name the validated identity %q: %s", identity, message)
			}
			if !strings.Contains(message, testCase.Expected.EffectContains) {
				t.Errorf("refusal does not name the fixed effect category %q: %s", testCase.Expected.EffectContains, message)
			}
			if !strings.Contains(message, "retry harvest") {
				t.Errorf("refusal does not name its safe recovery: %s", message)
			}
			if strings.Contains(message, "could not verify complete input") {
				t.Errorf("refusal kept the path-bearing V1 completion wrapper: %s", message)
			}
			sentinels := map[string]string{
				"private path":       testCase.PrivatePathSentinel,
				"private identity":   testCase.PrivateIdentitySentinel,
				"private session":    testCase.PrivateSessionSentinel,
				"private dependency": testCase.PrivateDependencySentinel,
			}
			for label, sentinel := range sentinels {
				if sentinel != "" && strings.Contains(message, sentinel) {
					t.Errorf("refusal leaked the %s sentinel %q: %s", label, sentinel, message)
				}
			}
			if strings.Contains(message, path) {
				t.Errorf("refusal leaked the native source locator %q: %s", path, message)
			}
		})
	}
}
