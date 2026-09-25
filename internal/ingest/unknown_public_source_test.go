package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/unknown_public_source.yaml
var unknownPublicSourceYAML []byte

type unknownPublicSourceCase struct {
	PayloadPrefix string         `yaml:"payload_prefix"`
	PayloadSuffix string         `yaml:"payload_suffix"`
	Complete      bool           `yaml:"complete"`
	Namespace     string         `yaml:"namespace"`
	Name          string         `yaml:"name"`
	Harness       ingest.Harness `yaml:"harness"`
	Source        string         `yaml:"source"`
	RecordIndices []int64        `yaml:"record_indices"`
	Positions     []int64        `yaml:"positions"`
	Pointers      []string       `yaml:"pointers"`
}

type unknownPublicSourceDocument struct {
	Required []string                  `yaml:"required_names"`
	Payload  string                    `yaml:"payload"`
	Expected string                    `yaml:"expected"`
	Sidecar  string                    `yaml:"sidecar"`
	Corrupt  map[ingest.Harness]string `yaml:"corrupt"`
	Cases    []unknownPublicSourceCase `yaml:"cases"`
}

func loadUnknownPublicSources(t *testing.T) unknownPublicSourceDocument {
	t.Helper()
	var doc unknownPublicSourceDocument
	d := yaml.NewDecoder(bytes.NewReader(unknownPublicSourceYAML))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing fixture document: %v", err)
	}
	names := map[string]bool{}
	var actualNames []string
	for _, c := range doc.Cases {
		if names[c.Name] || c.Name == "" || c.Source == "" || doc.Corrupt[c.Harness] == "" || (len(c.Positions) == 0) != c.Complete || len(c.Positions) != len(c.RecordIndices) || len(c.Positions) != len(c.Pointers) {
			t.Fatalf("invalid source fixture %q", c.Name)
		}
		names[c.Name] = true
		actualNames = append(actualNames, c.Name)
	}
	if err := testutil.RequireFixtureNames("unknown public source", "case", doc.Required, names); err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: doc.Required}, actualNames, "unknown public source"); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Only discovery is fixed to the synthetic file. Metadata extraction, source
// acquisition, capture, redaction, indexing, SQLite, export and push are real.
type fixedUnknownDiscovery struct {
	ingest.SourceAdapter
	session ingest.DiscoveredSession
}

func (a *fixedUnknownDiscovery) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	return []ingest.DiscoveredSession{a.session}, nil
}

var _ ingest.SourceAdapter = (*fixedUnknownDiscovery)(nil)

