package transcript_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/content_snapshot.yaml
var contentSnapshotYAML []byte

func TestContentSnapshotUsesOnlyCoherentCapturedInput(t *testing.T) {
	var fixture struct {
		Metadata      string   `yaml:"metadata"`
		Transcript    string   `yaml:"transcript"`
		Replacement   string   `yaml:"replacement"`
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name       string `yaml:"name"`
			SQL        string `yaml:"sql"`
			Replace    bool   `yaml:"replace"`
			NoMirror   bool   `yaml:"noMirror"`
			Refused    bool   `yaml:"refused"`
			Tool       bool   `yaml:"tool"`
			Transcript string `yaml:"transcript"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentSnapshotYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, row := range fixture.Cases {
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("content snapshot", "case", fixture.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			ctx := context.Background()
			fs := &ingest.OSFileSystem{}
			root := t.TempDir()
			db, err := store.Open(filepath.Join(t.TempDir(), "store.db"), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var meta ingest.UnifiedMetadata
			if err := json.Unmarshal([]byte(fixture.Metadata), &meta); err != nil {
				t.Fatal(err)
			}
			if row.Tool {
				meta.ModelHarness = ingest.HarnessCodex
			}
			path := ingest.SessionMetadataPath(root, string(meta.HostSlug), string(meta.SessionID), "")
			publish := func(body string, mirror bool) {
				t.Helper()
				options := ingest.ArtifactPublisherOptions{}
				if mirror {
					options.Mirror = db
				}
				publisher, err := ingest.NewArtifactPublisher(fs, root, options)
				if err != nil {
					t.Fatal(err)
				}
				meta.ContentHash = schema.ComputeTranscriptHash([]byte(body))
				meta.MetadataHash = schema.ComputeMetadataHash(&meta)
				data, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				artifact, err := ingest.NewManagedArtifact(data, []byte(body))
				if err != nil {
					t.Fatal(err)
				}
				observation, err := publisher.Observe(ctx, ingest.DiscoveredSession{SessionID: meta.SessionID, Harness: meta.ModelHarness}, path)
				if err != nil {
					t.Fatal(err)
				}
				committed, err := publisher.Publish(ctx, ingest.ArtifactPublication{Artifact: artifact, Observation: observation})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := publisher.Reconcile(ctx, committed); err != nil {
					t.Fatal(err)
				}
			}
			body := strings.Repeat("captured ", defaults.ContentPreviewLimit)
			source := fixture.Transcript
			if row.Transcript != "" {
				source = row.Transcript
			}
			publish(strings.ReplaceAll(source, "BODY", body), true)
			pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, ingest.PipelineConfig{OutputDir: ingest.ResolvedPath(root), Reindex: true, Parallelism: 1}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pipeline.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if row.SQL != "" {
				conn, err := db.Pool().Take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.ExecuteTransient(conn, row.SQL, nil)
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
			}
			if row.Replace {
				publish(fixture.Replacement, !row.NoMirror)
			}
			snapshot, err := transcript.ReadSessionContent(ctx, db, fs, root, string(meta.SessionID))
			if err != nil {
				t.Fatal(err)
			}
			if snapshot == nil || (snapshot.FullContentError != nil) != row.Refused {
				t.Fatalf("unexpected full-content outcome: %+v", snapshot)
			}
			turns, err := transcript.EntriesToTurnsValidated(snapshot.Entries)
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) == 0 {
				t.Fatal("stored preview was lost")
			}
			if row.Refused {
				if turns[0].Content == body || strings.Contains(turns[0].Content, "new retained input") {
					t.Fatal("unproven text escaped the stored preview")
				}
			} else if !row.Tool && turns[0].Content != body {
				t.Fatal("full content was not recovered from captured bytes")
			}
			viewer, err := api.NewStoreDataProviderWithFS(db, sessionvisibility.All(), fs, root).SessionByID(ctx, string(meta.SessionID))
			if err != nil || viewer == nil {
				t.Fatalf("viewer lost the readable SQL preview: %v", err)
			}
			payload, exportErr := export.ExportSession(ctx, db, fs, string(meta.SessionID), root)
			if (exportErr != nil) != row.Refused {
				t.Fatalf("export did not enforce full-content proof: %v", exportErr)
			}
			if row.Tool {
				found := false
				for _, turn := range viewer.Turns {
					for _, call := range turn.ToolCalls {
						if call.Result == body {
							found = true
						}
					}
				}
				if !found {
					t.Fatal("viewer folded bounded tool text before recovering full content")
				}
				encoded, err := json.Marshal(payload)
				if err != nil || !strings.Contains(string(encoded), body) {
					t.Fatal("export lost full tool output")
				}
			}
		})
	}
}
