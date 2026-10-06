package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/thinking_level_capability.yaml
var thinkingLevelCapabilityYAML []byte

//go:embed testdata/thinking_level_capability.manifest.yaml
var thinkingLevelCapabilityManifestYAML []byte

func TestPipelineThinkingLevelCapability(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name    string                  `yaml:"name"`
			Level   schema.ThinkingLevel    `yaml:"level"`
			Raw     schema.ThinkingLevelRaw `yaml:"raw"`
			History []struct {
				Level schema.ThinkingLevel    `yaml:"level"`
				Raw   schema.ThinkingLevelRaw `yaml:"raw"`
			} `yaml:"history"`
			ObservedModel string                     `yaml:"observedModel"`
			Advertisement []schema.ContentCapability `yaml:"advertisement"`
			Required      []schema.ContentCapability `yaml:"required"`
			Refuse        bool                       `yaml:"refuse"`
			SeedOnly      bool                       `yaml:"seedOnly"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(thinkingLevelCapabilityYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(thinkingLevelCapabilityManifestYAML, "thinking level capability")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, tc := range fixture.Cases {
		names[i] = tc.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "thinking level capability"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
			if tc.SeedOnly {
				path := filepath.Join("/sync", testutil.TestHostSlug, testutil.TestSessionUUID, testutil.TestSessionUUID+"--metadata.json")
				data, err := fs.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var metadata schema.UnifiedMetadata
				if err := json.Unmarshal(data, &metadata); err != nil {
					t.Fatal(err)
				}
				metadata.ThinkingLevel, metadata.ThinkingLevelRaw = tc.Level, tc.Raw
				for _, change := range tc.History {
					metadata.ThinkingLevelHistory = append(metadata.ThinkingLevelHistory, schema.ThinkingLevelChange{Level: change.Level, Raw: change.Raw})
				}
				data, err = json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := fs.WriteFile(path, data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			extra := map[string]string{}
			if tc.Level != "" && !tc.SeedOnly {
				extra["thinking_level"] = string(tc.Level)
			}
			if tc.Raw != "" && !tc.SeedOnly {
				extra["thinking_level_raw"] = string(tc.Raw)
			}
			if tc.ObservedModel != "" {
				extra["model_id"] = tc.ObservedModel
			}
			data, err := json.Marshal(extra)
			if err != nil {
				t.Fatal(err)
			}
			value, content := string(data), "answer"
			entry := schema.SessionEntry{SessionID: schema.SessionID(testutil.TestSessionUUID), EntryIndex: 1, Harness: defaults.HarnessClaudeCode, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText, ContentPreview: &content, Extra: &value}
			store := &testutil.StubPushStore{Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)}, Entries: map[ingest.SessionID][]schema.SessionEntry{ingest.SessionID(testutil.TestSessionUUID): {entry}}}
			publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: tc.Advertisement}}
			testutil.SeedPublicationInputs(store, fs, baseTestConfig().Output.BasePath)
			var stderr bytes.Buffer
			pipeline, err := push.NewPipeline(store, publisher, baseCreds(), baseTestConfig(), fs, push.PipelineConfig{Concurrency: 1}, &testutil.NoopRedactor{}, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.Refuse {
				if len(publisher.Calls) != 0 || result.Errors != 1 || len(result.Sessions) != 1 {
					t.Fatalf("refusal side effects: uploads=%d result=%+v", len(publisher.Calls), result)
				}
				message := result.Sessions[0].Error.Error()
				if !strings.Contains(message, "thinking_level_v1") || !strings.Contains(message, "before serialization or upload") || !strings.Contains(message, "then retry") {
					t.Fatalf("non-actionable refusal: %s", message)
				}
				return
			}
			if result.Errors != 0 || len(publisher.Calls) != 1 {
				t.Fatalf("uploads=%d result=%+v stderr=%s", len(publisher.Calls), result, stderr.String())
			}
			var envelope schema.TranscriptContent
			if err := json.Unmarshal(publisher.Calls[0].TranscriptBody, &envelope); err != nil {
				t.Fatal(err)
			}
			detail := envelope.SessionDetail
			if detail == nil || detail.ThinkingLevel != tc.Level || detail.ThinkingLevelRaw != tc.Raw || len(detail.Turns) != 1 {
				t.Fatalf("unexpected envelope: %+v", envelope)
			}
			wantLevel, wantRaw := tc.Level, tc.Raw
			if tc.SeedOnly {
				wantLevel, wantRaw = "", ""
			}
			if detail.Turns[0].ThinkingLevel != wantLevel || detail.Turns[0].ThinkingLevelRaw != wantRaw {
				t.Fatalf("lost turn evidence: %+v", detail.Turns)
			}
			if len(detail.ThinkingLevelHistory) != len(tc.History) {
				t.Fatalf("history=%+v, want %+v", detail.ThinkingLevelHistory, tc.History)
			}
			for i, change := range detail.ThinkingLevelHistory {
				if change.Level != tc.History[i].Level || change.Raw != tc.History[i].Raw {
					t.Fatalf("history[%d]=(%q,%q), want %+v", i, change.Level, change.Raw, tc.History[i])
				}
			}
			got := schema.RequiredContentCapabilities(*detail)
			if !slices.Equal(got, tc.Required) {
				t.Fatalf("required=%v, want %v", got, tc.Required)
			}
			if envelope.ContractVersion != "0.1.1" {
				t.Fatalf("publish envelope contract changed: %s", envelope.ContractVersion)
			}
		})
	}
}
