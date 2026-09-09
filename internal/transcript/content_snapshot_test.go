package transcript_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
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

// TestContentSnapshotUsesOnlyCoherentCapturedInput proves that stored content
// is database-authoritative, and which reader answers which caller.
//
// It used to prove the opposite: that a retained managed FILE decided whether
// full content could be served, so deleting or replacing a sidecar made an
// intact SQLite capture unexportable. That is the prior-version file overlay,
// which now has no production caller. Every case below therefore states what
// the DATABASE holds and asserts both readers against it: the strict reader
// behind export and publication, and the available reader behind every mounted
// previewer. A file that changed, a parser input proof that went missing and a
// stale producer stamp are none of them content defects; an unfinished capture
// stops export without stopping a preview; only damaged data stops both.
func TestContentSnapshotUsesOnlyCoherentCapturedInput(t *testing.T) {
	var fixture struct {
		Metadata      string   `yaml:"metadata"`
		Transcript    string   `yaml:"transcript"`
		Replacement   string   `yaml:"replacement"`
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name           string `yaml:"name"`
			SQL            string `yaml:"sql"`
			Replace        bool   `yaml:"replace"`
			NoMirror       bool   `yaml:"noMirror"`
			StrictRefused  bool   `yaml:"strictRefused"`
			PreviewRefused bool   `yaml:"previewRefused"`
			Incomplete     bool   `yaml:"incomplete"`
			Tool           bool   `yaml:"tool"`
			Transcript     string `yaml:"transcript"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentSnapshotYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, row := range fixture.Cases {
		names[row.Name] = true
		if row.PreviewRefused && !row.StrictRefused {
			t.Fatalf("case %q refuses a preview but not the strict read, which no stored state can express", row.Name)
		}
		if row.Incomplete && !row.StrictRefused {
			t.Fatalf("case %q expects the incomplete-capture category without a strict refusal", row.Name)
		}
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
			// The strict reader: what export and publication are allowed to
			// certify. Only the database decides it.
			strict, strictErr := db.ReadSessionContent(ctx, string(meta.SessionID))
			if (strictErr != nil) != row.StrictRefused {
				t.Fatalf("strict database read outcome=%v, want refused=%t", strictErr, row.StrictRefused)
			}
			if row.Incomplete && !errors.Is(strictErr, store.ErrContentCaptureIncomplete) {
				t.Fatalf("an unfinished capture lost its error category: %v", strictErr)
			}
			if !row.StrictRefused {
				turns, err := transcript.EntriesToTurnsValidated(strict.Entries)
				if err != nil || len(turns) == 0 {
					t.Fatalf("verified content produced no turns: %v", err)
				}
				if !row.Tool && turns[0].Content != body {
					t.Fatal("a file that changed or a stale stamp cost the database its stored full content")
				}
			}
			// The available reader: what every mounted previewer shows. A
			// capture that is merely unfinished is still previewable.
			available, availableErr := db.ReadSessionAvailable(ctx, string(meta.SessionID))
			if (availableErr != nil) != row.PreviewRefused {
				t.Fatalf("available database read outcome=%v, want refused=%t", availableErr, row.PreviewRefused)
			}
			if !row.PreviewRefused {
				turns, err := transcript.EntriesToTurnsValidated(available.Entries)
				if err != nil || len(turns) == 0 {
					t.Fatal("the previewer lost the content the database still holds")
				}
				if strings.Contains(turns[0].Content, "new retained input") {
					t.Fatal("unstored text from a replaced file escaped into the preview")
				}
			}
			viewer, viewerErr := api.NewStoreDataProviderWithFS(db, sessionvisibility.All(), fs, root).SessionByID(ctx, string(meta.SessionID))
			if (viewerErr != nil) != row.PreviewRefused {
				t.Fatalf("the mounted viewer disagreed with the available reader: %v", viewerErr)
			}
			payload, exportErr := export.ExportSession(ctx, db, fs, string(meta.SessionID), root)
			if (exportErr != nil) != row.StrictRefused {
				t.Fatalf("export did not enforce verified database content: %v", exportErr)
			}
			if row.StrictRefused {
				return
			}
			// The prior-version file overlay still runs for the one caller that
			// has only a file. It is asserted here so it cannot rot unnoticed,
			// and never as the oracle for what a consumer may serve.
			if row.Name == "coherent-full-content" {
				legacy, legacyErr := transcript.ReadSessionContent(ctx, db, fs, root, string(meta.SessionID))
				if legacyErr != nil || legacy == nil || len(legacy.Entries) == 0 {
					t.Fatalf("prior-version file overlay stopped returning the stored snapshot: %+v %v", legacy, legacyErr)
				}
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
