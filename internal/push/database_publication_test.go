package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/database_publication.yaml
var databasePublicationYAML []byte

// Any filesystem operation panics: complete publication has no filesystem exit.
type forbiddenPublicationFS struct{ ingest.FileSystem }

var _ ingest.FileSystem = forbiddenPublicationFS{}

func TestDatabasePublicationWithoutSourcesOrSidecars(t *testing.T) {
	var cases []struct {
		Name              string `yaml:"name"`
		CWD               string `yaml:"cwd"`
		Child             bool   `yaml:"child"`
		Incomplete        bool   `yaml:"incomplete"`
		InvalidateEntries bool   `yaml:"invalidateEntries"`
		MissingModel      bool   `yaml:"missingModel"`
		CloseDatabase     bool   `yaml:"closeDatabase"`
	}
	if err := yaml.Unmarshal(databasePublicationYAML, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		seen[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("database publication", "case", strings.Fields("ready-with-exact-cwd ready-with-source-confirmed-absence ready-child-without-sidecar legacy-capture-needs-ingest mismatched-index-needs-ingest missing-model-refuses database-unavailable-refuses"), seen); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "peasant.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			meta := ingest.NewUnifiedMetadata()
			meta.SessionID = testutil.TestSessionUUID
			meta.HostSlug = testutil.TestHostSlug
			meta.ModelHarness = defaults.HarnessClaudeCode
			meta.Model = testutil.TestModel
			meta.CWD = tc.CWD
			meta.Source = ingest.SourceInfo{FilePath: "/nonexistent/source.jsonl", Format: ingest.SourceFormatJSONL}
			meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "synthetic-project"}
			ingested := int64(1700000120000)
			meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested}
			meta.Stats = ingest.StatsInfo{TurnCount: 1, DurationMs: 60000}
			if tc.Child {
				parent := meta
				parent.SessionID = testutil.TestSessionUUID2
				testutil.SeedReadyPublication(t, db, &parent, nil)
				meta.ParentUUID = &parent.SessionID
			}
			if tc.MissingModel {
				meta.Model = ""
			}
			const secret = "sk-ant-api03-DATABASECAPTUREKEY00000000000x"
			content := "stored entry " + secret
			entries := []schema.SessionEntry{{SessionID: meta.SessionID, EntryIndex: 1, Harness: meta.ModelHarness, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &content}}
			if tc.Incomplete {
				if err := db.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: &meta}}); err != nil {
					t.Fatal(err)
				}
			} else {
				testutil.SeedReadyPublication(t, db, &meta, entries)
			}
			if tc.InvalidateEntries {
				if err := db.IndexSessionEntries(ctx, meta.SessionID, entries); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if tc.CloseDatabase {
				db.Close()
			}
			redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			publisher := &testutil.StubPublisher{StatusCode: 201}
			var output bytes.Buffer
			run := func(dry bool) (*push.PushResult, error) {
				pipeline, err := push.NewPipeline(db, publisher, baseCreds(), baseTestConfig(), forbiddenPublicationFS{}, push.PipelineConfig{Force: true, DryRun: dry, FilterSessionIDs: []string{meta.SessionID.String()}}, redactor, &output)
				if err != nil {
					t.Fatal(err)
				}
				return pipeline.Run(ctx)
			}
			result, runErr := run(true)
			failed := tc.Incomplete || tc.InvalidateEntries || tc.MissingModel || tc.CloseDatabase
			if !failed && (runErr != nil || result.Errors != 0 || result.New != 1) {
				t.Fatalf("dry run: %+v %v", result, runErr)
			}
			if len(publisher.Calls) != 0 {
				t.Fatal("dry run uploaded")
			}
			result, runErr = run(false)
			if failed {
				if runErr == nil && result.Errors == 0 {
					t.Fatalf("failed input accepted: %+v", result)
				}
				if tc.CloseDatabase {
					// An unusable database is a fact about the RUN. It must stop
					// the push with one run-level error, never become a row per
					// candidate telling the user to re-ingest each session: that
					// advice cannot repair a closed store, and a run that keeps
					// walking an unreadable database reports refusals it has no
					// evidence for.
					if runErr == nil {
						t.Fatalf("closed database did not stop the run: %+v", result)
					}
					if result != nil {
						for _, session := range result.Sessions {
							if push.ClassifyPushError(session.Error) == push.CategoryMetadataMissing {
								t.Fatalf("closed database blamed session %s for needing ingest: %v", session.SessionID, session.Error)
							}
						}
					}
				}
				if len(publisher.Calls) != 0 {
					t.Fatal("failed input uploaded")
				}
				if !tc.CloseDatabase {
					receipt, err := db.Publication(ctx, baseCreds().VillageURL, baseCreds().UserID, meta.Project.Hash, meta.SessionID.String())
					if err != nil || receipt != nil {
						t.Fatalf("failed input receipt=%+v err=%v", receipt, err)
					}
				}
				return
			}
			if runErr != nil || result.Errors != 0 || len(publisher.Calls) != 1 {
				t.Fatalf("real publish: %+v %v calls=%d", result, runErr, len(publisher.Calls))
			}
			call := publisher.Calls[0]
			if bytes.Contains(call.MetadataJSON, []byte(secret)) || !bytes.Contains(call.MetadataJSON, []byte("ANTHROPIC_KEY")) {
				t.Fatal("metadata entry redaction missing")
			}
			publisher.StatusCode = 200
			result, runErr = run(false)
			if runErr != nil || result.Errors != 0 || len(publisher.Calls) != 2 {
				t.Fatalf("repeat publish: %+v %v", result, runErr)
			}
			if !bytes.Equal(publisher.Calls[0].MetadataJSON, publisher.Calls[1].MetadataJSON) {
				t.Fatal("repeat changed metadata identity")
			}
		})
	}
}
