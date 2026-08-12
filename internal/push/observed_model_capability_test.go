package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/observed_model_capability.yaml
var observedModelCapabilityFixtureYAML []byte

//go:embed testdata/observed_model_capability.manifest.yaml
var observedModelCapabilityManifestYAML []byte

type observedModelCapabilityCase struct {
	Name          string                                   `yaml:"name"`
	ObservedModel string                                   `yaml:"observedModel"`
	Advertisement []village.ContentCapabilityAdvertisement `yaml:"advertisement"`
	WantUploads   int                                      `yaml:"wantUploads"`
	WantError     bool                                     `yaml:"wantError"`
}

type observedModelCapabilityFixture struct {
	Cases []observedModelCapabilityCase `yaml:"cases"`
}

func loadObservedModelCapabilityFixture(t *testing.T) observedModelCapabilityFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(observedModelCapabilityFixtureYAML))
	decoder.KnownFields(true)
	var fixture observedModelCapabilityFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode observed model capability fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("observed model capability fixture must contain exactly one document: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(observedModelCapabilityManifestYAML, "observed model capability")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if fixtureCase.Name == "" || fixtureCase.WantUploads < 0 {
			t.Fatalf("observed model capability fixture case %q is incomplete", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "observed model capability"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestPipelineObservedModelCapabilityGate(t *testing.T) {
	for _, fixtureCase := range loadObservedModelCapabilityFixture(t).Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
			entry := schema.SessionEntry{SessionID: schema.SessionID(testutil.TestSessionUUID), EntryIndex: 1, Harness: defaults.HarnessClaudeCode, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText}
			content := "answer"
			entry.ContentPreview = &content
			if fixtureCase.ObservedModel != "" {
				extra, _ := json.Marshal(map[string]string{"model_id": fixtureCase.ObservedModel})
				value := string(extra)
				entry.Extra = &value
			}
			store := &testutil.StubPushStore{
				Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)},
				Entries:  map[ingest.SessionID][]schema.SessionEntry{ingest.SessionID(testutil.TestSessionUUID): {entry}},
			}
			publisher := &testutil.StubPublisher{SchemaVersionResp: &village.SchemaVersionResponse{
				SchemaVersionResponse: schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion},
				ContentCapabilities:   fixtureCase.Advertisement,
			}}
			var stderr bytes.Buffer
			pipeline := newTestPipeline(store, publisher, fs, baseTestConfig(), push.PipelineConfig{Concurrency: 1}, &stderr)
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := len(publisher.Calls); got != fixtureCase.WantUploads {
				t.Fatalf("uploads=%d, want %d; result=%+v", got, fixtureCase.WantUploads, result)
			}
			gotError := result.Errors > 0
			if gotError != fixtureCase.WantError {
				t.Fatalf("error=%t, want %t; result=%+v", gotError, fixtureCase.WantError, result)
			}
			if fixtureCase.WantError {
				message := result.Sessions[0].Error.Error()
				for _, fragment := range []string{"refused because", "did not advertise", "in push.Pipeline.pushSession", "before content construction or upload", "no transcript bytes or metadata were sent", "silently removing", "then retry"} {
					if !strings.Contains(message, fragment) {
						t.Errorf("actionable refusal missing %q: %s", fragment, message)
					}
				}
			}
			if fixtureCase.ObservedModel != "" && fixtureCase.WantUploads == 1 {
				var envelope schema.TranscriptContent
				if err := json.Unmarshal(publisher.Calls[0].TranscriptBody, &envelope); err != nil {
					t.Fatalf("decode upload: %v", err)
				}
				if got := envelope.SessionDetail.Turns[0].ObservedModel.String(); got != fixtureCase.ObservedModel {
					t.Fatalf("uploaded observedModel=%q, want %q", got, fixtureCase.ObservedModel)
				}
			}
		})
	}
}
