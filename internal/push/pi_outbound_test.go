package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_redaction_outbound.yaml
var piOutboundYAML []byte

//go:embed testdata/pi_redaction_outbound.manifest.yaml
var piOutboundManifest []byte

type piOutboundFixture struct {
	Cases []struct {
		Name    string `yaml:"name"`
		Data    string `yaml:"data"`
		Want    string `yaml:"want"`
		Error   bool   `yaml:"error"`
		Padding int    `yaml:"padding"`
		Expand  int    `yaml:"expand"`
	} `yaml:"cases"`
	Capabilities []struct {
		Name      string                     `yaml:"name"`
		Advertise []schema.ContentCapability `yaml:"advertise"`
		Uploads   int                        `yaml:"uploads"`
		DryRun    bool                       `yaml:"dry_run"`
	} `yaml:"capabilities"`
	Namespaces []struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
		Want      string `yaml:"want"`
	} `yaml:"namespaces"`
}

func loadPiOutboundFixtures(t *testing.T) piOutboundFixture {
	t.Helper()
	var f piOutboundFixture
	d := yaml.NewDecoder(bytes.NewReader(piOutboundYAML))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		t.Fatalf("trailing YAML: %v", err)
	}
	m, err := testutil.DecodeRequiredNamesManifest(piOutboundManifest, "Pi outbound")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range f.Cases {
		names = append(names, c.Name)
	}
	for _, c := range f.Capabilities {
		names = append(names, c.Name)
	}
	for _, c := range f.Namespaces {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(m, names, "Pi outbound"); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPiOutboundNamespaceRedaction(t *testing.T) {
	for _, c := range loadPiOutboundFixtures(t).Namespaces {
		t.Run(c.Name, func(t *testing.T) {
			entries, err := piOutboundEntries(`{"safe":true}`)
			if err != nil {
				t.Fatal(err)
			}
			extra, _, err := ingest.DecodePiExtra(entries[1].Extra)
			if err != nil {
				t.Fatal(err)
			}
			extra.Namespace = &c.Namespace
			entries[1].Extra, err = ingest.EncodePiExtra(extra)
			if err != nil {
				t.Fatal(err)
			}
			entries, err = push.RedactEntries(piRewriteRedactor{}, entries)
			if err != nil {
				t.Fatal(err)
			}
			extra, _, err = ingest.DecodePiExtra(entries[1].Extra)
			if err != nil || extra.Namespace == nil || *extra.Namespace != c.Want {
				t.Fatalf("redacted namespace=%v want %q error=%v", extra.Namespace, c.Want, err)
			}
		})
	}
}

func TestPiOutboundMetadataRedaction(t *testing.T) {
	for _, c := range loadPiOutboundFixtures(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			entries, err := piOutboundEntries(c.Data + strings.Repeat(" ", c.Padding))
			if err == nil {
				entries, err = push.RedactEntries(piRewriteRedactor{expand: c.Expand}, entries)
			}
			if c.Error {
				if err == nil {
					t.Fatal("unsafe raw metadata or collision accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			extra, pi, err := ingest.DecodePiExtra(entries[2].Extra)
			if err != nil || !pi {
				t.Fatalf("decode: %v pi=%t", err, pi)
			}
			var got, want any
			if err := json.Unmarshal(extra.Metadata[0].Data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(c.Want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("metadata=%s want=%s", extra.Metadata[0].Data, c.Want)
			}
		})
	}
}

func TestPiPipelineCapabilityAndMultipartPreservation(t *testing.T) {
	for _, c := range loadPiOutboundFixtures(t).Capabilities {
		t.Run(c.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, schema.HarnessPi)
			entries, err := piOutboundEntries(`{"safe":true}`)
			if err != nil {
				t.Fatal(err)
			}
			store := &testutil.StubPushStore{Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, string(schema.HarnessPi), nil)}, Entries: map[ingest.SessionID][]schema.SessionEntry{schema.SessionID(testutil.TestSessionUUID): entries}}
			testutil.SeedPublicationInputs(store, fs, baseTestConfig().Output.BasePath)
			publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: c.Advertise}}
			var stderr bytes.Buffer
			pipeline, err := push.NewPipeline(store, publisher, baseCreds(), baseTestConfig(), fs, push.PipelineConfig{Concurrency: 1, DryRun: c.DryRun}, &testutil.NoopRedactor{}, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(publisher.Calls) != c.Uploads {
				t.Fatalf("uploads=%d want %d result=%+v", len(publisher.Calls), c.Uploads, result)
			}
			if c.DryRun {
				if publisher.SchemaVersionCalls != 0 || len(store.PushLogs) != 0 || len(store.PublicationAttempts) != 0 || len(store.SavedPublicationIDs) != 0 {
					t.Fatal("dry run performed remote/durable effects")
				}
				if result.Errors != 0 {
					t.Fatalf("dry run forecast failed: %+v", result)
				}
				return
			}
			if c.Uploads == 0 {
				if result.Errors == 0 {
					t.Fatal("missing capability did not fail")
				}
				return
			}
			content, err := schema.DecodeTranscriptContentRaw(publisher.Calls[0].TranscriptBody)
			if err != nil {
				t.Fatal(err)
			}
			if len(content.SessionDetail.NativeMetadata) != 1 || content.SessionDetail.Turns[0].Usage == nil || content.SessionDetail.Turns[0].ObservedModel == "" || len(content.SessionDetail.Turns[0].ToolCalls) != 1 || content.SessionDetail.Turns[0].ToolCalls[0].Namespace == nil || *content.SessionDetail.Turns[0].ToolCalls[0].Namespace != "extension" {
				t.Fatal("capability-bearing evidence disappeared")
			}
			if len(publisher.AuthoritativeCalls) != 1 || len(publisher.AuthoritativeCalls[0].Entries) != 2 {
				t.Fatal("metadata carrier was not excluded")
			}
			for _, entry := range publisher.AuthoritativeCalls[0].Entries {
				if entry.Extra != nil {
					t.Fatal("private Extra escaped in multipart metadata")
				}
			}
			if bytes.Contains(publisher.Calls[0].TranscriptBody, []byte("pi.carrier")) {
				t.Fatal("private carrier escaped transcript part")
			}
		})
	}
}

