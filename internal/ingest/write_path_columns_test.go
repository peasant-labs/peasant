package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/write_path_columns.yaml
var writePathColumnsFixtureData []byte

// seedColumnsPair writes a transcript and metadata pair into the session dir so
// a later run could read them if it chose to. It records no database row.
func seedColumnsPair(t *testing.T, fs ingest.FileSystem, sessionDir, sid string, meta *ingest.UnifiedMetadata) {
	t.Helper()
	if err := fs.WriteFile(filepath.Join(sessionDir, sid+"--transcript.jsonl"), []byte(rebuildTranscript), 0600); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(sessionDir, sid+"--metadata.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

// TestWritePathColumns pins the filesystem operations a store-backed harvest
// performs, so a change that adds a sync, a lock, or a read of the installed
// pair moves a counted column. The expected columns live in the fixture; "pair"
// is the session's own transcript and metadata paths under peasant-sync.
func TestWritePathColumns(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name                  string `yaml:"name"`
			Scenario              string `yaml:"scenario"`
			WriteFileTotal        int    `yaml:"write_file_total"`
			RenameUnderSessionDir int    `yaml:"rename_under_session_dir"`
			WalkDirTotal          int    `yaml:"walk_dir_total"`
			RemoveAllTotal        int    `yaml:"remove_all_total"`
			MkdirAllMin           int    `yaml:"mkdir_all_min"`
			PairReadFile          int    `yaml:"pair_read_file"`
			PairStat              int    `yaml:"pair_stat"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(writePathColumnsFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("empty or duplicate write-path column fixture %q", c.Name)
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}

	adapterTarget := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].AdapterVersion

	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			mfs := testutil.NewCountingFS(testutil.NewMemFS())
			git := testutil.DefaultGitResolver()
			sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
			modTime := time.Now().Add(-2 * time.Hour)
			setupSourceFile(t, mfs.MemFS, sourcePath)
			mfs.ModTimes[sourcePath] = modTime
			session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
			meta := makeMinimalMeta(t, testSessionID)
			adapters := map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}),
			}
			cfg := makePipelineConfig(testOutputDir)
			sessionDir := ingest.SessionDir(testOutputDir, string(meta.HostSlug), testSessionID, "")
			metadataPath := ingest.SessionMetadataPath(testOutputDir, string(meta.HostSlug), testSessionID, "")
			transcriptPath := filepath.Join(sessionDir, testSessionID+"--transcript.jsonl")

			switch c.Scenario {
			case "new":
				db, err := store.Open(filepath.Join(t.TempDir(), "columns.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
					ingest.WithStore(db), ingest.WithMetricsStore(db),
					ingest.WithIndexers(ingest.NewIndexerRegistry(mfs, ingest.IndexerRegistryOptions{})))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pipeline.Run(context.Background()); err != nil {
					t.Fatal(err)
				}
				if got := mfs.Total(testutil.FSOpWriteFile); got != c.WriteFileTotal {
					t.Errorf("WriteFile total = %d, want %d", got, c.WriteFileTotal)
				}
				if got := mfs.CountUnder(testutil.FSOpRename, sessionDir); got != c.RenameUnderSessionDir {
					t.Errorf("Rename under session dir = %d, want %d", got, c.RenameUnderSessionDir)
				}
				if got := mfs.Total(testutil.FSOpWalkDir); got != c.WalkDirTotal {
					t.Errorf("WalkDir total = %d, want %d", got, c.WalkDirTotal)
				}
				if got := mfs.Total(testutil.FSOpRemoveAll); got != c.RemoveAllTotal {
					t.Errorf("RemoveAll total = %d, want %d", got, c.RemoveAllTotal)
				}
				if got := mfs.Total(testutil.FSOpMkdirAll); got < c.MkdirAllMin {
					t.Errorf("MkdirAll total = %d, want >= %d", got, c.MkdirAllMin)
				}
			case "unchanged":
				seedColumnsPair(t, mfs, sessionDir, testSessionID, meta)
				ingestedMs := time.Now().UnixMilli() // after the source modTime
				stub := &testutil.StubSessionStore{
					LocationsByID: map[ingest.SessionID]ingest.SessionLocation{
						session.SessionID: {
							HostSlug:       testutil.TestHostSlug,
							IngestedMs:     &ingestedMs,
							SchemaVersion:  ingest.CurrentSchemaVersion,
							AdapterVersion: &adapterTarget,
							// An evidence-holding row with no recorded fingerprint:
							// the clock hint stands and the session is left alone,
							// with no native read and no pair read.
							SourceEvidenceSupported: true,
						},
					},
				}
				mfs.ResetCounts()
				pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(stub))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if result.Summary.Unchanged != 1 {
					t.Fatalf("second harvest did not classify the session unchanged: %+v", result.Summary)
				}
				if got := mfs.CountUnder(testutil.FSOpWalkDir, testOutputDir); got != 0 {
					t.Errorf("WalkDir under output = %d, want 0", got)
				}
				if got := mfs.Count(testutil.FSOpStat, sessionDir); got != 0 {
					t.Errorf("session dir Stat = %d, want 0", got)
				}
				if got := mfs.BytesReadUnder(testOutputDir, "transcript.jsonl"); got != 0 {
					t.Errorf("transcript bytes read = %d, want 0", got)
				}
			case "future_schema":
				seedColumnsPair(t, mfs, sessionDir, testSessionID, meta)
				ingestedMs := time.Now().UnixMilli()
				future := ingest.CurrentSchemaVersion + 1
				stub := &testutil.StubSessionStore{
					LocationsByID: map[ingest.SessionID]ingest.SessionLocation{
						session.SessionID: {
							HostSlug:      testutil.TestHostSlug,
							IngestedMs:    &ingestedMs,
							SchemaVersion: future,
						},
					},
				}
				mfs.ResetCounts()
				pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithStore(stub))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				// The future-schema row is refused from the database version
				// check; the session is not re-ingested and the metadata file is
				// never read.
				if result.Summary.New != 0 || result.Summary.Updated != 0 {
					t.Errorf("a future-schema row was ingested: %+v", result.Summary)
				}
			}

			if got := mfs.Count(testutil.FSOpReadFile, metadataPath) + mfs.Count(testutil.FSOpReadFile, transcriptPath); got != c.PairReadFile {
				t.Errorf("pair ReadFile = %d, want %d", got, c.PairReadFile)
			}
			if c.Scenario != "future_schema" {
				if got := mfs.Count(testutil.FSOpStat, metadataPath) + mfs.Count(testutil.FSOpStat, transcriptPath); got != c.PairStat {
					t.Errorf("pair Stat = %d, want %d", got, c.PairStat)
				}
			}
		})
	}
}
