package ingest_test

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/publication_capture_child.yaml
var publicationCaptureChildYAML []byte

func TestPublicationCaptureParentRecoveryPreservesChild(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name     string `yaml:"name"`
			ParentID string `yaml:"parent_id"`
			ChildID  string `yaml:"child_id"`
			Parent   string `yaml:"parent"`
			Child    string `yaml:"child"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(publicationCaptureChildYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if seen[c.Name] {
			t.Fatalf("duplicate fixture %s", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			root, output := t.TempDir(), t.TempDir()
			fs := &ingest.OSFileSystem{}
			parentID, err := ingest.NewSessionID(c.ParentID)
			if err != nil {
				t.Fatal(err)
			}
			childID, err := ingest.NewSessionID(c.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			parentSource := filepath.Join(root, "project", c.ParentID+".jsonl")
			childSource := filepath.Join(root, "project", c.ParentID, "subagents", c.ChildID+".jsonl")
			if err := os.MkdirAll(filepath.Dir(childSource), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(parentSource, []byte(c.Parent), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(childSource, []byte(c.Child), 0600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(parentSource, old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(childSource, old, old); err != nil {
				t.Fatal(err)
			}
			database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			cfg := ingest.PipelineConfig{OutputDir: ingest.ResolvedPath(output), Parallelism: 2, Sources: map[ingest.Harness]ingest.SourceConfig{
				ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(root)}},
			}}
			run := func() *ingest.PipelineResult {
				pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, cfg,
					ingest.WithSalt(database.InstallationSalt()), ingest.WithStore(database), ingest.WithMetricsStore(database),
					ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessClaudeCode: ingest.NewClaudeIndexer(fs)}), ingest.WithAnalyzer(metrics.NewEngine(database)))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(t.Context())
				if err != nil || result.Summary.Errors != 0 || result.Summary.StoreError != nil {
					t.Fatalf("ingest: %+v, %v", result, err)
				}
				return result
			}
			run()
			parent, err := database.LoadPublicationInput(t.Context(), parentID)
			if err != nil || parent.Readiness != ingest.PublicationReady {
				t.Fatalf("parent capture: %+v, %v", parent, err)
			}
			child, err := database.LoadPublicationInput(t.Context(), childID)
			if err != nil || child.Readiness != ingest.PublicationReady || child.Metadata.ParentUUID == nil || *child.Metadata.ParentUUID != parentID {
				t.Fatalf("child capture: %+v, %v", child, err)
			}
			childDir := ingest.SessionDir(output, string(child.Metadata.HostSlug), c.ChildID, c.ParentID)
			childMetadata := filepath.Join(childDir, c.ChildID+"--metadata.json")
			childTranscript := filepath.Join(childDir, c.ChildID+"--transcript.jsonl")
			beforeMeta, err := os.ReadFile(childMetadata)
			if err != nil {
				t.Fatal(err)
			}
			beforeTranscript, err := os.ReadFile(childTranscript)
			if err != nil {
				t.Fatal(err)
			}
			// Invalidate only the parent's proof via an old metadata writer and
			// remove its optional sidecar. The child's capture is still current.
			if err := database.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &parent.Metadata, Session: ingest.DiscoveredSession{SessionID: parentID, Harness: ingest.HarnessClaudeCode}}}); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(ingest.SessionDir(output, string(parent.Metadata.HostSlug), c.ParentID, ""), c.ParentID+"--metadata.json")); err != nil {
				t.Fatal(err)
			}
			result := run()
			if result.Summary.Updated != 1 || result.Summary.Unchanged != 1 {
				t.Fatalf("parent-only recovery = %+v", result.Summary)
			}
			after, err := database.LoadPublicationInput(t.Context(), childID)
			if err != nil || after.Readiness != ingest.PublicationReady || after.CaptureRevision != child.CaptureRevision || !reflect.DeepEqual(after.Entries, child.Entries) {
				t.Fatalf("child rewritten during parent recovery: %+v, %v", after, err)
			}
			assertFileBytes(t, fs, childMetadata, beforeMeta)
			assertFileBytes(t, fs, childTranscript, beforeTranscript)
			assertFileBytes(t, fs, parentSource, []byte(c.Parent))
			assertFileBytes(t, fs, childSource, []byte(c.Child))
		})
	}
	if !seen["parent_recovery_preserves_ready_child"] {
		t.Fatal("missing required parent recovery fixture")
	}
}
