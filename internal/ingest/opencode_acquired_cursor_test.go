package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/opencode_acquired_cursor.yaml
var acquiredCursorYAML []byte

type acquiredCursorFixtures struct {
	RequiredNames []string         `yaml:"requiredNames"`
	SourceFixture string           `yaml:"sourceFixture"`
	SessionID     ingest.SessionID `yaml:"sessionID"`
	Setup         string           `yaml:"setup"`
	Cases         []struct {
		Name           string `yaml:"name"`
		Change         string `yaml:"change"`
		StoredCursor   int64  `yaml:"storedCursor"`
		WantCursor     int64  `yaml:"wantCursor"`
		WantError      bool   `yaml:"wantError"`
		WantDiagnostic bool   `yaml:"wantDiagnostic"`
	} `yaml:"cases"`
}

func LoadAcquiredCursorFixtures(t *testing.T) acquiredCursorFixtures {
	t.Helper()
	var fixture acquiredCursorFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(acquiredCursorYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("cursor fixture requires one YAML document")
	}
	required := []string{"acquired-zero-then-newer", "changed-attribution-preserves-artifact", "missing-cursor-preserves-progress"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("cursor required-name manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid cursor case %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing cursor case %q", name)
		}
	}
	return fixture
}

// Setup writes are restricted to the test materializer's synthetic sources;
// SnapshotSource verifies that ownership before opening the fixture connection.
func applyAcquiredCursorSetup(t *testing.T, source testfixture.MaterializedSource, script string) {
	t.Helper()
	_ = testfixture.SnapshotSource(t, source)
	connection, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := sqlitex.ExecuteScript(connection, script, nil); err != nil {
		t.Fatal(err)
	}
}

type discoveredCursorAdapter struct {
	*ingest.OpenCodeAdapter
	session ingest.DiscoveredSession
}

func (a *discoveredCursorAdapter) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	return []ingest.DiscoveredSession{a.session}, nil
}

