package ingest_test

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/codex_repeated_metadata.yaml
var codexRepeatedMetadataYAML []byte

//go:embed testdata/codex_repeated_metadata.manifest.yaml
var codexRepeatedMetadataManifest []byte

type codexRepeatedMetadataCase struct {
	Name           string `yaml:"name"`
	Source         string `yaml:"source"`
	Refuse         bool   `yaml:"refuse"`
	MalformedLater bool   `yaml:"malformedLater"`
}

func loadCodexRepeatedMetadataFixtures(t *testing.T) []codexRepeatedMetadataCase {
	t.Helper()
	var doc struct {
		Cases []codexRepeatedMetadataCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(codexRepeatedMetadataYAML, &doc); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(codexRepeatedMetadataManifest, "codex repeated metadata")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range doc.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "codex repeated metadata"); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func TestCodexRepeatedSessionMetadata(t *testing.T) {
	for _, c := range loadCodexRepeatedMetadataFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			data := []byte(c.Source)
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			session := ingest.DiscoveredSession{SessionID: "11111111-1111-4111-8111-111111111111", Harness: ingest.HarnessCodex, SourcePath: ingest.ResolvedPath(path)}
			fs := &ingest.OSFileSystem{}
			adapter := ingest.NewCodexAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
			native, nativeErr := adapter.ExtractMetadata(t.Context(), session)
			original := ingest.NewUnifiedMetadata()
			original.SessionID, original.ModelHarness = session.SessionID, session.Harness
			original.Source.Format = ingest.SourceFormatJSONL
			original.HostSlug = "example.com-team-child"
			retained, retainedErr := adapter.ExtractMetadataFromTranscript(t.Context(), data, &original)
			indexer := ingest.NewCodexIndexer(fs, ingest.WithCodexProvenanceCapture(ingest.CodexProvenanceIndexerConfig{
				Enabled: true, GenerationID: func(ingest.DiscoveredSession) string { return "gen-repeated-metadata" },
			}))
			result, candidateErr := indexer.IndexTranscriptResult(t.Context(), session)
			if c.Refuse {
				if nativeErr == nil || retainedErr == nil || candidateErr == nil || result != nil {
					t.Fatalf("mismatched first record accepted: native=%v retained=%v candidate=%v", nativeErr, retainedErr, candidateErr)
				}
				if !strings.Contains(candidateErr.Error(), "no candidate was emitted") {
					t.Fatalf("unexpected candidate refusal: %v", candidateErr)
				}
				return
			}
			if nativeErr != nil {
				t.Fatalf("metadata extraction: native=%v candidate=%v", nativeErr, candidateErr)
			}
			if c.MalformedLater {
				if retainedErr == nil || candidateErr == nil || result != nil {
					t.Fatal("retained extraction or candidate accepted a malformed later payload")
				}
				found := false
				for _, warning := range native.Diagnostics.Warnings {
					found = found || warning.ErrorType == "parse_error" && warning.Location == "line 2"
				}
				if !found {
					t.Fatal("native extraction lost the later payload decoding warning")
				}
				return
			} else if retainedErr != nil {
				t.Fatal(retainedErr)
			}
			if candidateErr != nil {
				t.Fatal(candidateErr)
			}
			candidate, ok := result.(indexformat.V2)
			if !ok {
				t.Fatalf("candidate result = %T, want V2", result)
			}
			if err := candidate.Validate(); err != nil {
				t.Fatal(err)
			}
			for name, metadata := range map[string]*ingest.UnifiedMetadata{"native": native, "candidate": &candidate.Generation.Metadata, "retained": retained} {
				if metadata == nil {
					continue
				}
				if metadata.SessionID != session.SessionID || metadata.Version != "0.100.0" || metadata.Timestamp.Start != time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC).UnixMilli() {
					t.Errorf("%s replaced child metadata: %+v", name, metadata)
				}
				if metadata.Model != "gpt-5" || metadata.Timestamp.End != time.Date(2026, 9, 1, 10, 0, 3, 0, time.UTC).UnixMilli() {
					t.Errorf("%s stopped decoding after session_meta", name)
				}
			}
			if candidate.Generation.Metadata.CWD != "/synthetic/child" || candidate.Generation.Metadata.Git.Branch == nil || *candidate.Generation.Metadata.Git.Branch != "child" {
				t.Fatal("candidate adopted the parent project context")
			}
			if !c.MalformedLater {
				legacy, err := ingest.NewCodexIndexer(fs).IndexTranscriptBytesResult(t.Context(), session, data)
				if err != nil || legacy.IndexVersion() != 1 {
					t.Fatalf("retained format-1 indexing: result=%v err=%v", legacy, err)
				}
			}
		})
	}
}
