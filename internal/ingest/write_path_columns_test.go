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
	"github.com/peasant-labs/peasant/internal/store/storetest"
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

// TestWritePathMirrorsInPagesOf256 pins the mirror paging contract: no store
// transaction records more than MirrorPageSize sessions. The store refuses an
// oversized page outright, and a harvest of more sessions than one page holds
// records every row across more than one bounded transaction, none over the
// cap. The exact transaction count depends on how the concurrent drain batches
// the work, so the fixture asserts the bound the design guarantees rather than
// a timing-dependent number.
func TestWritePathMirrorsInPagesOf256(t *testing.T) {
	ctx := context.Background()

	// The store refuses a page larger than MirrorPageSize outright; this is why
	// the write path never hands it more than one page at a time.
	oversize := newDurabilityStore(t, storetest.CopyGoldenDB(t))
	refusals := oversize.Store.MirrorArtifacts(ctx, make([]ingest.ArtifactMirrorRequest, ingest.MirrorPageSize+1))
	if len(refusals) != ingest.MirrorPageSize+1 {
		t.Fatalf("refusal count = %d, want %d", len(refusals), ingest.MirrorPageSize+1)
	}
	for _, result := range refusals {
		if result.Err == nil {
			t.Fatal("the store must refuse a page larger than MirrorPageSize")
		}
	}
	oversize.Shutdown()

	// A harvest of more sessions than one page holds records every row without
	// ever exceeding the page cap, and in more than one transaction.
	const sessionCount = 300
	mfs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	modTime := time.Now().Add(-2 * time.Hour)
	discovered := make([]ingest.DiscoveredSession, 0, sessionCount)
	metas := make(map[ingest.SessionID]*ingest.UnifiedMetadata, sessionCount)
	ids := make([]ingest.SessionID, 0, sessionCount)
	for i := 1; i <= sessionCount; i++ {
		idStr := fmt.Sprintf("%08d-0000-4000-8000-000000000000", i)
		sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, idStr)
		setupSourceFile(t, mfs, sourcePath)
		mfs.ModTimes[sourcePath] = modTime
		session := makeDiscoveredSession(t, idStr, sourcePath, modTime)
		discovered = append(discovered, session)
		metas[session.SessionID] = makeMinimalMeta(t, idStr)
		ids = append(ids, session.SessionID)
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(discovered, metas),
	}
	indexer := &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile}

	ds := newDurabilityStore(t, storetest.CopyGoldenDB(t))
	pipeline, err := ingest.NewPipeline(mfs, git, adapters, makePipelineConfig(testOutputDir),
		ingest.WithStore(ds), ingest.WithMetricsStore(ds),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessClaudeCode: indexer}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(ctx); err != nil {
		t.Fatalf("harvest of %d sessions: %v", sessionCount, err)
	}
	if got := ds.MaxPage(); got > ingest.MirrorPageSize {
		t.Errorf("a mirror transaction carried %d sessions, over the %d cap", got, ingest.MirrorPageSize)
	}
	if got := ds.MirrorCalls(); got < 2 {
		t.Errorf("mirror transactions = %d; %d sessions cannot fit one page and must span more than one", got, sessionCount)
	}
	for _, id := range ids {
		state, err := ds.ReadIndexState(ctx, id)
		if err != nil || state == nil || state.ArtifactHash == nil {
			t.Fatalf("every session must be recorded; %s is missing (%+v, %v)", id, state, err)
		}
	}
}

// TestWritePathInstallsMetadataLast proves the install order: the transcript is
// renamed into place before the metadata, so a failure on the metadata rename
// leaves the new transcript beside the OLD metadata, which still names the old
// transcript hash. The pair is then refused on read by its hash. Were the
// metadata installed first, the same fault would leave both files old and no
// mixed pair, so this fixture fails if the order regresses.
func TestWritePathInstallsMetadataLast(t *testing.T) {
	ctx := context.Background()
	mfs := testutil.NewCountingFS(testutil.NewMemFS())
	git := testutil.DefaultGitResolver()
	sid, _ := ingest.NewSessionID(testSessionID)
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	sessionDir := ingest.SessionDir(testOutputDir, testutil.TestHostSlug, testSessionID, "")
	metadataPath := ingest.SessionMetadataPath(testOutputDir, testutil.TestHostSlug, testSessionID, "")
	transcriptPath := filepath.Join(sessionDir, testSessionID+"--transcript.jsonl")

	// First harvest: install the old pair from an old source.
	oldContent := []byte(`{"sessionId":"test","type":"user","message":{"role":"user","content":"one"},"timestamp":"2024-02-19T00:00:00Z"}` + "\n")
	if err := mfs.WriteFile(sourcePath, oldContent, 0600); err != nil {
		t.Fatal(err)
	}
	oldMod := time.Now().Add(-3 * time.Hour)
	mfs.ModTimes[sourcePath] = oldMod
	session := makeDiscoveredSession(t, testSessionID, sourcePath, oldMod)
	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta}),
	}
	first, err := ingest.NewPipeline(mfs, git, adapters, makePipelineConfig(testOutputDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(ctx); err != nil {
		t.Fatalf("first harvest: %v", err)
	}
	oldMetadata, err := mfs.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("old metadata must exist after the first harvest: %v", err)
	}
	oldTranscript, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}

	// Second harvest: the source changed. Plant a rename failure on the metadata
	// file only. The transcript is installed first, so it lands; the metadata
	// rename fails and the old metadata stays.
	newContent := []byte(`{"sessionId":"test","type":"user","message":{"role":"user","content":"two, changed"},"timestamp":"2024-02-19T01:00:00Z"}` + "\n")
	if err := mfs.WriteFile(sourcePath, newContent, 0600); err != nil {
		t.Fatal(err)
	}
	newMod := time.Now().Add(-time.Hour)
	mfs.ModTimes[sourcePath] = newMod
	session.ModTime = newMod
	adapters = map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{sid: makeMinimalMeta(t, testSessionID)}),
	}
	mfs.Fail(testutil.FSOpRename, metadataPath, fmt.Errorf("simulated crash before the metadata rename"))
	second, err := ingest.NewPipeline(mfs, git, adapters, makePipelineConfig(testOutputDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Run(ctx); err != nil {
		t.Fatalf("second harvest: %v", err)
	}

	// The transcript is the new content; the metadata is still the old file.
	gotTranscript, err := mfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotTranscript) == string(oldTranscript) {
		t.Fatal("the transcript should have been installed (renamed first) before the metadata rename failed")
	}
	gotMetadata, err := mfs.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMetadata) != string(oldMetadata) {
		t.Fatal("the old metadata must remain after its rename failed; metadata is installed last")
	}
	// The mixed pair is refused on read by its hash: the old metadata does not
	// name the new transcript.
	if _, err := ingest.ReadManagedPair(mfs, testOutputDir, metadataPath, sid); err == nil {
		t.Fatal("a transcript-new/metadata-old pair must be refused on read")
	}
}