func TestPipelineStoresOnlyAcquiredOpenCodeCursor(t *testing.T) {
	fixture := LoadAcquiredCursorFixtures(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			source := testfixture.MaterializeByName(t, fixture.SourceFixture)
			applyAcquiredCursorSetup(t, source, fixture.Setup)
			filesystem := &ingest.OSFileSystem{}
			git := testutil.DefaultGitResolver()
			adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, git, salt.Salt{}, "latest", mountedCurrentEnvironment{"OPENCODE_DB": source.Path}, filesystem, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatal(err)
			}
			config := makePipelineConfig(filepath.Join(t.TempDir(), "managed"))
			config.Sources = map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Dir(source.Path))}, Enabled: true}}
			config.StalenessThreshold = 0
			sessions, err := adapter.Discover(t.Context(), config.Sources[ingest.HarnessOpenCode])
			if err != nil {
				t.Fatalf("discover cursor input: %+v %v", sessions, err)
			}
			var selected *ingest.DiscoveredSession
			for _, session := range sessions {
				if session.SessionID == fixture.SessionID {
					copy := session
					selected = &copy
				}
			}
			if selected == nil {
				t.Fatalf("cursor fixture session %s was not discovered", fixture.SessionID)
			}
			legacyMeta, legacyTranscript, err := adapter.MaterializeTranscript(t.Context(), *selected)
			if err != nil {
				t.Fatal(err)
			}
			materialized, err := adapter.MaterializeTranscriptWithCursor(t.Context(), *selected)
			if err != nil || materialized.EventSeq == nil || *materialized.EventSeq != 0 {
				t.Fatalf("explicit acquired zero missing: %+v %v", materialized, err)
			}
			if !reflect.DeepEqual(legacyMeta, materialized.Metadata) || !bytes.Equal(legacyTranscript, materialized.Transcript) {
				t.Fatal("cursor acquisition changed frozen materialization output")
			}
			database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			staleDiscovery := &discoveredCursorAdapter{OpenCodeAdapter: adapter, session: *selected}
			registry := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return staleDiscovery }}
			pipeline, err := ingest.NewPipeline(filesystem, git, registry, config, ingest.WithStore(database))
			if err != nil {
				t.Fatal(err)
			}
			first, err := pipeline.Run(t.Context())
			if err != nil || first.Summary.New != 1 || first.Summary.StoreError != nil {
				t.Fatalf("publish acquired input: %+v %v", first, err)
			}
			metadataPath := ingest.SessionMetadataPath(string(config.OutputDir), string(legacyMeta.HostSlug), string(fixture.SessionID), "")
			beforeMetadata, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeTranscript, err := os.ReadFile(filepath.Join(filepath.Dir(metadataPath), string(fixture.SessionID)+"--transcript.json"))
			if err != nil {
				t.Fatal(err)
			}
			if row.StoredCursor != 0 {
				if err := database.UpsertOpenCodeSeqCursor(t.Context(), fixture.SessionID, row.StoredCursor); err != nil {
					t.Fatal(err)
				}
			}
			beforeState, err := database.ReadIndexState(t.Context(), fixture.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			applyAcquiredCursorSetup(t, source, row.Change)
			beforeSource := testfixture.SnapshotSource(t, source)
			config.Force = true
			pipeline, err = ingest.NewPipeline(filesystem, git, registry, config, ingest.WithStore(database))
			if err != nil {
				t.Fatal(err)
			}
			second, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if second.Summary.Errors != 0 || second.Summary.StoreError != nil {
				t.Fatalf("native refresh failure became a blocking pipeline outcome: %+v", second)
			}
			cursors, err := database.BulkLookupOpenCodeSeqCursors(t.Context(), []ingest.SessionID{fixture.SessionID})
			cursor, recorded := cursors[fixture.SessionID]
			if err != nil || !recorded || cursor != row.WantCursor {
				t.Fatalf("stored cursor = %v, want %d: %v", cursors, row.WantCursor, err)
			}
			if row.WantError {
				// The fixture error belongs to native acquisition. The pipeline
				// preserves the last-good artifact and reports a nonfatal warning.
				unchanged := false
				for _, session := range second.Sessions {
					if session.SessionID == fixture.SessionID {
						unchanged = session.Status == ingest.DiffUnchanged && session.Error == nil
					}
				}
				if !unchanged || second.Summary.New != 0 || second.Summary.Updated != 0 || second.Summary.Indexed != 0 || second.Summary.Computed != 0 {
					t.Fatalf("unavailable native refresh claimed new completion: %+v", second)
				}
				warned := false
				for _, diagnostic := range second.Diagnostics {
					if diagnostic.ErrorType == "adapter_refresh_unavailable" && strings.Contains(diagnostic.Message, "changed after discovery") && diagnostic.Location != "" && diagnostic.Remediation != "" {
						warned = true
					}
				}
				if !warned {
					t.Fatalf("missing actionable nonfatal refresh warning: %+v", second.Diagnostics)
				}
				afterState, err := database.ReadIndexState(t.Context(), fixture.SessionID)
				if err != nil || !reflect.DeepEqual(beforeState, afterState) {
					t.Fatalf("failed refresh changed stored artifact/index completion: before=%+v after=%+v error=%v", beforeState, afterState, err)
				}
				afterMetadata, err := os.ReadFile(metadataPath)
				if err != nil {
					t.Fatal(err)
				}
				afterTranscript, err := os.ReadFile(filepath.Join(filepath.Dir(metadataPath), string(fixture.SessionID)+"--transcript.json"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(beforeMetadata, afterMetadata) || !bytes.Equal(beforeTranscript, afterTranscript) {
					t.Fatal("changed consumed attribution replaced last-good artifact")
				}
			}
			found := false
			for _, diagnostic := range second.Diagnostics {
				found = found || diagnostic.ErrorType == "native_cursor_unavailable"
			}
			if found != row.WantDiagnostic {
				t.Fatalf("optional cursor diagnostic = %t, want %t: %+v", found, row.WantDiagnostic, second.Diagnostics)
			}
			testfixture.AssertUnchanged(t, source, beforeSource)
		})
	}
}
