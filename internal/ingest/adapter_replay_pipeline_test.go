package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
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

type replayPipelineFixtures struct {
	Metadata      string   `yaml:"publicationMetadata"`
	Transcript    string   `yaml:"publicationTranscript"`
	Append        string   `yaml:"nativeAppend"`
	RequiredNames []string `yaml:"pipelineRequiredNames"`
	Cases         []struct {
		Name             string `yaml:"name"`
		Target           int    `yaml:"target"`
		Reindex          bool   `yaml:"reindex"`
		Native           bool   `yaml:"native"`
		Appended         bool   `yaml:"appended"`
		FailRead         bool   `yaml:"failRead"`
		DiscoveryFailure bool   `yaml:"discoveryFailure"`
		NoSeed           bool   `yaml:"noSeed"`
		Replay           bool   `yaml:"replay"`
		Warning          bool   `yaml:"warning"`
		Fatal            bool   `yaml:"fatal"`
		UnknownClock     bool   `yaml:"unknownClock"`
	} `yaml:"pipelineCases"`
}

func LoadReplayPipelineFixtures(t *testing.T) replayPipelineFixtures {
	t.Helper()
	var fixture replayPipelineFixtures
	if err := yaml.Unmarshal(adapterReplayYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid replay case %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing replay case %q", name)
		}
	}
	return fixture
}

type replaySourceFS struct {
	*ingest.OSFileSystem
	nativePath string
	failRead   bool
}

func (filesystem *replaySourceFS) ReadFile(path string) ([]byte, error) {
	if path == filesystem.nativePath && filesystem.failRead {
		return nil, os.ErrPermission
	}
	return filesystem.OSFileSystem.ReadFile(path)
}

var _ ingest.FileSystem = (*replaySourceFS)(nil)

type replayDiscoveryAdapter struct {
	ingest.SourceAdapter
	sessions         []ingest.DiscoveredSession
	discoveryFailure bool
	replayed         bool
	extracted        bool
}

func (a *replayDiscoveryAdapter) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	if a.discoveryFailure {
		return nil, errors.New("fixture native discovery failed")
	}
	return a.sessions, nil
}

func (a *replayDiscoveryAdapter) ExtractMetadata(ctx context.Context, session ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	a.extracted = true
	return a.SourceAdapter.ExtractMetadata(ctx, session)
}

func (a *replayDiscoveryAdapter) ReplayRetained(ctx context.Context, data []byte, metadata *ingest.UnifiedMetadata) (*ingest.UnifiedMetadata, []byte, error) {
	a.replayed = true
	return a.SourceAdapter.(ingest.RetainedInputReplayer).ReplayRetained(ctx, data, metadata)
}

var _ ingest.SourceAdapter = (*replayDiscoveryAdapter)(nil)
var _ ingest.RetainedInputReplayer = (*replayDiscoveryAdapter)(nil)

