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
// whose native source is still present. The repair has to re-ingest the
// session, rewrite the sidecar at the current schema, and land as a format-2
// managed generation with no failed session and no second session row.
//
// The candidate fixtures prove what the builder emits from bytes. This proves
// the harvest path repairs and activates it, which is what a legacy artifact
// depends on.
func TestRepairedLegacyClaudeArtifactActivatesFormatTwo(t *testing.T) {
	ctx := t.Context()
	fs := testutil.NewCountingFS(testutil.NewMemFS())
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

	fresh := makeMinimalMeta(t, id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.FilePath = nativePath
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.ContentHash = schema.ComputeTranscriptHash(transcript)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{{
			SessionID: id, Harness: ingest.HarnessClaudeCode,
			SourcePath: ingest.ResolvedPath(nativePath), SourceFormat: ingest.SourceFormatJSONL,
		}}, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	run := func() *ingest.PipelineResult {
		t.Helper()
		pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, makePipelineConfig(testOutputDir),
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
		return result
	}

	// Run 1 settles the session at the current schema with its native source.
	first := run()
	if first.Summary.Errors != 0 || first.Summary.Indexed != 1 {
		t.Fatalf("initial native ingest: %+v, diagnostics=%+v", first.Summary, first.Diagnostics)
	}
	sessionDir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
	sidecarPath := filepath.Join(sessionDir, id.String()+"--metadata.json")
	if _, err := fs.ReadFile(sidecarPath); err != nil {
		t.Fatalf("the initial ingest wrote no sidecar: %v", err)
	}

	// Downgrade the stored sidecar to the legacy revision this build
	// re-extracts. The repair has to notice it and rebuild the pair.
	// The stored row carries the revision too, so a legacy store is a row and a
	// sidecar at the old schema, not a rewritten file beside a current row.
	rewriteLegacySidecar(t, fs, sidecarPath, 8)
	downgradeStoredSchema(t, database, id, 8)
	fs.ResetCounts()

	second := run()
	if second.Summary.Errors != 0 {
		t.Fatalf("the repair failed: %+v, diagnostics=%+v", second.Summary, second.Diagnostics)
	}
	if second.Summary.Updated != 1 || second.Summary.Indexed != 1 {
		t.Fatalf("a legacy sidecar was not repaired and indexed: %+v", second.Summary)
	}
	if reads := fs.ReadCount(nativePath); reads == 0 {
		t.Fatal("the repair never read the native source it was supposed to re-ingest")
	}

	// The repair rewrites the sidecar at the current schema; a run that indexed
	// from the capture but left the legacy sidecar behind would re-repair on
	// every harvest.
	repaired, err := fs.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("the repaired sidecar was not written: %v", err)
	}
	var sidecar struct {
		SchemaVersion int    `json:"schemaVersion"`
		ContentHash   string `json:"contentHash"`
	}
	if err := json.Unmarshal(repaired, &sidecar); err != nil {
		t.Fatalf("decode repaired sidecar: %v", err)
	}
	if sidecar.SchemaVersion != int(ingest.CurrentSchemaVersion) {
		t.Fatalf("repaired sidecar schemaVersion = %d, want %d", sidecar.SchemaVersion, ingest.CurrentSchemaVersion)
	}
	if sidecar.ContentHash != schema.ComputeTranscriptHash(transcript) {
		t.Fatalf("repaired sidecar contentHash = %q, want the native bytes' hash", sidecar.ContentHash)
	}

	// A repaired session settles: the next harvest owes it nothing.
	third := run()
	if third.Summary.Indexed != 0 || third.Summary.Errors != 0 {
		t.Fatalf("a repaired session was not settled: %+v, diagnostics=%+v", third.Summary, third.Diagnostics)
	}

	conn, err := database.Pool().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	assertStoredInt(t, conn, "SELECT COALESCE(index_format_version, 0) FROM sessions WHERE session_id = ?", 2,
		"the repaired session did not activate a format-2 generation", string(id))
	assertStoredInt(t, conn, "SELECT COUNT(*) FROM sessions WHERE session_id = ?", 1,
		"the repair created a second session row", string(id))
	assertStoredInt(t, conn, "SELECT sessions_error FROM ingest_log ORDER BY id DESC LIMIT 1", 0,
		"the repair was counted as a session error")
}

// rewriteLegacySidecar sets the stored sidecar's schema revision to the given
// legacy value, leaving the rest of the recorded metadata intact.
func rewriteLegacySidecar(t *testing.T, fs *testutil.CountingFS, path string, version int) {
	t.Helper()
	data, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar %s: %v", path, err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode sidecar %s: %v", path, err)
	}
	document["schemaVersion"] = version
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode sidecar %s: %v", path, err)
	}
	if err := fs.WriteFile(path, encoded, 0600); err != nil {
		t.Fatalf("write sidecar %s: %v", path, err)
	}
}

// downgradeStoredSchema sets the row's recorded metadata revision, which is what
// the harvest's refresh selection reads.
func downgradeStoredSchema(t *testing.T, database *store.Store, id ingest.SessionID, version int) {
	t.Helper()
	ctx := t.Context()
	conn, err := database.Pool().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET schema_version = ? WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{version, string(id)},
	}); err != nil {
		t.Fatalf("downgrade stored schema: %v", err)
	}
}

func assertStoredInt(t *testing.T, conn *sqlite.Conn, query string, want int, message string, args ...any) {
	t.Helper()
	found := false
	got := 0
	opts := &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		found = true
		got = stmt.ColumnInt(0)
		return nil
	}}
	if err := sqlitex.ExecuteTransient(conn, query, opts); err != nil {
		t.Fatalf("%s: %v", message, err)
	}
	if !found {
		t.Fatalf("%s: the query returned no row", message)
	}
	if got != want {
		t.Fatalf("%s: got %d, want %d", message, got, want)
	}
}
