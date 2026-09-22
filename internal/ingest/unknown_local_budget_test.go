package ingest_test

import (
	"bytes"
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
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/unknown_local_budget.yaml
var unknownLocalBudgetYAML []byte

func TestUnknownLocalRetentionBeyondTransferBudget(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Payload       string   `yaml:"payload"`
		Source        string   `yaml:"source"`
		Cases         []struct {
			Earlier      bool   `yaml:"earlier"`
			Name         string `yaml:"name"`
			Native       bool   `yaml:"native"`
			PaddingBytes int    `yaml:"paddingBytes"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(unknownLocalBudgetYAML))
	d.KnownFields(true)
	if err := d.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatal("expected one fixture document")
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "local retention budget"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			fs := &ingest.OSFileSystem{}
			payload := strings.ReplaceAll(fixture.Payload, "BODY", strings.Repeat("x", c.PaddingBytes))
			if len(payload) <= 8<<20 || len(payload) >= defaults.MaxJSONLRecordBytes {
				t.Fatal("fixture does not straddle transfer/source boundary")
			}
			sid := schema.SessionID(testutil.TestSessionUUID)
			sourcePath := ingest.ResolvedPath(filepath.Join(dir, "source.jsonl"))
			source := strings.ReplaceAll(strings.ReplaceAll(fixture.Source, "SESSION_ID", string(sid)), "UNKNOWN", payload)
			if c.Earlier {
				source = strings.Replace(source, `"history_mode":"legacy"`, `"history_mode":"legacy","subagent_history_start_ordinal":3`, 1)
			}
			if err := os.WriteFile(sourcePath.String(), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			session := ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessCodex, SourcePath: sourcePath, SourceFormat: ingest.SourceFormatJSONL, CWD: "/workspace", ModTime: time.Now().Add(-time.Hour)}
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessCodex: func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
				return &fixedUnknownDiscovery{SourceAdapter: ingest.NewCodexAdapter(fs, git, s), session: session}
			}}
			dbPath := storetest.CopyGoldenDB(t)
			root := filepath.Join(dir, "artifacts")
			open := func() *store.Store {
				var opts []store.OpenOption
				if c.Native {
					artifacts, err := store.NewOSGenerationArtifactStore(root)
					if err != nil {
						t.Fatal(err)
					}
					locks, err := store.NewFileSessionLocker(root)
					if err != nil {
						t.Fatal(err)
					}
					opts = append(opts, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locks))
				}
				db, err := store.Open(dbPath, opts...)
				if err != nil {
					t.Fatal(err)
				}
				return db
			}
			db := open()
			defer func() { _ = db.Close() }()
			cfg := makePipelineConfig(filepath.Join(dir, "managed"))
			cfg.Force = true
			cfg.Sources = map[ingest.Harness]ingest.SourceConfig{ingest.HarnessCodex: {Enabled: true, Paths: []ingest.ResolvedPath{sourcePath}}}
			cfg.AllowedSessionIDs = map[ingest.SessionID]bool{sid: true}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Indexed != 1 {
				t.Fatalf("source-valid evidence was not committed: %+v logs=%+v", result.Summary, result.IndexLog)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = open()
			snapshot, err := db.ReadSessionContent(t.Context(), string(sid))
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Capture.CaptureFormat != ingest.ContentCaptureFormatFull || snapshot.Capture.FailureCode != ingest.ContentCaptureUnknownDataRetained {
				t.Fatal("full local evidence was not certified")
			}
			selected := snapshot.Entries
			if c.Native {
				if err := db.WithSessionSnapshot(t.Context(), sid, func(generation indexformat.ReadSnapshot) error {
					selected = append([]schema.SessionEntry(nil), generation.Main.Entries...)
					for _, earlier := range generation.Earlier {
						selected = append(selected, earlier.Content.Entries...)
					}
					if c.Earlier && len(generation.Earlier) == 0 {
						t.Fatal("expected retained earlier partition")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			records, err := ingest.CollectRetainedUnknown(selected, ingest.HarnessCodex)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].Payload != payload {
				t.Fatal("reopened local evidence lost bytes")
			}
			encoded, err := json.Marshal(selected)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(encoded, []byte("opening")) || !bytes.Contains(encoded, []byte("closing")) {
				t.Fatal("known siblings lost")
			}
			if _, err := export.ExportSession(t.Context(), db, fs, string(sid)); err == nil || !strings.Contains(err.Error(), "8 MiB transfer limit") {
				t.Fatalf("export must explicitly refuse transfer size, not local retention: %v", err)
			}
			publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{MinPushContractVersion: "0.0.1", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: schema.AllContentCapabilities}}
			engine, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			pushCfg := &config.Config{Output: config.OutputConfig{BasePath: cfg.OutputDir.String()}, Push: config.PushConfig{Method: config.PushMethodAll, Visibility: config.VisibilityPrivate}}
			p, err := push.NewPipeline(db, publisher, &auth.Credentials{APIKey: "synthetic", KeyID: "synthetic", UserID: "synthetic", Username: "synthetic", VillageURL: "https://village.example.test"}, pushCfg, nil, push.PipelineConfig{Force: true, Concurrency: 1, FilterSessionIDs: []string{string(sid)}}, engine, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			published, err := p.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(publisher.Calls) != 0 || len(published.Sessions) != 1 || published.Sessions[0].Error == nil || !strings.Contains(published.Sessions[0].Error.Error(), "8 MiB transfer limit") {
				t.Fatalf("wrong transfer refusal: %+v", published)
			}
		})
	}
}