func TestUnknownSourceToPublication(t *testing.T) {
	t.Parallel()
	doc := loadUnknownPublicSources(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			dir := t.TempDir()
			fs := &ingest.OSFileSystem{}
			body := strings.Repeat("synthetic-", 1024)
			secretBody := strings.Repeat("A", 36)
			payload := strings.ReplaceAll(strings.ReplaceAll(doc.Payload, "SECRET_BODY", secretBody), "BODY", body)
			expected := strings.ReplaceAll(doc.Expected, "BODY", body)
			payload = c.PayloadPrefix + payload + c.PayloadSuffix
			expected = c.PayloadPrefix + expected + c.PayloadSuffix
			data := strings.ReplaceAll(c.Source, "UNKNOWN", payload)
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			path, err := ingest.NewResolvedPath(filepath.Join(dir, sid.String()+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path.String(), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if c.Harness == ingest.HarnessStrike {
				if err := os.WriteFile(strings.TrimSuffix(path.String(), ".jsonl")+".meta.json", []byte(strings.ReplaceAll(doc.Sidecar, "SESSION_ID", sid.String())), 0600); err != nil {
					t.Fatal(err)
				}
			}
			session := ingest.DiscoveredSession{SessionID: sid, Harness: c.Harness, SourcePath: path, SourceFormat: ingest.SourceFormatJSONL, CWD: "/workspace", ModTime: time.Now().Add(-time.Hour)}
			factory := func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
				var adapter ingest.SourceAdapter
				switch c.Harness {
				case ingest.HarnessClaudeCode:
					adapter = ingest.NewClaudeAdapter(fs, git, s)
				case ingest.HarnessCursor:
					adapter = ingest.NewCursorAdapter(fs, git, s)
				case ingest.HarnessStrike:
					adapter = ingest.NewStrikeAdapter(fs, git, s)
				default:
					t.Fatalf("unsupported fixture harness %s", c.Harness)
				}
				return &fixedUnknownDiscovery{SourceAdapter: adapter, session: session}
			}
			db := storetest.Open(t)
			writer := &unknownFailingStore{Store: db}
			cfg := makePipelineConfig(filepath.Join(dir, "managed"))
			cfg.Sources = map[ingest.Harness]ingest.SourceConfig{c.Harness: {Enabled: true, Paths: []ingest.ResolvedPath{path}}}
			cfg.Force = true
			run := func() *ingest.PipelineResult {
				p, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{c.Harness: factory}, cfg, ingest.WithStore(writer), ingest.WithMetricsStore(writer), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
				if err != nil {
					t.Fatal(err)
				}
				result, err := p.Run(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			result := run()
			if result.Summary.Indexed != 1 {
				t.Fatalf("source failed to index: %+v %+v", result, result.IndexLog)
			}
			counts := result.Summary.RetainedUnknownKinds
			if c.Complete {
				if len(counts) != 0 {
					t.Fatal("additive fields were falsely counted as uninterpreted records")
				}
			} else if len(counts) != 1 || counts[0].Harness != c.Harness || counts[0].Namespace != c.Namespace || counts[0].Kind != "future" || counts[0].Occurrences != len(c.Positions) || counts[0].Sessions != 1 {
				t.Fatalf("source occurrence/session counts disagree: %+v", counts)
			}
			entries, err := db.ListEntries(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			var evidence []ingest.RetainedUnknown
			for _, entry := range entries {
				records, err := ingest.RetainedUnknownOf(entry)
				if err != nil {
					t.Fatal(err)
				}
				evidence = append(evidence, records...)
			}
			if len(evidence) != len(c.Positions) {
				t.Fatalf("stored occurrences: %d", len(evidence))
			}
			for i, record := range evidence {
				p := record.Position.Public
				if p == nil || p.SourceRef != "source-0" || p.RecordIndex != c.RecordIndices[i] || p.Position != c.Positions[i] || record.Position.Line != int(c.RecordIndices[i])+1 || record.Position.JSONPointer != c.Pointers[i] || record.Namespace != c.Namespace {
					t.Fatalf("actual traversal not retained: %+v want record %d position %d pointer %s", record.Position, c.RecordIndices[i], c.Positions[i], c.Pointers[i])
				}
				if string(record.Payload) != payload {
					t.Fatalf("stored evidence is not raw source bytes: got %.300s want %.300s", record.Payload, payload)
				}
			}
			detail, err := export.ExportSession(ctx, db, fs, sid.String())
			if err != nil {
				t.Fatalf("real capture is not exportable: %v", err)
			}
			// Interim raw-at-rest: export serves stored bytes verbatim until
			// SLICE-5 lands export-time baseline redaction; the upload below
			// already carries the redacted form via the configured engine.
			assertUnknownPublicDetail(t, c, payload, detail)
			capture, found, err := db.GetSessionContentCapture(ctx, sid)
			wantStatus, wantCode := ingest.ContentCaptureIncomplete, ingest.ContentCaptureUnknownDataRetained
			if c.Complete {
				wantStatus, wantCode = ingest.ContentCaptureComplete, ingest.ContentCaptureNoFailure
			}
			if err != nil || !found || capture.Status != wantStatus || capture.CaptureFormat != ingest.ContentCaptureFormatFull || capture.FailureCode != wantCode {
				t.Fatalf("full certificate: %+v %v", capture, err)
			}
			cfg.Force = false
			settled := run()
			if settled.Summary.Indexed != 0 || len(settled.Summary.RetainedUnknownKinds) != 0 {
				t.Fatalf("unchanged source repeated capture work: %+v", settled.Summary)
			}
			cfg.Force = true
			publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{MinPushContractVersion: "0.0.1", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: schema.AllContentCapabilities}}
			engine, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			pushConfig := &config.Config{Output: config.OutputConfig{BasePath: cfg.OutputDir.String()}, Push: config.PushConfig{Method: config.PushMethodAll, Visibility: config.VisibilityPrivate}}
			creds := &auth.Credentials{APIKey: "synthetic-key", KeyID: "synthetic-key-id", UserID: "synthetic-user", Username: "synthetic", VillageURL: "https://village.example.test"}
			p, err := push.NewPipeline(db, publisher, creds, pushConfig, nil, push.PipelineConfig{Force: true, Concurrency: 1, FilterSessionIDs: []string{sid.String()}}, engine, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			published, err := p.Run(ctx)
			if err != nil || len(publisher.Calls) != 1 || published.Errors != 0 {
				t.Fatalf("production publication: %+v %v calls=%d", published, err, len(publisher.Calls))
			}
			var envelope schema.TranscriptContent
			if err := json.Unmarshal(publisher.Calls[0].TranscriptBody, &envelope); err != nil {
				t.Fatal(err)
			}
			assertUnknownPublicDetail(t, c, expected, envelope.SessionDetail)
			if len(publisher.AuthoritativeCalls) != 1 {
				t.Fatal("authoritative publication missing")
			}
			partial := publisher.AuthoritativeCalls[0].Diagnostics.Partial
			if !c.Complete && (partial == nil || !*partial) || c.Complete && partial != nil && *partial {
				t.Fatal("partial metadata mirror disagrees with actual upload")
			}
			priorExport, err := export.ExportSession(ctx, db, fs, sid.String())
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(priorExport)
			// A forced retained reindex cannot replace the prior export when the
			// atomic evidence write fails, nor claim newly accounted occurrences.
			cfg.Reindex = true
			writer.fail = true
			failed := run()
			if failed.Summary.Indexed != 0 || len(failed.Summary.RetainedUnknownKinds) != 0 {
				t.Fatalf("failed write certified accounting: %+v", failed.Summary)
			}
			after, err := export.ExportSession(ctx, db, fs, sid.String())
			if err != nil {
				t.Fatal(err)
			}
			afterBytes, _ := json.Marshal(after)
			if !bytes.Equal(before, afterBytes) {
				t.Fatal("failed capture write replaced prior export")
			}
			// Broken JSON and invalid known fields remain capture errors; neither
			// may be certified merely because earlier unknown data was retained.
			indexer := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[c.Harness].(ingest.AuthoritativeTranscriptIndexer)
			if _, err := indexer.IndexTranscriptBytesForCapture(ctx, session, []byte(data+"{\n")); err == nil {
				t.Fatal("broken JSON certified")
			}
			if _, err := indexer.IndexTranscriptBytesForCapture(ctx, session, []byte(data+doc.Corrupt[c.Harness]+"\n")); err == nil {
				t.Fatal("invalid known data certified")
			}
		})
	}
}

func assertUnknownPublicDetail(t *testing.T, c unknownPublicSourceCase, expected string, detail *schema.SessionDetailPayload) {
	t.Helper()
	if detail == nil || len(detail.RetainedUnknown) != len(c.Positions) {
		t.Fatalf("public evidence missing: %+v", detail)
	}
	if !c.Complete && (detail.Diagnostics == nil || !detail.Diagnostics.Partial) || c.Complete && detail.Diagnostics != nil && detail.Diagnostics.Partial {
		t.Fatal("interpretation partial flag disagrees with source")
	}
	for i, record := range detail.RetainedUnknown {
		if record.SourceRef != "source-0" || record.RecordIndex != c.RecordIndices[i] || record.Position != c.Positions[i] || record.Pointer != c.Pointers[i] || record.Payload != expected || record.Namespace != c.Namespace || record.Kind != "future" {
			t.Fatalf("public source evidence differs at occurrence %d: record=%d position=%d pointer=%s", i, record.RecordIndex, record.Position, record.Pointer)
		}
	}
	found := false
	for _, turn := range detail.Turns {
		if strings.Contains(turn.Content, "closing") {
			found = true
		}
		if strings.Contains(turn.Content, "synthetic-synthetic-") {
			t.Fatal("unknown payload fabricated conversation")
		}
	}
	if !found {
		t.Fatal("known closing sibling lost")
	}
}
