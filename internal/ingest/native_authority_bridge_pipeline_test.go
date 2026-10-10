package ingest_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// storedStamps is the historical producer/format pair a stored session carries.
type storedStamps struct {
	producer int
	format   int
	adapter  int
}

// runOpenCodeAuthorityBridgePipeline drives the MOUNTED production native
// maintenance path against a real store that holds a V1 full content
// certificate with historical producer/format stamps and no active managed
// generation, over a native OpenCode source whose assistant row still carries a
// running/streaming tool. Ordinary maintenance must hold the last good full
// authority, install no V2 generation, name the unsettled message and sequence
// in the held diagnostic, and leave the entries, certificate, and stamps
// byte-identical. Once the tool settles, the same maintenance must activate a
// complete V2 generation.
func runOpenCodeAuthorityBridgePipeline(t *testing.T, tc nativeRefreshRepairCase) {
	t.Helper()
	settled := testfixture.MaterializeByName(t, tc.NativeSource)
	unsettled := testfixture.MaterializeByName(t, tc.UnsettledNativeSource)

	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	dbPath := storetest.CopyGoldenDB(t)
	db := nativeRepairStore(t, dbPath, root)
	defer func() { _ = db.Close() }()
	// The native OpenCode snapshot reads the real SQLite source, so the whole
	// mounted path runs on the OS filesystem with a real output directory.
	fs := &ingest.OSFileSystem{}
	outputDir := t.TempDir()

	sid, err := ingest.NewSessionID(tc.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	// Produce a real managed projection from the settled native source, so the
	// seeded session owns a genuine V1 retained pair the pipeline reconstructs
	// through the production OpenCode reader.
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	settledDir, err := ingest.NewResolvedPath(filepath.Dir(settled.Path))
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{settledDir}})
	if err != nil || len(discovered) != 1 {
		t.Fatalf("discover settled native source: sessions=%d error=%v", len(discovered), err)
	}
	materialized, err := adapter.MaterializeTranscript(t.Context(), discovered[0])
	if err != nil {
		t.Fatalf("materialize settled managed projection: %v", err)
	}

	meta := *materialized.Metadata
	meta.ModelHarness = ingest.HarnessOpenCode
	// The original native locator is the UNFINISHED source: the stored
	// projection is the retained baseline, and the native snapshot is what the
	// maintenance run re-reads.
	meta.Source = ingest.SourceInfo{FilePath: unsettled.Path, Format: ingest.SourceFormatJSON}
	if meta.Timestamp.Ingested == nil {
		ingested := time.Now().UnixMilli()
		meta.Timestamp.Ingested = &ingested
	}

	storetest.SeedManagedInput(t, db, fs, outputDir, meta, materialized.Data)
	// The seeded certificate is complete/full; record the current OpenCode
	// SQLite origin the native source really has, so the held diagnostic proves
	// the bridge preserves the origin and the status.
	execBridgeSQL(t, db, `UPDATE session_content_captures SET transcript_origin = ? WHERE session_id = ?`,
		int64(ingest.TranscriptOriginOpenCodeCurrentSQLite), string(sid))
	setStoredIndexFormat(t, db, sid, tc.StoredIndexerVersion, tc.StoredIndexFormat)
	setSessionAdapterVersion(t, db, sid, ingest.NativeGenerationRepairTargets[ingest.HarnessOpenCode].AdapterVersion)

	entriesBefore, err := db.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatalf("read seeded V1 entries: %v", err)
	}
	if len(entriesBefore) == 0 {
		t.Fatal("the seeded V1 retained pair produced no entries")
	}
	captureBefore := readAuthorityCapture(t, db, sid)
	stampsBefore := readStoredStamps(t, db, sid)
	indexBefore, err := db.ReadIndexState(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	exportBytes := func() []byte {
		t.Helper()
		detail, err := export.ExportSession(t.Context(), db, fs, string(sid))
		if err != nil {
			t.Fatalf("export last-good full capture: %v", err)
		}
		data, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	exportBefore := exportBytes()

	held := runNativeRepairPipeline(t, db, fs, outputDir)
	if got := indexedOutcomeFor(held, sid); got == ingest.IndexOutcomeIndexed || got == ingest.IndexOutcomeReindexed {
		t.Fatalf("held maintenance reported %q, want no success: %+v", got, held.IndexLog)
	}
	assertNoActiveGeneration(t, db, sid)
	assertEntriesUnchanged(t, db, sid, entriesBefore)
	if after := readAuthorityCapture(t, db, sid); after != captureBefore {
		t.Fatalf("held maintenance changed the full certificate: %+v -> %+v", captureBefore, after)
	}
	if after := readStoredStamps(t, db, sid); after != stampsBefore {
		t.Fatalf("held maintenance moved historical stamps: %+v -> %+v", stampsBefore, after)
	}
	indexAfter, err := db.ReadIndexState(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(indexBefore, indexAfter) {
		t.Fatalf("held maintenance changed stored index evidence: %+v -> %+v", indexBefore, indexAfter)
	}
	if !bytes.Equal(exportBefore, exportBytes()) {
		t.Fatal("held maintenance changed the last-good full export")
	}
	assertHeldDiagnostic(t, held, sid, tc.SessionID, tc.HeldDiagnosticContains)

	// The source settles: the running/streaming tools become terminal. Ordinary
	// maintenance now activates a complete V2 generation from the same stored
	// V1 authority.
	settledBytes, err := os.ReadFile(settled.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unsettled.Path, settledBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	activated := runNativeRepairPipeline(t, db, fs, outputDir)
	if got := indexedOutcomeFor(activated, sid); got != ingest.IndexOutcomeIndexed && got != ingest.IndexOutcomeReindexed {
		t.Fatalf("settled maintenance reported %q, want indexed: %+v", got, activated.IndexLog)
	}
	var generationID string
	var completeness indexformat.GenerationCompleteness
	if err := db.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
		generationID = snapshot.GenerationID
		completeness = snapshot.Completeness
		return nil
	}); err != nil {
		t.Fatalf("read activated snapshot: %v", err)
	}
	if generationID == "" {
		t.Fatal("settled maintenance activated no V2 generation")
	}
	if completeness != indexformat.GenerationCompletenessComplete {
		t.Fatalf("activated completeness = %q, want complete", completeness)
	}
	if after := readStoredStamps(t, db, sid); after.format != 2 {
		t.Fatalf("activated stored format = %d, want 2", after.format)
	}
}

func assertNoActiveGeneration(t *testing.T, db *store.Store, sid ingest.SessionID) {
	t.Helper()
	var generationID string
	if err := db.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
		generationID = snapshot.GenerationID
		return nil
	}); err != nil {
		t.Fatalf("read held snapshot: %v", err)
	}
	if generationID != "" {
		t.Fatalf("held maintenance installed generation %q, want none", generationID)
	}
}

