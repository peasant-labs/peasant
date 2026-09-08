package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/artifact_startup.yaml
var artifactStartupYAML []byte

type artifactStartupFixtures struct {
	RequiredNames []string `yaml:"requiredNames"`
	ParentID      string   `yaml:"parentID"`
	ChildID       string   `yaml:"childID"`
	Transcript    string   `yaml:"transcript"`
	Cases         []struct {
		Name       string `yaml:"name"`
		FailMirror bool   `yaml:"failMirror"`
		DryRun     bool   `yaml:"dryRun"`
		Reindex    bool   `yaml:"reindex"`
		OnlyParent bool   `yaml:"onlyParent"`
	} `yaml:"cases"`
}

func LoadArtifactStartupFixtures(t *testing.T) artifactStartupFixtures {
	t.Helper()
	var fixture artifactStartupFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(artifactStartupYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("startup fixture requires one YAML document")
	}
	required := []string{"logs-bootstrap", "failed-mirror-restart", "dry-run-preserves-pending", "explicit-scope-keeps-child-out"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("startup required-name manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid startup case %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing startup case %q", name)
		}
	}
	return fixture
}

type startupMirrorStore struct {
	*store.Store
	fail bool
}

func (s *startupMirrorStore) MirrorArtifacts(ctx context.Context, requests []ingest.ArtifactMirrorRequest) []ingest.ArtifactMirrorResult {
	if !s.fail {
		return s.Store.MirrorArtifacts(ctx, requests)
	}
	results := make([]ingest.ArtifactMirrorResult, len(requests))
	for index, request := range requests {
		results[index] = ingest.ArtifactMirrorResult{SessionID: request.Artifact.Metadata.SessionID, Err: errors.New("synthetic unavailable database mirror")}
	}
	return results
}

func TestPipelineReconcilesRetainedArtifactsBeforeSelection(t *testing.T) {
	fixture := LoadArtifactStartupFixtures(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			output := filepath.Join(root, "managed")
			filesystem := &ingest.OSFileSystem{}
			database, err := store.Open(filepath.Join(root, "peasant.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			mirror := &startupMirrorStore{Store: database, fail: row.FailMirror}
			parent := makeDiscoveredSession(t, fixture.ParentID, filepath.Join(root, "parent.jsonl"), time.Now().Add(-time.Hour))
			child := makeDiscoveredSession(t, fixture.ChildID, filepath.Join(root, "child.jsonl"), parent.ModTime)
			child.ParentUUID = &parent.SessionID
			for _, session := range []ingest.DiscoveredSession{parent, child} {
				if err := os.WriteFile(session.SourcePath.String(), []byte(fixture.Transcript), 0600); err != nil {
					t.Fatal(err)
				}
			}
			parentMeta := makeReindexMeta(t, fixture.ParentID, parent.SourcePath.String())
			childMeta := makeReindexMeta(t, fixture.ChildID, child.SourcePath.String())
			childMeta.ParentUUID = &parent.SessionID
			config := makePipelineConfig(output)
			var seedOptions []ingest.PipelineOption
			if row.FailMirror {
				seedOptions = append(seedOptions, ingest.WithStore(mirror))
			}
			seed, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{parent, child}, map[ingest.SessionID]*ingest.UnifiedMetadata{parent.SessionID: parentMeta, child.SessionID: childMeta}),
			}, config, seedOptions...)
			if err != nil {
				t.Fatal(err)
			}
			seeded, err := seed.Run(t.Context())
			if err != nil || seeded.Summary.New != 2 || seeded.Summary.Errors != 0 {
				t.Fatalf("seed committed files: %+v %v", seeded, err)
			}
			if row.FailMirror && seeded.Summary.StoreError == nil {
				t.Fatal("mirror failure was not exercised")
			}
			publisher, err := ingest.NewArtifactPublisher(filesystem, output, ingest.ArtifactPublisherOptions{Mirror: database})
			if err != nil {
				t.Fatal(err)
			}
			beforePending, err := publisher.PendingSessions()
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{ingest.SessionMetadataPath(output, string(parentMeta.HostSlug), fixture.ParentID, ""), ingest.SessionMetadataPath(output, string(childMeta.HostSlug), fixture.ChildID, fixture.ParentID)}
			before := make(map[string][]byte)
			for _, path := range paths {
				before[path], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			mirror.fail = false
			config.Sources = nil
			config.Reindex, config.DryRun = row.Reindex, row.DryRun
			config.SessionFilter = func(ingest.DiscoveredSession) bool { return false }
			if row.OnlyParent {
				config.AllowedSessionIDs = map[ingest.SessionID]bool{parent.SessionID: true}
			}
			adapter := &metadataPolicyAdapter{StubAdapter: &testutil.StubAdapter{ProviderValue: ingest.HarnessClaudeCode}}
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter }}
			pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), adapters, config, ingest.WithStore(mirror), ingest.WithMetricsStore(database), ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for index, sid := range []ingest.SessionID{parent.SessionID, child.SessionID} {
				state, err := database.ReadIndexState(t.Context(), sid)
				if err != nil {
					t.Fatal(err)
				}
				want := !row.DryRun && (!row.OnlyParent || sid == parent.SessionID)
				if want {
					captured, err := publisher.Capture(t.Context(), sid, paths[index])
					if err != nil {
						t.Fatal(err)
					}
					if state == nil || state.ArtifactHash == nil || *state.ArtifactHash != captured.ArtifactHash || captured.Metadata.DerivedAt == nil {
						t.Fatalf("committed artifact not mirrored: %s %+v diagnostics=%+v", sid, state, result.Diagnostics)
					}
					entries, err := database.ListEntries(t.Context(), sid)
					if err != nil || len(entries) == 0 {
						t.Fatalf("retained bootstrap did not index %s: %v", sid, err)
					}
				} else {
					if state != nil {
						t.Fatalf("out-of-scope or dry-run session was mirrored: %+v", state)
					}
					after, err := os.ReadFile(paths[index])
					if err != nil || !bytes.Equal(before[paths[index]], after) {
						t.Fatal("read-only/scoped metadata changed")
					}
				}
			}
			pending, err := publisher.PendingSessions()
			if err != nil {
				t.Fatal(err)
			}
			if row.DryRun {
				if !reflect.DeepEqual(pending, beforePending) {
					t.Fatal("dry-run recovered pending state")
				}
			} else {
				if len(pending) != 0 {
					t.Fatalf("startup retained completed intents: %v", pending)
				}
				for _, path := range paths {
					before[path], err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := pipeline.Run(t.Context()); err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					after, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(before[path], after) {
						t.Fatal("equal hash changed metadata/DerivedAt")
					}
				}
			}
			for _, session := range []ingest.DiscoveredSession{parent, child} {
				data, err := os.ReadFile(session.SourcePath.String())
				if err != nil || string(data) != fixture.Transcript {
					t.Fatal("native source changed")
				}
			}
			if adapter.extracts.Load() != 0 {
				t.Fatal("retained reconciliation invoked native extraction")
			}
		})
	}
}