func TestPipelineRetainedAdapterMaintenance(t *testing.T) {
	fixture := LoadReplayPipelineFixtures(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			output := filepath.Join(root, "managed")
			native := filepath.Join(root, "native.jsonl")
			filesystem := &replaySourceFS{OSFileSystem: &ingest.OSFileSystem{}, nativePath: native}
			database, err := store.Open(filepath.Join(root, "peasant.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			var metadata ingest.UnifiedMetadata
			if err := json.Unmarshal([]byte(fixture.Metadata), &metadata); err != nil {
				t.Fatal(err)
			}
			metadata.Source.FilePath = native
			if row.UnknownClock {
				unknown := int64(0)
				metadata.Timestamp.Ingested = &unknown
			}
			encoded, err := json.Marshal(&metadata)
			if err != nil {
				t.Fatal(err)
			}
			artifact, err := ingest.NewManagedArtifact(encoded, []byte(fixture.Transcript))
			if err != nil {
				t.Fatal(err)
			}
			sid := metadata.SessionID
			session := ingest.DiscoveredSession{SessionID: sid, Harness: metadata.ModelHarness, SourcePath: ingest.ResolvedPath(native), SourceFormat: ingest.SourceFormatJSONL}
			publisher, err := ingest.NewArtifactPublisher(filesystem, output, ingest.ArtifactPublisherOptions{Mirror: database})
			if err != nil {
				t.Fatal(err)
			}
			path := ingest.SessionMetadataPath(output, string(metadata.HostSlug), string(sid), "")
			if !row.NoSeed {
				observation, err := publisher.Observe(t.Context(), session, "")
				if err != nil {
					t.Fatal(err)
				}
				committed, err := publisher.Publish(t.Context(), ingest.ArtifactPublication{Artifact: artifact, Observation: observation})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := publisher.Reconcile(t.Context(), committed); err != nil {
					t.Fatal(err)
				}
				// A real initial index makes adapter-only maintenance observable.
				baselineConfig := makePipelineConfig(output)
				baselineConfig.Reindex = true
				baselineConfig.Sources = nil
				baseline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, baselineConfig, ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := baseline.Run(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			adapter := &replayDiscoveryAdapter{SourceAdapter: ingest.NewClaudeAdapter(filesystem, testutil.DefaultGitResolver(), salt.Salt{}), discoveryFailure: row.DiscoveryFailure}
			if row.Native {
				data := fixture.Transcript + "\n"
				if row.Appended {
					data += fixture.Append + "\n"
				}
				if err := os.WriteFile(native, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				changed := time.UnixMilli(*metadata.Timestamp.Ingested + 1000)
				if err := os.Chtimes(native, changed, changed); err != nil {
					t.Fatal(err)
				}
				session.ModTime = changed
				adapter.sessions = []ingest.DiscoveredSession{session}
			}
			filesystem.failRead = row.FailRead
			versions := maps.Clone(ingest.HarvesterVersionRegistry)
			target := versions[ingest.HarnessClaudeCode]
			target.AdapterVersion = row.Target
			versions[ingest.HarnessClaudeCode] = target
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter }}
			config := makePipelineConfig(output)
			config.Reindex = row.Reindex
			pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), adapters, config, ingest.WithHarvesterVersions(versions), ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if row.Fatal {
				if err == nil {
					t.Fatal("initial source failure was not fatal")
				}
				return
			}
			if err != nil || result.Summary.Errors != 0 {
				t.Fatalf("maintenance failed: %+v %v", result, err)
			}
			if row.Warning && len(result.Diagnostics) == 0 {
				t.Fatal("source refresh failure was not reported")
			}
			if row.Replay != adapter.replayed {
				t.Fatalf("retained adapter path selected=%t, expected=%t", adapter.replayed, row.Replay)
			}
			if row.Native && !adapter.extracted {
				t.Fatal("native change did not take precedence")
			}
			if !row.Native && adapter.extracted {
				t.Fatal("retained replay read the native source")
			}
			locations, err := database.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sid})
			if err != nil {
				t.Fatal(err)
			}
			location := locations[sid]
			path = ingest.SessionMetadataPath(output, string(location.HostSlug), string(sid), "")
			current, err := publisher.Capture(t.Context(), sid, path)
			if err != nil {
				t.Fatal(err)
			}
			if row.Replay || row.Native && !row.FailRead {
				if current.Metadata.AdapterVersion == nil || *current.Metadata.AdapterVersion != row.Target {
					t.Fatal("successful adapter execution was not stamped")
				}
			} else if current.Metadata.AdapterVersion != nil {
				t.Fatal("failed or skipped adapter received a success stamp")
			}
			if row.Replay || row.FailRead || !row.Native {
				if !reflect.DeepEqual(current.Metadata.Timestamp.Ingested, metadata.Timestamp.Ingested) || !bytes.Equal(current.Transcript, artifact.Transcript) {
					t.Fatal("native-free processing advanced the acquired clock or replaced retained input")
				}
			}
			if row.Native && !row.FailRead && !bytes.Contains(current.Transcript, []byte(fixture.Append)) {
				t.Fatal("new native append was lost")
			}
			cursors, err := database.BulkLookupOpenCodeSeqCursors(t.Context(), []ingest.SessionID{sid})
			if err != nil || len(cursors) != 0 {
				t.Fatalf("native-free replay invented acquired cursor evidence: %v %v", cursors, err)
			}
			entries, err := database.ListEntries(t.Context(), sid)
			if err != nil || len(entries) == 0 {
				t.Fatalf("usable index was lost: %v", err)
			}
			if row.UnknownClock {
				if err := os.WriteFile(native, []byte(fixture.Transcript+"\n"+fixture.Append+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				changed := time.Now().Add(-time.Hour)
				if err := os.Chtimes(native, changed, changed); err != nil {
					t.Fatal(err)
				}
				session.ModTime = changed
				adapter.sessions = []ingest.DiscoveredSession{session}
				adapter.replayed = false
				if _, err := pipeline.Run(t.Context()); err != nil {
					t.Fatal(err)
				}
				if !adapter.extracted || adapter.replayed {
					t.Fatalf("retained processing hid an unacquired native append: native=%t retained=%t", adapter.extracted, adapter.replayed)
				}
			}
		})
	}
}
