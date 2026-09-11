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

type adapterMaintenanceFixtures struct {
	Metadata      string   `yaml:"publicationMetadata"`
	Transcript    string   `yaml:"publicationTranscript"`
	Append        string   `yaml:"nativeAppend"`
	RequiredNames []string `yaml:"pipelineRequiredNames"`
	Cases         []struct {
		Name               string `yaml:"name"`
		Target             int    `yaml:"target"`
		Reindex            bool   `yaml:"reindex"`
		Native             bool   `yaml:"native"`
		Appended           bool   `yaml:"appended"`
		FailRead           bool   `yaml:"failRead"`
		DiscoveryFailure   bool   `yaml:"discoveryFailure"`
		MissingMetadata    bool   `yaml:"missingMetadata"`
		NoSeed             bool   `yaml:"noSeed"`
		RetainedExtraction bool   `yaml:"retainedExtraction"`
		Warning            bool   `yaml:"warning"`
		Fatal              bool   `yaml:"fatal"`
		UnknownClock       bool   `yaml:"unknownClock"`
	} `yaml:"pipelineCases"`
}

func LoadAdapterMaintenanceFixtures(t *testing.T) adapterMaintenanceFixtures {
	t.Helper()
	var fixture adapterMaintenanceFixtures
	if err := yaml.Unmarshal(adapterTranscriptYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid adapter maintenance case %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing adapter maintenance case %q", name)
		}
	}
	return fixture
}

type adapterMaintenanceFS struct {
	*ingest.OSFileSystem
	nativePath string
	failRead   bool
}

func (filesystem *adapterMaintenanceFS) ReadFile(path string) ([]byte, error) {
	if path == filesystem.nativePath && filesystem.failRead {
		return nil, os.ErrPermission
	}
	return filesystem.OSFileSystem.ReadFile(path)
}

var _ ingest.FileSystem = (*adapterMaintenanceFS)(nil)

type adapterMaintenanceAdapter struct {
	ingest.SourceAdapter
	sessions            []ingest.DiscoveredSession
	discoveryFailure    bool
	transcriptExtracted bool
	extracted           bool
}

func (a *adapterMaintenanceAdapter) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	if a.discoveryFailure {
		return nil, errors.New("fixture native discovery failed")
	}
	return a.sessions, nil
}

func (a *adapterMaintenanceAdapter) ExtractMetadata(ctx context.Context, session ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	a.extracted = true
	return a.SourceAdapter.ExtractMetadata(ctx, session)
}

func (a *adapterMaintenanceAdapter) ExtractMetadataFromTranscript(ctx context.Context, data []byte, metadata *ingest.UnifiedMetadata) (*ingest.UnifiedMetadata, error) {
	a.transcriptExtracted = true
	return a.SourceAdapter.(ingest.TranscriptMetadataExtractor).ExtractMetadataFromTranscript(ctx, data, metadata)
}

var _ ingest.SourceAdapter = (*adapterMaintenanceAdapter)(nil)
var _ ingest.TranscriptMetadataExtractor = (*adapterMaintenanceAdapter)(nil)

