package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_model_observations.yaml
var openCodeModelObservationFixtureYAML []byte

//go:embed testdata/opencode_model_observations.manifest.yaml
var openCodeModelObservationManifestYAML []byte

type openCodeModelObservationCase struct {
	Name                    string `yaml:"name"`
	MessageID               string `yaml:"messageID"`
	Role                    string `yaml:"role"`
	ModelID                 string `yaml:"modelID"`
	StructuralPartType      string `yaml:"structuralPartType,omitempty"`
	ReasoningTokens         int    `yaml:"reasoningTokens,omitempty"`
	CacheRead               int    `yaml:"cacheRead,omitempty"`
	CacheWrite              int    `yaml:"cacheWrite,omitempty"`
	ExpectedRole            string `yaml:"expectedRole"`
	ExpectedModel           string `yaml:"expectedModel,omitempty"`
	ExpectedReasoningTokens int    `yaml:"expectedReasoningTokens,omitempty"`
	ExpectedCacheRead       int    `yaml:"expectedCacheRead,omitempty"`
	ExpectedCacheWrite      int    `yaml:"expectedCacheWrite,omitempty"`
}

type openCodeModelObservationFixture struct {
	Cases []openCodeModelObservationCase `yaml:"cases"`
}

func decodeOpenCodeModelObservationFixture(data []byte) (openCodeModelObservationFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture openCodeModelObservationFixture
	if err := decoder.Decode(&fixture); err != nil {
		return openCodeModelObservationFixture{}, fmt.Errorf("decode OpenCode model-observation fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return openCodeModelObservationFixture{}, fmt.Errorf("OpenCode model-observation fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || fixtureCase.MessageID == "" || fixtureCase.Role == "" || fixtureCase.ExpectedRole == "" {
			return openCodeModelObservationFixture{}, fmt.Errorf("OpenCode model-observation fixture case %q is incomplete", fixtureCase.Name)
		}
		if _, duplicate := names[fixtureCase.Name]; duplicate {
			return openCodeModelObservationFixture{}, fmt.Errorf("OpenCode model-observation fixture repeats case name %q", fixtureCase.Name)
		}
		names[fixtureCase.Name] = struct{}{}
	}
	return fixture, nil
}

func loadOpenCodeModelObservationFixture(t *testing.T) openCodeModelObservationFixture {
	t.Helper()
	fixture, err := decodeOpenCodeModelObservationFixture(openCodeModelObservationFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeSemanticManifest(openCodeModelObservationManifestYAML, "OpenCode model observations")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "OpenCode model observations"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestOpenCodeModelObservationFixtureGuards(t *testing.T) {
	loadOpenCodeModelObservationFixture(t)
	manifest, err := testutil.DecodeSemanticManifest(openCodeModelObservationManifestYAML, "OpenCode model observations")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(openCodeModelObservationFixtureYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		fixture, err := decodeOpenCodeModelObservationFixture(mutated)
		if err != nil {
			t.Fatalf("required case %q replacement unexpectedly failed to decode: %v", required, err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateSemanticNames(manifest, names, "OpenCode model observations"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}

func TestOpenCodeIndexer_ModelObservationBoundary(t *testing.T) {
	fixture := loadOpenCodeModelObservationFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			sessionID := testutil.TestOpenCodeSesID
			session := setupOpenCodeFixture(t, fs, sessionID, "project-fixture")
			messagePath := fmt.Sprintf("/opencode-store/storage/message/%s/%s.json", sessionID, fixtureCase.MessageID)
			message := map[string]any{
				"id":        fixtureCase.MessageID,
				"sessionID": sessionID,
				"role":      fixtureCase.Role,
				"modelID":   fixtureCase.ModelID,
				"content":   "fixture content",
				"tokens": map[string]any{
					"reasoning":   fixtureCase.ReasoningTokens,
					"cache_read":  fixtureCase.CacheRead,
					"cache_write": fixtureCase.CacheWrite,
				},
			}
			encoded, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(messagePath, encoded, 0644); err != nil {
				t.Fatal(err)
			}
			if fixtureCase.StructuralPartType != "" {
				partPath := fmt.Sprintf("/opencode-store/storage/part/%s/part_001.json", fixtureCase.MessageID)
				part, _ := json.Marshal(map[string]string{"id": "part_001", "type": fixtureCase.StructuralPartType})
				if err := fs.WriteFile(partPath, part, 0644); err != nil {
					t.Fatal(err)
				}
			}

			entries, err := ingest.NewOpenCodeIndexer(fs).IndexTranscript(context.Background(), session)
			if err != nil {
				t.Fatalf("index fixture: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("entry count = %d, want 1", len(entries))
			}
			entry := entries[0]
			if entry.Role.String() != fixtureCase.ExpectedRole {
				t.Errorf("role = %q, want %q", entry.Role, fixtureCase.ExpectedRole)
			}
			model, present := modelIDFromEntry(entry)
			if model != fixtureCase.ExpectedModel || present != (fixtureCase.ExpectedModel != "") {
				t.Errorf("model observation = (%q, present=%t), want (%q, present=%t)", model, present, fixtureCase.ExpectedModel, fixtureCase.ExpectedModel != "")
			}
			assertOpenCodeExtraNumber(t, entry.Extra, "tokens_reasoning", fixtureCase.ExpectedReasoningTokens)
			assertOpenCodeExtraNumber(t, entry.Extra, "cache_read", fixtureCase.ExpectedCacheRead)
			assertOpenCodeExtraNumber(t, entry.Extra, "cache_write", fixtureCase.ExpectedCacheWrite)
		})
	}
}

func assertOpenCodeExtraNumber(t *testing.T, extraJSON *string, key string, expected int) {
	t.Helper()
	var extra map[string]any
	if extraJSON != nil {
		if err := json.Unmarshal([]byte(*extraJSON), &extra); err != nil {
			t.Fatalf("decode Extra: %v", err)
		}
	}
	value, present := extra[key]
	if expected == 0 {
		if present {
			t.Errorf("Extra[%q] unexpectedly present: %v", key, value)
		}
		return
	}
	if !present || int(value.(float64)) != expected {
		t.Errorf("Extra[%q] = %v, want %d", key, value, expected)
	}
}
