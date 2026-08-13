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
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/observed_model_capability.yaml
var observedModelCapabilityFixtureYAML []byte

//go:embed testdata/observed_model_capability.manifest.yaml
var observedModelCapabilityManifestYAML []byte

type observedModelCapabilityCase struct {
	Name          string                     `yaml:"name"`
	ObservedModel string                     `yaml:"observedModel"`
	Advertisement []schema.ContentCapability `yaml:"advertisement"`
	WantUploads   int                        `yaml:"wantUploads"`
	WantError     bool                       `yaml:"wantError"`
	DryRun        bool                       `yaml:"dryRun"`
	RedactPattern string                     `yaml:"redactPattern"`
	Content       string                     `yaml:"content"`
	WantContent   string                     `yaml:"wantContent"`
	ShapeMutation string                     `yaml:"shapeMutation"`
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
			content := fixtureCase.Content
			if content == "" {
				content = "answer"
			}
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
			publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{
				MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion,
				ContentCapabilities: fixtureCase.Advertisement,
			}}
			var stderr bytes.Buffer
			var redactor ingest.TextRedactor
			if fixtureCase.RedactPattern != "" {
				var err error
				redactor, err = redact.NewRedactor(redact.Standard, []redact.UserPattern{{ID: "publication-project-pattern", Category: redact.CategoryProject, Pattern: fixtureCase.RedactPattern}}, redact.XDGPaths{})
				if err != nil {
					t.Fatalf("build custom redactor: %v", err)
				}
			}
			if fixtureCase.ShapeMutation != "" {
				redactor = shapeChangingRedactor{mutation: fixtureCase.ShapeMutation}
			}
			pipeline := push.NewPipeline(store, publisher, baseCreds(), baseTestConfig(), fs, push.PipelineConfig{Concurrency: 1, DryRun: fixtureCase.DryRun}, redactor, &stderr)
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
			if fixtureCase.DryRun {
				if publisher.SchemaVersionCalls != 0 || len(store.SavedPublicationIDs) != 0 || len(store.PushLogs) != 0 || len(store.PublicationAttempts) != 0 {
					t.Fatalf("dry run side effects: schema=%d uploads=%d persistence=%d audit=%d attempts=%d", publisher.SchemaVersionCalls, len(publisher.Calls), len(store.SavedPublicationIDs), len(store.PushLogs), len(store.PublicationAttempts))
				}
				if len(result.Sessions) != 1 || result.Sessions[0].Status != push.PushStatusNew {
					t.Fatalf("dry run forecast=%+v, want one new session without capability refusal", result)
				}
			}
			if fixtureCase.WantError {
				message := result.Sessions[0].Error.Error()
				if fixtureCase.ShapeMutation != "" {
					for _, fragment := range []string{"what:", "why:", "where:", "when:", "meaning:", "fix:", "nothing was uploaded", "evidence-bearing structure"} {
						if !strings.Contains(message, fragment) {
							t.Errorf("shape refusal missing %q: %s", fragment, message)
						}
					}
					return
				}
				for _, fragment := range []string{"enriched transcript push refused", "  what:", "  why: the target Village did not advertise", "  where: push.Pipeline.pushSession", "  when:", "  meaning:", "  fix:", "after local canonical content construction and validation", "before serialization or upload", "no transcript bytes or metadata were sent", "silently removing", "then retry"} {
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
				if fixtureCase.WantContent != "" && envelope.SessionDetail.Turns[0].Content != fixtureCase.WantContent {
					t.Fatalf("uploaded content=%q, want %q", envelope.SessionDetail.Turns[0].Content, fixtureCase.WantContent)
				}
				if fixtureCase.RedactPattern != "" {
					if len(publisher.AuthoritativeCalls) != 1 || len(publisher.AuthoritativeCalls[0].Entries) != 1 {
						t.Fatalf("authoritative metadata entries=%+v, want one", publisher.AuthoritativeCalls)
					}
					modelID, present := modelIDFromExtra(t, publisher.AuthoritativeCalls[0].Entries[0].Extra)
					if !present || modelID != fixtureCase.ObservedModel {
						t.Fatalf("metadata model_id=(%q,%t), want (%q,true)", modelID, present, fixtureCase.ObservedModel)
					}
					if got := *publisher.AuthoritativeCalls[0].Entries[0].ContentPreview; got != fixtureCase.WantContent {
						t.Fatalf("metadata content=%q, want %q", got, fixtureCase.WantContent)
					}
				}
			}
		})
	}
}

type shapeChangingRedactor struct{ mutation string }

func (r shapeChangingRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	return meta
}
func (r shapeChangingRedactor) Level() string          { return "standard" }
func (r shapeChangingRedactor) RuleSetVersion() string { return "test" }
func (r shapeChangingRedactor) RedactJSON(value any) any {
	document, ok := value.(map[string]any)
	if !ok || document["kind"] != string(schema.ContentKindSessionDetail) {
		return value
	}
	if r.mutation == "remove_session_detail" {
		delete(document, "sessionDetail")
	}
	return document
}

func modelIDFromExtra(t *testing.T, extra *string) (string, bool) {
	t.Helper()
	if extra == nil {
		return "", false
	}
	var document map[string]string
	if err := json.Unmarshal([]byte(*extra), &document); err != nil {
		t.Fatalf("decode entry extra: %v", err)
	}
	value, present := document["model_id"]
	return value, present
}
