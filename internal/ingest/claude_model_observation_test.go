package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_model_observations.yaml
var claudeModelObservationFixtureYAML []byte

//go:embed testdata/claude_model_observations.manifest.yaml
var claudeModelObservationManifestYAML []byte

type claudeModelExpectedEntry struct {
	Index         int    `yaml:"index"`
	Role          string `yaml:"role"`
	ObservedModel string `yaml:"observedModel,omitempty"`
}

type claudeModelObservationCase struct {
	Name            string                     `yaml:"name"`
	Lines           []string                   `yaml:"lines"`
	ExpectedEntries []claudeModelExpectedEntry `yaml:"expectedEntries"`
}

type claudeModelObservationFixture struct {
	Cases []claudeModelObservationCase `yaml:"cases"`
}

func decodeClaudeModelObservationFixture(data []byte) (claudeModelObservationFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture claudeModelObservationFixture
	if err := decoder.Decode(&fixture); err != nil {
		return claudeModelObservationFixture{}, fmt.Errorf("decode Claude model-observation fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return claudeModelObservationFixture{}, fmt.Errorf("Claude model-observation fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || len(fixtureCase.Lines) == 0 || len(fixtureCase.ExpectedEntries) != len(fixtureCase.Lines) {
			return claudeModelObservationFixture{}, fmt.Errorf("Claude model-observation fixture case %q has incomplete or mismatched rows", fixtureCase.Name)
		}
		if _, duplicate := names[fixtureCase.Name]; duplicate {
			return claudeModelObservationFixture{}, fmt.Errorf("Claude model-observation fixture repeats case name %q", fixtureCase.Name)
		}
		names[fixtureCase.Name] = struct{}{}
		for index, line := range fixtureCase.Lines {
			var probe map[string]any
			if err := json.Unmarshal([]byte(line), &probe); err != nil {
				return claudeModelObservationFixture{}, fmt.Errorf("Claude model-observation fixture case %q line %d is invalid JSON: %w", fixtureCase.Name, index, err)
			}
			if fixtureCase.ExpectedEntries[index].Index != index || fixtureCase.ExpectedEntries[index].Role == "" {
				return claudeModelObservationFixture{}, fmt.Errorf("Claude model-observation fixture case %q expected entry %d has invalid index or role", fixtureCase.Name, index)
			}
		}
	}
	return fixture, nil
}

func loadClaudeModelObservationFixture(t *testing.T) claudeModelObservationFixture {
	t.Helper()
	fixture, err := decodeClaudeModelObservationFixture(claudeModelObservationFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeSemanticManifest(claudeModelObservationManifestYAML, "Claude model observations")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "Claude model observations"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestClaudeModelObservationFixtureGuards(t *testing.T) {
	loadClaudeModelObservationFixture(t)
	manifest, err := testutil.DecodeSemanticManifest(claudeModelObservationManifestYAML, "Claude model observations")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(claudeModelObservationFixtureYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		fixture, err := decodeClaudeModelObservationFixture(mutated)
		if err != nil {
			t.Fatalf("required case %q replacement unexpectedly failed to decode: %v", required, err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateSemanticNames(manifest, names, "Claude model observations"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}

func TestClaudeIndexer_PersistsAssistantModelObservations(t *testing.T) {
	assertClaudeModelObservationFixture(t, loadClaudeModelObservationFixture(t), func(entries []schema.SessionEntry) []schema.SessionEntry {
		return entries
	})
}

func assertClaudeModelObservationFixture(t *testing.T, fixture claudeModelObservationFixture, mutate func([]schema.SessionEntry) []schema.SessionEntry) {
	t.Helper()
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			sessionID := ingest.SessionID(testutil.TestSessionUUID)
			session := ingest.DiscoveredSession{SessionID: sessionID, Harness: ingest.HarnessClaudeCode, SourcePath: "/fixture.jsonl", SourceFormat: ingest.SourceFormatJSONL}
			entries, err := ingest.NewClaudeIndexer(testutil.NewMemFS()).IndexTranscriptBytes(context.Background(), session, []byte(strings.Join(fixtureCase.Lines, "\n")+"\n"))
			if err != nil {
				t.Fatalf("index fixture: %v", err)
			}
			if len(entries) != len(fixtureCase.ExpectedEntries) {
				t.Fatalf("indexed entry count = %d, want %d", len(entries), len(fixtureCase.ExpectedEntries))
			}

			entries = mutate(entries)
			database := storetest.Open(t)
			storetest.SeedSession(t, database, string(sessionID))
			if err := database.IndexSessionEntries(context.Background(), sessionID, entries); err != nil {
				t.Fatalf("persist indexed entries: %v", err)
			}
			stored, err := database.ListEntries(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("rehydrate indexed entries: %v", err)
			}
			if len(stored) != len(fixtureCase.ExpectedEntries) {
				t.Fatalf("stored entry count = %d, want %d", len(stored), len(fixtureCase.ExpectedEntries))
			}
			for index, expected := range fixtureCase.ExpectedEntries {
				entry := stored[index]
				if entry.EntryIndex != expected.Index || entry.Role.String() != expected.Role {
					t.Errorf("stored entry %d identity = (%d, %q), want (%d, %q)", index, entry.EntryIndex, entry.Role, expected.Index, expected.Role)
				}
				got, present := modelIDFromEntry(entry)
				if present != (expected.ObservedModel != "") || got != expected.ObservedModel {
					t.Errorf("stored entry %d model observation = (%q, present=%t), want (%q, present=%t)", index, got, present, expected.ObservedModel, expected.ObservedModel != "")
				}
			}
		})
	}
}

func modelIDFromEntry(entry schema.SessionEntry) (string, bool) {
	if entry.Extra == nil {
		return "", false
	}
	var extra map[string]any
	if json.Unmarshal([]byte(*entry.Extra), &extra) != nil {
		return "", false
	}
	model, ok := extra["model_id"].(string)
	return model, ok
}
