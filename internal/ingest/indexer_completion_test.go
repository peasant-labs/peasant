package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/indexer_completion.yaml
var indexerCompletionData []byte

type indexerCompletionFixture struct {
	Name          string         `yaml:"name"`
	Harness       ingest.Harness `yaml:"harness"`
	Input         string         `yaml:"input"`
	Missing       bool           `yaml:"missing"`
	Error         bool           `yaml:"error"`
	Entries       int            `yaml:"entries"`
	LegacyEntries *int           `yaml:"legacyEntries"`
	LongContent   bool           `yaml:"longContent"`
	Oversized     bool           `yaml:"oversized"`
}

func loadIndexerCompletionFixtures(t *testing.T) []indexerCompletionFixture {
	t.Helper()
	var fixtures struct {
		RequiredNames []string                   `yaml:"requiredNames"`
		Cases         []indexerCompletionFixture `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(indexerCompletionData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("duplicate/absent fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required completion fixture %q missing", name)
		}
	}
	return fixtures.Cases
}

func TestConcreteIndexerCompletion(t *testing.T) {
	for _, fixture := range loadIndexerCompletionFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			session := ingest.DiscoveredSession{SessionID: schema.SessionID(testutil.TestSessionUUID), Harness: fixture.Harness, SourcePath: "/synthetic/transcript.jsonl"}
			data := []byte(fixture.Input)
			if fixture.LongContent {
				data = []byte(strings.ReplaceAll(fixture.Input, "LONG_CONTENT", strings.Repeat("x", defaults.ContentPreviewLimit+50)))
			}
			if fixture.Oversized {
				data = append(data, []byte(strings.Repeat("x", defaults.ScannerMaxLine)+"\n")...)
			}
			if !fixture.Missing {
				if err := fs.WriteFile(session.SourcePath.String(), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			indexer := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[fixture.Harness]
			strict, ok := indexer.(ingest.VersionedTranscriptIndexer)
			if !ok {
				t.Fatal("production indexer cannot verify completion")
			}
			result, err := strict.IndexTranscriptResult(context.Background(), session)
			if fixture.Error {
				if err == nil || result != nil {
					t.Fatalf("incomplete input certified: result=%#v error=%v", result, err)
				}
				if strings.Count(err.Error(), "could not verify complete input") != 1 {
					t.Fatalf("completion diagnostic is missing or repeated: %v", err)
				}
			} else {
				assertCompletionEntries(t, result, err, fixture.Entries)
			}
			if !fixture.Missing {
				fromBytes, bytesErr := strict.IndexTranscriptBytesResult(context.Background(), session, data)
				if (bytesErr != nil) != fixture.Error || !reflect.DeepEqual(result, fromBytes) {
					t.Fatalf("file/bytes outcomes differ: %#v %v / %#v %v", result, err, fromBytes, bytesErr)
				}
				after, readErr := fs.ReadFile(session.SourcePath.String())
				if readErr != nil || !bytes.Equal(data, after) {
					t.Fatalf("source changed: %v", readErr)
				}
			}
			if fixture.LegacyEntries != nil {
				legacy, legacyErr := indexer.IndexTranscript(context.Background(), session)
				if legacyErr != nil || len(legacy) != *fixture.LegacyEntries {
					t.Fatalf("legacy tolerance changed: entries=%d error=%v", len(legacy), legacyErr)
				}
			}
			if !fixture.Error {
				full := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: true})[fixture.Harness].(ingest.VersionedTranscriptIndexer)
				fullResult, fullErr := full.IndexTranscriptResult(context.Background(), session)
				assertCompletionEntries(t, fullResult, fullErr, fixture.Entries)
				boundedEntries := result.(indexformat.V1).Entries
				fullEntries := fullResult.(indexformat.V1).Entries
				if fixture.LongContent && (boundedEntries[0].ContentPreview == nil || fullEntries[0].ContentPreview == nil || len(*boundedEntries[0].ContentPreview) != defaults.ContentPreviewLimit || len(*fullEntries[0].ContentPreview) <= defaults.ContentPreviewLimit) {
					t.Fatal("fixture did not exercise full-content truncation difference")
				}
				for n := range boundedEntries {
					boundedEntries[n].ContentPreview, fullEntries[n].ContentPreview = nil, nil
					boundedEntries[n].ToolOutput, fullEntries[n].ToolOutput = nil, nil
				}
				if !reflect.DeepEqual(boundedEntries, fullEntries) {
					t.Fatal("bounded/full-content coordinates or semantics differ")
				}
			}
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			cancelledResult, cancelledErr := strict.IndexTranscriptBytesResult(cancelled, session, data)
			if cancelledErr == nil || cancelledResult != nil {
				t.Fatal("cancelled parsing certified completion")
			}
		})
	}
}

func assertCompletionEntries(t *testing.T, result indexformat.Result, err error, count int) {
	t.Helper()
	if err != nil {
		t.Fatalf("verified input refused: %v", err)
	}
	output, ok := result.(indexformat.V1)
	if !ok || len(output.Entries) != count {
		t.Fatalf("result=%#v, want V1 with %d entries", result, count)
	}
}
