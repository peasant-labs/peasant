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

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/retained_unknown_publication.yaml
var retainedPublicationYAML []byte

//go:embed testdata/retained_unknown_publication.manifest.yaml
var retainedPublicationManifest []byte

type retainedPublicationCase struct {
	Kind               string `yaml:"kind"`
	Namespace          string `yaml:"namespace"`
	ExpectedReason     string `yaml:"expectedReason"`
	ExpectedKind       string `yaml:"expectedKind"`
	ExpectedNamespace  string `yaml:"expectedNamespace"`
	Managed            bool   `yaml:"managed"`
	Earlier            bool   `yaml:"earlier"`
	Name               string `yaml:"name"`
	Payload            string `yaml:"payload"`
	Expected           string `yaml:"expected"`
	Pointer            string `yaml:"pointer"`
	Repeat             int    `yaml:"repeat"`
	Advertised         bool   `yaml:"advertised"`
	MissingCoordinates bool   `yaml:"missingCoordinates"`
	Duplicate          bool   `yaml:"duplicate"`
	WriteError         bool   `yaml:"writeError"`
}

func loadRetainedPublicationCases(t *testing.T) []retainedPublicationCase {
	t.Helper()
	var fixture struct {
		Cases []retainedPublicationCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(retainedPublicationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("fixture requires one YAML document")
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(retainedPublicationManifest, "retained publication")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "retained publication"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func TestRetainedUnknownPublication(t *testing.T) {
	for _, c := range loadRetainedPublicationCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			db := storetest.Open(t)
			if c.Managed {
				db = ppeOpenStore(t)
			}
			built := ppeBuild(t, publishedProvenanceEnvelopeCase{Name: c.Name, Harness: "claude-code", Path: "legacy", Entries: "plain", Stats: "zero"})
			entries := built.legacyEntries
			// A prior good capture must survive any evidence/write refusal.
			testutil.SeedReadyPublication(t, db, built.meta, entries)
			before, err := export.ExportSession(ctx, db, nil, string(ppeSessionID))
			if err != nil {
				t.Fatal(err)
			}
			payload := strings.ReplaceAll(c.Payload, "BODY", strings.Repeat("x", c.Repeat))
			expected := strings.ReplaceAll(c.Expected, "BODY", strings.Repeat("x", c.Repeat))
			position := ingest.UnknownSourcePosition{Line: 2, JSONPointer: c.Pointer}
			if !c.MissingCoordinates {
				position.Public = &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: 1, Position: 3}
			}
			kind, namespace := c.Kind, c.Namespace
			if kind == "" {
				kind = "future"
			}
			if namespace == "" {
				namespace = "record"
			}
			record, err := ingest.NewRetainedUnknownFromSource(schema.HarnessClaudeCode, namespace, kind, position, json.RawMessage(payload))
			if err != nil {
				t.Fatal(err)
			}
			carrier, err := ingest.RetainedUnknownEntry(ppeSessionID, 1, record)
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, carrier)
			if c.Duplicate {
				duplicate, err := ingest.RetainedUnknownEntry(ppeSessionID, 2, record)
				if err != nil {
					t.Fatal(err)
				}
				entries = append(entries, duplicate)
			}
			closing := "known closing text"
			entries = append(entries, schema.SessionEntry{SessionID: ppeSessionID, EntryIndex: len(entries), Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &closing})
			input, _, err := push.LoadPublicationInput(ctx, db, string(ppeSessionID))
			if err != nil {
				t.Fatal(err)
			}
			versions := testutil.HarvesterVersionsForSeed(t, schema.HarnessClaudeCode)
			capture := ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, FailureCode: ingest.ContentCaptureUnknownDataRetained, CaptureFormat: ingest.ContentCaptureFormatFull, SourceAuthority: ingest.ContentSourceNewIngest}
			wantRecords := 1
			var writeErr error
			if c.Managed {
				generation := built.generation
				entries[0].SourceEntryRef = ""
				entries[0].Provenance = nil
				generation.Generation.Main.Entries = entries
				generation.Generation.Metadata.Stats.TurnCount = 2
				if c.Earlier {
					priorRecord := record
					priorRecord.Position = ingest.UnknownSourcePosition{Line: 1, Public: &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: 0, Position: 0}}
					priorEntry, err := ingest.RetainedUnknownEntry(ppeSessionID, 0, priorRecord)
					if err != nil {
						t.Fatal(err)
					}
					generation.Generation.Earlier = []indexformat.EarlierPartition{{State: schema.EarlierHistoryUncertainMigrated, Content: indexformat.Partition{Entries: []schema.SessionEntry{priorEntry}}}}
					wantRecords++
				}
				if err := generation.Validate(); err != nil {
					t.Fatal(err)
				}
				_, writeErr = db.ActivateGeneration(ctx, store.GenerationActivation{Generation: generation, Blobs: built.blobs, IndexerVersion: versions.IndexerVersion, IndexedAtMs: 1, CaptureRevision: input.CaptureRevision, ContentCapture: capture})
			} else {
				written := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
					SessionID: ppeSessionID, Result: indexformat.V1{Entries: entries}, IndexVersion: versions.IndexVersion,
					IndexerVersion: versions.IndexerVersion, CaptureRevision: input.CaptureRevision, RequireFullContent: true,
					ContentCapture: capture,
				}})
				if len(written) != 1 {
					t.Fatalf("missing write result: %+v", written)
				}
				writeErr = written[0].Err
			}
			if (writeErr != nil) != c.WriteError {
				t.Fatalf("write error=%v, want refusal=%v", writeErr, c.WriteError)
			}
			detail, err := export.ExportSession(ctx, db, nil, string(ppeSessionID))
			if err != nil {
				t.Fatal(err)
			}
			if c.WriteError {
				old, _ := json.Marshal(before)
				current, _ := json.Marshal(detail)
				if !bytes.Equal(old, current) {
					t.Fatal("failed write changed prior export")
				}
				return
			}
			if detail.Diagnostics == nil || !detail.Diagnostics.Partial || len(detail.RetainedUnknown) != wantRecords || detail.RetainedUnknown[0].Payload != payload {
				t.Fatalf("export lost retained evidence or partial state: %+v", detail.RetainedUnknown)
			}
			if len(detail.Turns) != 2 || detail.Turns[1].Content != closing {
				t.Fatal("evidence invented a turn or lost known siblings")
			}
			engine, err := redact.NewRedactor(redact.Standard, []redact.UserPattern{{ID: "unknown-secret", Category: redact.CategorySecrets, Pattern: "custom-secret", Replacement: "[CUSTOM]"}}, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			publisher := ppePublisher(publishedProvenanceEnvelopeCase{Advertisement: "graph"})
			selected, selectedDetail, err := push.LoadPublicationInput(ctx, db, string(ppeSessionID))
			if err != nil {
				t.Fatal(err)
			}
			content, err := push.BuildPublishTranscriptContent(selectedDetail, &selected.Metadata, selected.Entries, publisher.SchemaVersionResp.PushContractVersion, baseTestConfig().Push.Fields, selected.SessionOrigin)
			if err != nil {
				t.Fatal(err)
			}
			review, err := push.PublicationReviewText(content, engine)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(c.Expected, "[CUSTOM]") && !strings.Contains(review, "custom-secret") {
				t.Fatal("consent scanner cannot see the decoded secret")
			}
			if !c.Advertised {
				var capabilities []schema.ContentCapability
				for _, capability := range schema.AllContentCapabilities {
					if capability != schema.ContentCapabilityRetainedUnknownV1 {
						capabilities = append(capabilities, capability)
					}
				}
				publisher.SchemaVersionResp.ContentCapabilities = capabilities
			}
			pipeline, err := push.NewPipeline(db, publisher, baseCreds(), baseTestConfig(), nil, push.PipelineConfig{Force: true, Concurrency: 1, FilterSessionIDs: []string{string(ppeSessionID)}}, engine, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !c.Advertised {
				if len(publisher.Calls) != 0 || result.Errors == 0 {
					t.Fatalf("receiver silently downgraded: %+v", result)
				}
				if c.ExpectedReason == "" || len(result.Sessions) != 1 || result.Sessions[0].Error == nil || !strings.Contains(result.Sessions[0].Error.Error(), c.ExpectedReason) || !strings.Contains(result.Sessions[0].Error.Error(), "retained_unknown_v1") {
					t.Fatal("refusal did not identify the missing retained evidence capability")
				}
				return
			}
			if len(publisher.Calls) != 1 || result.Errors != 0 {
				t.Fatalf("publish failed: %+v", result)
			}
			if bytes.Contains(publisher.Calls[0].TranscriptBody, []byte("custom-secret")) {
				t.Fatal("configured secret survived actual uploaded transcript")
			}
			private, err := push.RedactEntries(engine, entries)
			if err != nil {
				t.Fatal(err)
			}
			privateBytes, err := json.Marshal(private)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(privateBytes, []byte("custom-secret")) {
				t.Fatal("private evidence restoration bypassed label redaction")
			}
			var envelope schema.TranscriptContent
			if err := json.Unmarshal(publisher.Calls[0].TranscriptBody, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.SessionDetail.RetainedUnknown[0].Payload != expected {
				t.Fatalf("payload changed or escaped secret leaked: %q", envelope.SessionDetail.RetainedUnknown[0].Payload)
			}
			last := envelope.SessionDetail.RetainedUnknown[wantRecords-1]
			if c.ExpectedKind != "" && (last.Kind != c.ExpectedKind || last.Namespace != c.ExpectedNamespace) {
				t.Fatal("native labels were stripped or changed instead of applying the configured rule")
			}
			if last.Pointer != c.Pointer || last.Position != 3 || last.RecordIndex != 1 {
				t.Fatal("source coordinates changed")
			}
			if c.Earlier && (envelope.SessionDetail.RetainedUnknown[0].Position != 0 || len(envelope.SessionDetail.EarlierHistory) != 1) {
				t.Fatal("earlier history occurrence was lost or reordered")
			}
			if len(publisher.AuthoritativeCalls) != 1 {
				t.Fatal("no authoritative publication")
			}
			if publisher.AuthoritativeCalls[0].Diagnostics.Partial == nil || !*publisher.AuthoritativeCalls[0].Diagnostics.Partial {
				t.Fatal("authoritative metadata lost partial mirror")
			}
		})
	}
}