func piOutboundEntries(data string) ([]schema.SessionEntry, error) {
	sid := schema.SessionID(testutil.TestSessionUUID)
	u, err := ingest.PiUsageFromRaw(string(sid), "assistant", schema.UsageScopeAssistant, nil)
	if err != nil {
		return nil, err
	}
	extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraUsage, Harness: schema.HarnessPi, SourceRef: u.SourceEntryRef, Usage: &u, ModelID: "fixture-model"})
	if err != nil {
		return nil, err
	}
	text := "answer"
	entry := schema.SessionEntry{SessionID: sid, Harness: schema.HarnessPi, EntryIndex: 0, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText, ContentPreview: &text, Extra: extra}
	namespace := "extension"
	toolID, toolName, toolInput, parent := ingest.PiPublicRef(string(sid), "tool", "tool"), "search", "{}", 0
	toolExtra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraState, Harness: schema.HarnessPi, SourceRef: u.SourceEntryRef, Namespace: &namespace})
	if err != nil {
		return nil, err
	}
	toolEntry := schema.SessionEntry{SessionID: sid, Harness: schema.HarnessPi, EntryIndex: 1, Role: schema.RoleAssistant, EntryType: schema.EntryTypeToolUse, Depth: 1, ParentIndex: &parent, ToolCallID: &toolID, ToolNamesCSV: &toolName, ToolInput: &toolInput, Extra: toolExtra}
	ref := ingest.PiPublicRef(string(sid), "entry", "custom")
	carrier, err := ingest.NewPiCarrier(sid, 2, ingest.PiExtra{Kind: ingest.PiExtraCarrier, Harness: schema.HarnessPi, SourceRef: ref, Metadata: []schema.NativeMetadataRecord{{ID: ingest.PiPublicRef(string(sid), "metadata", "custom"), Kind: schema.NativeMetadataPiCustomData, Source: schema.NativeSourceRef{EntryRef: ref, SourceType: schema.NativeSourcePiCustom}, CustomType: "extension", Data: json.RawMessage(data)}}})
	return []schema.SessionEntry{entry, toolEntry, carrier}, err
}

type piRewriteRedactor struct {
	testutil.NoopRedactor
	expand int
}

var _ ingest.TextRedactor = (*piRewriteRedactor)(nil)

func (r piRewriteRedactor) RedactJSON(v any) any {
	switch x := v.(type) {
	case string:
		if r.expand > 0 && x == "expand" {
			return strings.Repeat("x", r.expand)
		}
		return strings.ReplaceAll(x, "secret", "safe")
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = r.RedactJSON(v)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = r.RedactJSON(v)
		}
		return out
	default:
		return v
	}
}