func TestPipelineRetainedAdapterMaintenance(t *testing.T) {
	fixture := LoadAdapterMaintenanceFixtures(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			output := filepath.Join(root, "managed")
			native := filepath.Join(root, "native.jsonl")
			filesystem := &adapterMaintenanceFS{OSFileSystem: &ingest.OSFileSystem{}, nativePath: native}
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
			path := ingest.SessionMetadataPath(output, string(metadata.HostSlug), string(sid), "")
			if !row.NoSeed {
				sessionDir := ingest.SessionDir(output, string(metadata.HostSlug), string(sid), "")
				if err := filesystem.MkdirAll(sessionDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := filesystem.WriteFile(filepath.Join(sessionDir, string(sid)+"--transcript.jsonl"), artifact.Transcript, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := filesystem.WriteFile(path, artifact.MetadataJSON, 0o600); err != nil {
					t.Fatal(err)
				}
				if results := database.MirrorArtifacts(t.Context(), []ingest.ArtifactMirrorRequest{{Artifact: artifact}}); len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
					t.Fatalf("seed mirror: %+v", results)
				}
				// A real initial index makes adapter-only maintenance observable.
				baselineConfig := makePipelineConfig(output)
				baselineConfig.Reindex = true
				baselineConfig.Sources = nil
				baseline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, baselineConfig, ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
				if err != nil {
					t.Fatal(err)
				}
				baselineResult, err := baseline.Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				// The retained transcript cannot backfill an absent projection.
				// Report that failure, then let ordinary indexing establish it.
				recoveryReported := false
				for _, diagnostic := range baselineResult.Diagnostics {
					if diagnostic.ErrorType == "content_recovery_unavailable" {
						recoveryReported = true
					}
				}
				if !recoveryReported {
					t.Fatal("retained content recovery failure was not reported")
				}
				state, err := database.ReadIndexState(t.Context(), sid)
				if err != nil || state == nil || state.IndexedInputHash == nil || state.IndexerVersion != ingest.HarvesterVersionRegistry[metadata.ModelHarness].IndexerVersion {
					t.Fatalf("failed content recovery excluded supported fallback indexing: %+v %v", state, err)
				}
			}
			adapter := &adapterMaintenanceAdapter{SourceAdapter: ingest.NewClaudeAdapter(filesystem, testutil.DefaultGitResolver(), salt.Salt{}), discoveryFailure: row.DiscoveryFailure}
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
			if row.MissingMetadata {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target.IndexerVersion++
			}
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
			if row.RetainedExtraction != adapter.transcriptExtracted {
				t.Fatalf("retained adapter path selected=%t, expected=%t", adapter.transcriptExtracted, row.RetainedExtraction)
			}
			if row.Native && !adapter.extracted {
				t.Fatal("native change did not take precedence")
			}
			if !row.Native && adapter.extracted {
				t.Fatal("retained extraction read the native source")
			}
			locations, err := database.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sid})
			if err != nil {
				t.Fatal(err)
			}
			location := locations[sid]
			path = ingest.SessionMetadataPath(output, string(location.HostSlug), string(sid), "")
			current, err := ingest.ReadManagedPair(filesystem, output, path, sid)
			if err != nil {
				t.Fatal(err)
			}
			if row.RetainedExtraction || row.Native && !row.FailRead {
				if current.Metadata.AdapterVersion == nil || *current.Metadata.AdapterVersion != row.Target {
					t.Fatal("successful adapter execution was not stamped")
				}
			} else if current.Metadata.AdapterVersion != nil {
				t.Fatal("failed or skipped adapter received a success stamp")
			}
			if row.RetainedExtraction || row.FailRead || !row.Native {
				if !reflect.DeepEqual(current.Metadata.Timestamp.Ingested, metadata.Timestamp.Ingested) || !bytes.Equal(current.Transcript, artifact.Transcript) {
					t.Fatal("native-free processing advanced the acquired clock or replaced retained input")
				}
			}
			if row.Native && !row.FailRead && !bytes.Contains(current.Transcript, []byte(fixture.Append)) {
				t.Fatal("new native append was lost")
			}
			cursors, err := database.BulkLookupOpenCodeSeqCursors(t.Context(), []ingest.SessionID{sid})
			if err != nil || len(cursors) != 0 {
				t.Fatalf("retained extraction invented acquired cursor evidence: %v %v", cursors, err)
			}
			entries, err := database.ListEntries(t.Context(), sid)
			if err != nil || len(entries) == 0 {
				t.Fatalf("usable index was lost: %v", err)
			}
			if row.MissingMetadata {
				state, err := database.ReadIndexState(t.Context(), sid)
				if err != nil || state == nil || state.IndexerVersion != target.IndexerVersion || state.IndexedInputHash == nil {
					t.Fatalf("recovered input did not complete indexing: %+v %v", state, err)
				}
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
				adapter.transcriptExtracted = false
				if _, err := pipeline.Run(t.Context()); err != nil {
					t.Fatal(err)
				}
				if !adapter.extracted || adapter.transcriptExtracted {
					t.Fatalf("retained processing hid an unacquired native append: native=%t retained=%t", adapter.extracted, adapter.transcriptExtracted)
				}
			}
		})
	}
}
