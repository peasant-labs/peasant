package ingest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestUnchangedSourceNotReextractedAcrossRefreshFreeSchemaBump drives a real
// ordinary harvest over a real captured source. After the source has settled,
// the stored session and its publication capture are rewritten to the previous,
// refresh-free schema, exactly as a database written before the bump holds
// them. The unchanged source must still report unchanged and leave the managed
// pair alone; the same row must still re-extract a genuinely changed source.
func TestUnchangedSourceNotReextractedAcrossRefreshFreeSchemaBump(t *testing.T) {
	t.Parallel()
	older := int(ingest.CurrentSchemaVersion) - 1
	if !ingest.MetadataSchemaVersionIsCurrent(older) {
		t.Skipf("schema %d is not readable by this build; no refresh-free predecessor to exercise", older)
	}
	fixture := loadCapturedSourceFixtures(t)[0]
	sid, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
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
		// Ordinary new source data can precede the previous ingestion clock.
		stamp := time.Unix(1_700_000_000, 0)
		if err := os.Chtimes(source, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fs := &ingest.OSFileSystem{}
	cfg := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join(root, "source"))}}}, OutputDir: ingest.ResolvedPath(filepath.Join(root, "output")), Parallelism: 1}
	run := func() *ingest.PipelineResult {
		t.Helper()
		pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessClaudeCode: ingest.NewClaudeIndexer(fs)}))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	write(fixture.Initial + "\n")
	if first := run(); first.Summary.New != 1 {
		t.Fatalf("first ingest: %+v", first.Summary)
	}
	if repeat := run(); repeat.Summary.Unchanged != 1 || repeat.Summary.Updated != 0 {
		t.Fatalf("current-schema no-op: %+v", repeat.Summary)
	}
	// A database written before the refresh-free bump records the previous
	// version on the session and its capture. The source has not moved.
	storetest.SetStoredMetadataSchema(t, db, sid, older)
	bumped := run()
	if bumped.Summary.Unchanged != 1 || bumped.Summary.Updated != 0 {
		t.Fatalf("unchanged source re-extracted across a refresh-free schema bump: %+v", bumped.Summary)
	}
	// The same stored row must still re-extract a genuinely changed source.
	write(fixture.Initial + "\n" + fixture.Append + "\n")
	changed := run()
	if changed.Summary.Updated != 1 {
		t.Fatalf("changed source not re-extracted: %+v", changed.Summary)
	}
}
