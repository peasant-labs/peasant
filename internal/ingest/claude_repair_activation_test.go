package ingest_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestRepairedLegacyClaudeArtifactActivatesFormatTwo drives the production
// harvest over a stored Claude session whose sidecar predates this build and
// whose native source is still present. The repair has to land as a format-2
// managed generation, and the run has to report no failed session.
//
// The candidate fixtures prove what the builder emits from bytes. This proves
// the harvest path actually activates it, which is what a repaired artifact
// depends on.
func TestRepairedLegacyClaudeArtifactActivatesFormatTwo(t *testing.T) {
	ctx := t.Context()
	fs := testutil.NewMemFS()
	root := filepath.Join(t.TempDir(), "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	database := storetest.OpenWith(t, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))

	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	transcript := []byte(missingPairNativeTranscript)
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	if err := fs.WriteFile(nativePath, transcript, 0600); err != nil {
		t.Fatal(err)
	}

	// A schema this build re-extracts from native input, with the native source
	// still there: the repair the issue is about.
	meta := makeMinimalMeta(t, id.String())
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Source.Format = ingest.SourceFormatJSONL
	meta.Source.FilePath = nativePath
	meta.ContentHash = schema.ComputeTranscriptHash(transcript)
	meta.SchemaVersion = 8
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
	if err := fs.WriteFile(filepath.Join(sessionDir, id.String()+"--transcript.jsonl"), transcript, 0600); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(sessionDir, id.String()+"--metadata.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}

	fresh := makeMinimalMeta(t, id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.FilePath = nativePath
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.ContentHash = schema.ComputeTranscriptHash(transcript)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	cfg := makePipelineConfig(testOutputDir)
	cfg.Force = true
	pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database), ingest.WithLogger(database),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithHarvesterVersions(ingest.GenerationTargets(ingest.HarvesterVersionRegistry)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if result.Summary.Errors != 0 {
		t.Fatalf("a repairable session failed the harvest: %+v, diagnostics=%+v", result.Summary, result.Diagnostics)
	}
	if result.Summary.Indexed != 1 {
		t.Fatalf("the repaired session was not indexed: %+v", result.Summary)
	}

	conn, err := database.Pool().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	assertStoredInt(t, conn, "SELECT COALESCE(index_format_version, 0) FROM sessions WHERE session_id = ?", string(id), 2,
		"the repaired session did not activate a format-2 generation")
	assertStoredInt(t, conn, "SELECT sessions_error FROM ingest_log ORDER BY id DESC LIMIT 1", "", 0,
		"the repair was counted as a session error")
}

func assertStoredInt(t *testing.T, conn *sqlite.Conn, query, arg string, want int, message string) {
	t.Helper()
	got := -1
	opts := &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		got = stmt.ColumnInt(0)
		return nil
	}}
	if arg != "" {
		opts.Args = []any{arg}
	}
	if err := sqlitex.ExecuteTransient(conn, query, opts); err != nil {
		t.Fatalf("%s: %v", message, err)
	}
	if got != want {
		t.Fatalf("%s: got %d, want %d", message, got, want)
	}
}
