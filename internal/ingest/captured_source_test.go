package ingest_test

import (
	"bytes"
	_ "embed"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
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
	Name              string   `yaml:"name"`
	SessionID         string   `yaml:"session_id"`
	Initial           string   `yaml:"initial"`
	Append            string   `yaml:"append"`
	Malformed         string   `yaml:"malformed"`
	BoundedSessionIDs []string `yaml:"bounded_session_ids"`
	Entries           []struct {
		Role   ingest.Role `yaml:"role"`
		Depth  int         `yaml:"depth"`
		Parent *int        `yaml:"parent"`
		Text   string      `yaml:"text"`
	} `yaml:"entries"`
}

func TestCapturedFileOrdinaryLifecycle(t *testing.T) {
	fixtures := loadCapturedSourceFixtures(t)
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			testCapturedFileOrdinaryLifecycle(t, fixture)
		})
	}
}

func loadCapturedSourceFixtures(t *testing.T) []capturedSourceCase {
	t.Helper()
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
	return fixtures.Cases
}

func testCapturedFileOrdinaryLifecycle(t *testing.T, fixture capturedSourceCase) {
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
	if err != nil || len(entries) != len(fixture.Entries) {
		t.Fatalf("stored entries=%d error=%v", len(entries), err)
	}
	for i, want := range fixture.Entries {
		got := entries[i]
		if got.EntryIndex != i || got.Role != want.Role || got.Depth != want.Depth || !reflect.DeepEqual(got.ParentIndex, want.Parent) || got.ContentPreview == nil || *got.ContentPreview != want.Text {
			t.Fatalf("stored entry %d lost its captured structure or text: %+v", i, got)
		}
	}
	stableEntries := entries
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
	if err != nil || !reflect.DeepEqual(entries, stableEntries) {
		t.Fatalf("failed capture changed entries: %v", err)
	}
}

func TestStoreFreeCapturedFileLifecycle(t *testing.T) {
	for _, fixture := range loadCapturedSourceFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			sourceRoot := filepath.Join(root, "source")
			source := filepath.Join(sourceRoot, "-workspace", fixture.SessionID+".jsonl")
			if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
				t.Fatal(err)
			}
			write := func(content string) {
				t.Helper()
				if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				stamp := time.Unix(1_700_000_000, 0)
				if err := os.Chtimes(source, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			git := testutil.DefaultGitResolver()
			cfg := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(sourceRoot)}}}, OutputDir: ingest.ResolvedPath(filepath.Join(root, "output")), Parallelism: 1, StalenessThreshold: 100 * 365 * 24 * time.Hour}
			run := func() *ingest.PipelineResult {
				t.Helper()
				pipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, git, ingest.DefaultAdapterRegistry, cfg)
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, session := range result.Sessions {
					if session.Error != nil {
						t.Fatalf("harvest logs failed: %v", session.Error)
					}
				}
				return result
			}
			write(fixture.Initial + "\n")
			first := run()
			if len(first.Sessions) != 1 || first.Sessions[0].OutputPath == "" {
				t.Fatalf("first logs: %+v", first)
			}
			oldDir := first.Sessions[0].OutputPath
			child := filepath.Join(oldDir, "subagents", "retained", "child.txt")
			if err := os.MkdirAll(filepath.Dir(child), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(child, []byte("retained"), 0o600); err != nil {
				t.Fatal(err)
			}
			if repeat := run(); repeat.Summary.Unchanged != 1 {
				t.Fatalf("active no-op: %+v", repeat)
			}
			write(fixture.Initial + "\n" + fixture.Append + "\n")
			if update := run(); update.Summary.Updated != 1 {
				t.Fatalf("old-mtime update: %+v", update)
			}
			if repeat := run(); repeat.Summary.Unchanged != 1 {
				t.Fatalf("update no-op: %+v", repeat)
			}
			git.Remote = "https://github.com/example/corrected-project.git"
			repair := run()
			if repair.Summary.Updated != 1 || repair.Sessions[0].OutputPath == oldDir {
				t.Fatalf("upstream repair: %+v", repair)
			}
			newDir := repair.Sessions[0].OutputPath
			transcript, err := os.ReadFile(filepath.Join(newDir, fixture.SessionID+"--transcript.jsonl"))
			if err != nil || string(transcript) != fixture.Initial+"\n"+fixture.Append+"\n" {
				t.Fatalf("repaired transcript: %v", err)
			}
			metaPath := filepath.Join(newDir, fixture.SessionID+"--metadata.json")
			before, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatal(err)
			}
			if repeat := run(); repeat.Summary.Unchanged != 1 {
				t.Fatalf("repair no-op: %+v", repeat)
			}
			after, err := os.ReadFile(metaPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("no-op rewrote metadata: %v", err)
			}
			if data, err := os.ReadFile(child); err != nil || string(data) != "retained" {
				t.Fatalf("child lost: %v", err)
			}
			if _, err := os.Stat(filepath.Join(oldDir, fixture.SessionID+"--metadata.json")); !os.IsNotExist(err) {
				t.Fatalf("duplicate old metadata: %v", err)
			}
		})
	}
}

// At one worker, the next accepted source must not be acquired before the
// previous transcript is written. Discovery's ordinary reads are not captures.
type boundedCaptureFS struct {
	ingest.OSFileSystem
	captures atomic.Int64
	writes   atomic.Int64
	eager    atomic.Bool
}

var _ ingest.FileSystem = (*boundedCaptureFS)(nil)

func (f *boundedCaptureFS) ReadSourcePrefix(path string) ([]byte, error) {
	if f.captures.Add(1) > f.writes.Load()+1 {
		f.eager.Store(true)
	}
	return f.OSFileSystem.ReadSourcePrefix(path)
}

func (f *boundedCaptureFS) WriteFile(path string, data []byte, mode fs.FileMode) error {
	err := f.OSFileSystem.WriteFile(path, data, mode)
	if err == nil && strings.HasSuffix(path, "--transcript.jsonl") {
		f.writes.Add(1)
	}
	return err
}

func TestSourceAcquisitionUsesBoundedWorkers(t *testing.T) {
	for _, fixture := range loadCapturedSourceFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			sourceRoot := filepath.Join(root, "source")
			if err := os.MkdirAll(filepath.Join(sourceRoot, "-workspace"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, sid := range fixture.BoundedSessionIDs {
				path := filepath.Join(sourceRoot, "-workspace", sid+".jsonl")
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(fixture.Initial, fixture.SessionID, sid)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			filesystem := &boundedCaptureFS{}
			cfg := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(sourceRoot)}}}, OutputDir: ingest.ResolvedPath(filepath.Join(root, "output")), Parallelism: 1}
			pipeline, err := ingest.NewPipeline(filesystem, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.New != len(fixture.BoundedSessionIDs) || filesystem.eager.Load() || filesystem.captures.Load() != int64(len(fixture.BoundedSessionIDs)) {
				t.Fatalf("unbounded or repeated acquisition: captures=%d eager=%v result=%+v", filesystem.captures.Load(), filesystem.eager.Load(), result)
			}
		})
	}
}
