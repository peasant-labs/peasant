package transcript

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/model_projection.yaml
var modelProjectionFixtureYAML []byte

//go:embed testdata/model_projection.manifest.yaml
var modelProjectionManifestYAML []byte

type modelProjectionEntry struct {
	Index         int    `yaml:"index"`
	Role          string `yaml:"role"`
	Depth         int    `yaml:"depth"`
	ParentIndex   *int   `yaml:"parentIndex,omitempty"`
	Content       string `yaml:"content"`
	ObservedModel string `yaml:"observedModel,omitempty"`
}

type modelProjectionCase struct {
	Name                   string                 `yaml:"name"`
	Assertions             []string               `yaml:"assertions"`
	InputPath              string                 `yaml:"inputPath"`
	StoredModel            string                 `yaml:"storedModel"`
	Entries                []modelProjectionEntry `yaml:"entries"`
	ExpectedSeed           string                 `yaml:"expectedSeed"`
	ExpectedObservedModels []string               `yaml:"expectedObservedModels"`
}

type modelProjectionFixture struct {
	Cases []modelProjectionCase `yaml:"cases"`
}

type modelProjectionResult struct {
	Name     string
	Failures []string
}

func loadModelProjectionFixture(t *testing.T) modelProjectionFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(modelProjectionFixtureYAML))
	decoder.KnownFields(true)
	var fixture modelProjectionFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode model projection fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("model projection fixture must contain exactly one YAML document: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(modelProjectionManifestYAML, "model projection")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if fixtureCase.Name == "" || len(fixtureCase.Assertions) == 0 || (fixtureCase.InputPath != "fold" && fixtureCase.InputPath != "detail") || fixtureCase.StoredModel == "" || fixtureCase.ExpectedSeed == "" || len(fixtureCase.Entries) == 0 || len(fixtureCase.ExpectedObservedModels) != len(fixtureCase.Entries) {
			t.Fatalf("model projection fixture case %q is incomplete", fixtureCase.Name)
		}
		for _, entry := range fixtureCase.Entries {
			role := schema.Role(entry.Role)
			if !role.IsValid() {
				t.Fatalf("model projection fixture case %q has invalid role %q", fixtureCase.Name, entry.Role)
			}
			if entry.ObservedModel != "" {
				if _, err := ingest.NewObservedModelID(entry.ObservedModel); err != nil {
					t.Fatalf("model projection fixture case %q has invalid observedModel: %v", fixtureCase.Name, err)
				}
			}
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "model projection"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestModelProjectionFixtureGuards(t *testing.T) {
	loadModelProjectionFixture(t)
	manifest, err := testutil.DecodeSemanticManifest(modelProjectionManifestYAML, "model projection")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(modelProjectionFixtureYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		decoder := yaml.NewDecoder(bytes.NewReader(mutated))
		decoder.KnownFields(true)
		var fixture modelProjectionFixture
		if err := decoder.Decode(&fixture); err != nil {
			t.Fatalf("decode renamed fixture: %v", err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateSemanticNames(manifest, names, "model projection"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}

func TestModelProjectionProductionPath(t *testing.T) {
	results := runModelProjectionFixture(loadModelProjectionFixture(t))
	for _, result := range results {
		result := result
		t.Run(result.Name, func(t *testing.T) {
			for _, failure := range result.Failures {
				t.Error(failure)
			}
		})
	}
}

func runModelProjectionFixture(fixture modelProjectionFixture) []modelProjectionResult {
	results := make([]modelProjectionResult, 0, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		result := modelProjectionResult{Name: fixtureCase.Name}
		entries := make([]schema.SessionEntry, len(fixtureCase.Entries))
		for index, source := range fixtureCase.Entries {
			entry := schema.SessionEntry{EntryIndex: source.Index, Role: schema.Role(source.Role), Depth: source.Depth, ParentIndex: source.ParentIndex, EntryType: schema.EntryTypeText, ContentPreview: &source.Content}
			if source.ObservedModel != "" {
				extra, _ := json.Marshal(map[string]string{"model_id": source.ObservedModel})
				value := string(extra)
				entry.Extra = &value
			}
			entries[index] = entry
		}
		turns := EntriesToTurns(entries)
		if fixtureCase.InputPath == "detail" {
			turns = make([]ingest.Turn, len(fixtureCase.Entries))
			for index, source := range fixtureCase.Entries {
				turns[index] = ingest.Turn{Index: source.Index, Role: schema.Role(source.Role), Depth: source.Depth, ParentIndex: source.ParentIndex, Content: source.Content}
				if source.ObservedModel != "" {
					turns[index].ObservedModel = ingest.ObservedModelID(source.ObservedModel)
				}
			}
		}
		if len(turns) != len(fixtureCase.ExpectedObservedModels) {
			result.Failures = append(result.Failures, fmt.Sprintf("turn count=%d, want %d", len(turns), len(fixtureCase.ExpectedObservedModels)))
			results = append(results, result)
			continue
		}
		session := &ingest.Session{Model: fixtureCase.StoredModel, Turns: turns}
		detail := SessionToDetail(session)
		if modelProjectionAsserts(fixtureCase.Assertions, "seed") && detail.Model != fixtureCase.ExpectedSeed {
			result.Failures = append(result.Failures, fmt.Sprintf("seed=%q, want %q", detail.Model, fixtureCase.ExpectedSeed))
		}
		if modelProjectionAsserts(fixtureCase.Assertions, "observations") {
			for index, expected := range fixtureCase.ExpectedObservedModels {
				if got := detail.Turns[index].ObservedModel.String(); got != expected {
					result.Failures = append(result.Failures, fmt.Sprintf("turn %d observedModel=%q, want %q", index, got, expected))
				}
			}
		}
		results = append(results, result)
	}
	return results
}

func modelProjectionAsserts(assertions []string, want string) bool {
	for _, assertion := range assertions {
		if assertion == want {
			return true
		}
	}
	return false
}