func assertEntriesUnchanged(t *testing.T, db *store.Store, sid ingest.SessionID, before []schema.SessionEntry) {
	t.Helper()
	after, err := db.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatalf("read held entries: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("held maintenance changed the canonical entries:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func assertHeldDiagnostic(t *testing.T, result *ingest.PipelineResult, sid ingest.SessionID, rawSessionID string, wants []string) {
	t.Helper()
	var held []string
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Location == string(sid) {
			held = append(held, diagnostic.Message)
		}
	}
	if len(held) == 0 {
		t.Fatalf("held maintenance reported no diagnostic naming session %s: %+v", rawSessionID, result.Diagnostics)
	}
	joined := strings.Join(held, "\n")
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			t.Fatalf("held diagnostic does not contain %q: %s", want, joined)
		}
	}
}

// execBridgeSQL runs one parameterized statement on the store's pool.
func execBridgeSQL(t *testing.T, db *store.Store, query string, args ...any) {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("exec bridge SQL: %v", err)
	}
}

func readAuthorityCapture(t *testing.T, db *store.Store, sid ingest.SessionID) ingest.SessionContentCapture {
	t.Helper()
	capture, found, err := db.GetSessionContentCapture(t.Context(), sid)
	if err != nil {
		t.Fatalf("read stored capture: %v", err)
	}
	if !found {
		t.Fatal("stored session carries no content certificate")
	}
	return capture
}

func readStoredStamps(t *testing.T, db *store.Store, sid ingest.SessionID) storedStamps {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var stamps storedStamps
	if err := sqlitex.ExecuteTransient(conn, `SELECT index_version, index_format_version, adapter_version FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			stamps.producer = stmt.ColumnInt(0)
			stamps.format = stmt.ColumnInt(1)
			stamps.adapter = stmt.ColumnInt(2)
			return nil
		},
	}); err != nil {
		t.Fatalf("read stored stamps: %v", err)
	}
	return stamps
}
