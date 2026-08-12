package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/missing_source_recovery.yaml
var missingSourceRecoveryFixtureYAML []byte

//go:embed testdata/missing_source_recovery.manifest.yaml
var missingSourceRecoveryManifestYAML []byte

type missingSourceExistingEntry struct {
	Index         int    `yaml:"index"`
	Role          string `yaml:"role"`
	Content       string `yaml:"content"`
	ObservedModel string `yaml:"observedModel"`
}

type missingSourceRecoveryCase struct {
	Name                 string                     `yaml:"name"`
	SessionID            string                     `yaml:"sessionID"`
	SourcePath           string                     `yaml:"sourcePath"`
	OutputTranscriptPath string                     `yaml:"outputTranscriptPath"`
	ExistingEntry        missingSourceExistingEntry `yaml:"existingEntry"`
}

type missingSourceRecoveryFixture struct {
	Cases []missingSourceRecoveryCase `yaml:"cases"`
}

func decodeMissingSourceRecoveryFixture(data []byte) (missingSourceRecoveryFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture missingSourceRecoveryFixture
	if err := decoder.Decode(&fixture); err != nil {
		return missingSourceRecoveryFixture{}, fmt.Errorf("decode missing-source recovery fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return missingSourceRecoveryFixture{}, fmt.Errorf("missing-source recovery fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || fixtureCase.SessionID == "" || fixtureCase.SourcePath == "" || fixtureCase.OutputTranscriptPath == "" || fixtureCase.ExistingEntry.Role == "" || fixtureCase.ExistingEntry.Content == "" || fixtureCase.ExistingEntry.ObservedModel == "" {
			return missingSourceRecoveryFixture{}, fmt.Errorf("missing-source recovery fixture case %q is incomplete", fixtureCase.Name)
		}
		if _, duplicate := names[fixtureCase.Name]; duplicate {
			return missingSourceRecoveryFixture{}, fmt.Errorf("missing-source recovery fixture repeats case name %q", fixtureCase.Name)
		}
		names[fixtureCase.Name] = struct{}{}
	}
	return fixture, nil
}

func loadMissingSourceRecoveryFixture(t *testing.T) missingSourceRecoveryFixture {
	t.Helper()
	fixture, err := decodeMissingSourceRecoveryFixture(missingSourceRecoveryFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeSemanticManifest(missingSourceRecoveryManifestYAML, "missing-source recovery")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "missing-source recovery"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestMissingSourceRecoveryFixtureGuards(t *testing.T) {
	loadMissingSourceRecoveryFixture(t)
}

func TestPipeline_AutoDetectMissingSourcesKeepsExistingEntriesStale(t *testing.T) {
	fixture := loadMissingSourceRecoveryFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			sessionID, err := ingest.NewSessionID(fixtureCase.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			extraBytes, _ := json.Marshal(map[string]string{"model_id": fixtureCase.ExistingEntry.ObservedModel})
			extra := string(extraBytes)
			content := fixtureCase.ExistingEntry.Content
			metricsStore := testutil.NewStubMetricsStore()
			metricsStore.StaleIndexSessions = []ingest.SessionID{sessionID}
			metricsStore.IndexedEntries[sessionID] = []schema.SessionEntry{{
				SessionID:      sessionID,
				EntryIndex:     fixtureCase.ExistingEntry.Index,
				Role:           schema.Role(fixtureCase.ExistingEntry.Role),
				EntryType:      schema.EntryTypeText,
				ContentPreview: &content,
				Extra:          &extra,
			}}
			metricsStore.IndexStates[sessionID] = ingest.CurrentIndexVersion - 1
			metricsStore.SourceInfoByID = map[ingest.SessionID]struct {
				SourcePath   string
				SourceFormat ingest.SourceFormat
				Harness      string
			}{sessionID: {SourcePath: fixtureCase.SourcePath, SourceFormat: ingest.SourceFormatJSONL, Harness: string(ingest.HarnessClaudeCode)}}
			metricsStore.LookupSessionLocationFunc = func(context.Context, ingest.SessionID) (string, string, error) {
				return "missing-host", "", nil
			}

			fs := testutil.NewMemFS()
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return &emptyAdapter{} },
			}, ingest.PipelineConfig{OutputDir: "/sync", Sources: map[ingest.Harness]ingest.SourceConfig{}},
				ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessClaudeCode: ingest.NewClaudeIndexer(testutil.NewMemFS())}),
				ingest.WithMetricsStore(metricsStore),
			)
			if err != nil {
				t.Fatalf("create pipeline: %v", err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatalf("run pipeline: %v", err)
			}
			if result.Summary.Indexed != 0 {
				t.Fatalf("indexed count = %d, want 0 while all source copies are missing", result.Summary.Indexed)
			}
			stored := metricsStore.IndexedEntries[sessionID]
			if len(stored) != 1 || stored[0].EntryIndex != fixtureCase.ExistingEntry.Index || stored[0].Extra == nil || *stored[0].Extra != extra {
				t.Fatalf("existing indexed evidence changed after missing-source recovery: %#v", stored)
			}
			if version := metricsStore.IndexStates[sessionID]; version != ingest.CurrentIndexVersion-1 {
				t.Fatalf("index state = %d, want stale retryable version %d", version, ingest.CurrentIndexVersion-1)
			}
		})
	}
}

type emptyAdapter struct{}

func (*emptyAdapter) Harness() ingest.Harness { return ingest.HarnessClaudeCode }

func (*emptyAdapter) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	return nil, nil
}

func (*emptyAdapter) ExtractMetadata(context.Context, ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	return nil, errors.New("not called")
}
