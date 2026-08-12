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

//go:embed testdata/model_observation_survival.yaml
var modelObservationSurvivalFixtureYAML []byte

//go:embed testdata/model_observation_survival.manifest.yaml
var modelObservationSurvivalManifestYAML []byte

type modelObservationFixtureEntry struct {
	Index         int    `yaml:"index"`
	Role          string `yaml:"role"`
	Content       string `yaml:"content,omitempty"`
	ObservedModel string `yaml:"observedModel,omitempty"`
}

type modelObservationSurvivalCase struct {
	Name     string                         `yaml:"name"`
	Entries  []modelObservationFixtureEntry `yaml:"entries"`
	Expected []modelObservationFixtureEntry `yaml:"expected"`
}

type modelObservationSurvivalFixture struct {
	Cases []modelObservationSurvivalCase `yaml:"cases"`
}

func decodeModelObservationSurvivalFixture(data []byte) (modelObservationSurvivalFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture modelObservationSurvivalFixture
	if err := decoder.Decode(&fixture); err != nil {
		return modelObservationSurvivalFixture{}, fmt.Errorf("decode model-observation survival fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return modelObservationSurvivalFixture{}, fmt.Errorf("model-observation survival fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || len(fixtureCase.Entries) == 0 || len(fixtureCase.Expected) == 0 {
			return modelObservationSurvivalFixture{}, fmt.Errorf("model-observation survival fixture case %q is incomplete", fixtureCase.Name)
		}
		if _, duplicate := names[fixtureCase.Name]; duplicate {
			return modelObservationSurvivalFixture{}, fmt.Errorf("model-observation survival fixture repeats case name %q", fixtureCase.Name)
		}
		names[fixtureCase.Name] = struct{}{}
		for _, entry := range append(append([]modelObservationFixtureEntry{}, fixtureCase.Entries...), fixtureCase.Expected...) {
			if entry.Role != schema.RoleAssistant.String() {
				return modelObservationSurvivalFixture{}, fmt.Errorf("model-observation survival fixture case %q has unsupported role %q", fixtureCase.Name, entry.Role)
			}
		}
	}
	return fixture, nil
}

func loadModelObservationSurvivalFixture(t *testing.T) modelObservationSurvivalFixture {
	t.Helper()
	fixture, err := decodeModelObservationSurvivalFixture(modelObservationSurvivalFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeSemanticManifest(modelObservationSurvivalManifestYAML, "model-observation survival")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "model-observation survival"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestModelObservationSurvivalFixtureGuards(t *testing.T) {
	loadModelObservationSurvivalFixture(t)
	manifest, err := testutil.DecodeSemanticManifest(modelObservationSurvivalManifestYAML, "model-observation survival")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(modelObservationSurvivalFixtureYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		fixture, err := decodeModelObservationSurvivalFixture(mutated)
		if err != nil {
			t.Fatalf("required case %q replacement unexpectedly failed to decode: %v", required, err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateSemanticNames(manifest, names, "model-observation survival"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}

func TestEntriesToTurns_PreservesModelObservationBoundaries(t *testing.T) {
	assertModelObservationSurvivalFixture(t, loadModelObservationSurvivalFixture(t), EntriesToTurns)
}

func assertModelObservationSurvivalFixture(t *testing.T, fixture modelObservationSurvivalFixture, fold func([]schema.SessionEntry) []ingest.Turn) {
	t.Helper()
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			entries := make([]schema.SessionEntry, len(fixtureCase.Entries))
			for index, source := range fixtureCase.Entries {
				entries[index] = fixtureEntry(source)
			}
			turns := fold(entries)
			if len(turns) != len(fixtureCase.Expected) {
				t.Fatalf("surviving turns = %d, want %d; real suppression/dedup path dropped an observation boundary", len(turns), len(fixtureCase.Expected))
			}
			for index, expected := range fixtureCase.Expected {
				if turns[index].Index != expected.Index || turns[index].Role.String() != expected.Role || turns[index].Content != expected.Content {
					t.Errorf("surviving turn %d = (index=%d role=%q content=%q), want (index=%d role=%q content=%q)", index, turns[index].Index, turns[index].Role, turns[index].Content, expected.Index, expected.Role, expected.Content)
				}
				observation := modelObservation(entries[index])
				if observation.present != (expected.ObservedModel != "") || observation.value != expected.ObservedModel {
					t.Errorf("surviving turn %d observation = (%q, present=%t), want (%q, present=%t)", index, observation.value, observation.present, expected.ObservedModel, expected.ObservedModel != "")
				}
			}
		})
	}
}

func fixtureEntry(source modelObservationFixtureEntry) schema.SessionEntry {
	entry := schema.SessionEntry{EntryIndex: source.Index, Role: schema.Role(source.Role), EntryType: schema.EntryTypeText}
	if source.Content != "" {
		entry.ContentPreview = &source.Content
	}
	if source.ObservedModel != "" {
		extra, _ := json.Marshal(map[string]string{"model_id": source.ObservedModel})
		value := string(extra)
		entry.Extra = &value
	}
	return entry
}
