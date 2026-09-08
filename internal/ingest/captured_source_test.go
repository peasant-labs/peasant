package ingest_test

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/captured_source.yaml
var capturedSourceYAML []byte

type capturedSourceCase struct {
	Name      string `yaml:"name"`
	SessionID string `yaml:"session_id"`
	Initial   string `yaml:"initial"`
	Append    string `yaml:"append"`
	Malformed string `yaml:"malformed"`
}

func TestCapturedFileOrdinaryLifecycle(t *testing.T) {
	var fixtures struct {
		Cases []capturedSourceCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(capturedSourceYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("captured_source.yaml", "cases", []string{"ordinary active prefix lifecycle"}, names); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source", "-workspace", fixture.SessionID+".jsonl")
			if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
				t.Fatal(err)
			}
			write := func(content string) {
				t.Helper()
				if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				// Ordinary new source data can precede the previous ingestion audit clock.
				stamp := time.Unix(1_700_000_000, 0)
				if err := os.Chtimes(source, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			db, err := store.Open(filepath.Join(root, "peasant.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sid, err := ingest.NewSessionID(fixture.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			fs := &ingest.OSFileSystem{}
			cfg := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join(root, "source"))}}}, OutputDir: ingest.ResolvedPath(filepath.Join(root, "output")), Parallelism: 1}
			run := func() *ingest.PipelineResult {
				t.Helper()
				pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessClaudeCode: ingest.NewClaudeIndexer(fs)}))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			location := func() ingest.SessionLocation {
				t.Helper()
				locs, err := db.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sid})
				if err != nil {
					t.Fatal(err)
				}
				return locs[sid]
			}
			initial := fixture.Initial + "\n"
			write(initial + fixture.Append[:len(fixture.Append)/2])
			first := run()
			if first.Summary.New != 1 {
				t.Fatalf("first ingest: %+v", first)
			}
			before := location()
			if len(before.SourceFingerprint) == 0 {
				t.Fatal("missing consumed fingerprint")
			}
			if repeat := run(); repeat.Summary.Unchanged != 1 {
				t.Fatalf("initial no-op: %+v", repeat)
			}
			write(initial + fixture.Append + "\n")
			if updated := run(); updated.Summary.Updated != 1 {
				t.Fatalf("completed trailing record: %+v", updated)
			}
			after := location()
			if bytes.Equal(before.SourceFingerprint, after.SourceFingerprint) {
				t.Fatal("completed record not consumed")
			}
			entries, err := db.ListEntries(t.Context(), sid)
			if err != nil || len(entries) != 2 {
				t.Fatalf("stored entries=%d error=%v", len(entries), err)
			}
			if repeat := run(); repeat.Summary.Unchanged != 1 {
				t.Fatalf("updated no-op: %+v", repeat)
			}
			stable := location()
			if *stable.IngestedMs != *after.IngestedMs {
				t.Fatal("no-op rewrote ingest audit clock")
			}
			path := filepath.Join(string(cfg.OutputDir), stable.HostSlug, fixture.SessionID, fixture.SessionID+"--transcript.jsonl")
			stored, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			write(initial + fixture.Append + "\n" + fixture.Malformed + "\n")
			failed := run()
			if len(failed.Sessions) != 1 || failed.Sessions[0].Error == nil {
				t.Fatalf("malformed record accepted: %+v", failed)
			}
			preserved := location()
			if !bytes.Equal(stable.SourceFingerprint, preserved.SourceFingerprint) || *stable.IngestedMs != *preserved.IngestedMs {
				t.Fatal("failed capture advanced stored evidence")
			}
			retained, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(stored, retained) {
				t.Fatalf("failed capture replaced artifact: %v", err)
			}
			entries, err = db.ListEntries(t.Context(), sid)
			if err != nil || len(entries) != 2 {
				t.Fatalf("failed capture changed entries: %v", err)
			}
		})
	}
}
