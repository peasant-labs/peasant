package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

const (
	reclaimCmdSessionID = "cccc3333-3333-4333-8333-333333333333"
	reclaimCmdOldGen    = "g_reclaim_cmd_old"
	reclaimCmdActiveGen = "g_reclaim_cmd_active"
)

// executeReclaimCmd runs `peasant reclaim` under a test root with --data-dir,
// --config-dir and --config all scoped to dir. The config sets output.basePath
// to the owned-artifact root the test seeded, so the command reads and removes
// the same generation directories. Combined stdout+stderr is captured.
func executeReclaimCmd(t *testing.T, dir, ownedRoot string, args []string) (string, error) {
	t.Helper()
	configPath := filepath.Join(dir, "reclaim-config.yaml")
	body := "version: 1\noutput:\n  basePath: " + ownedRoot + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write reclaim config: %v", err)
	}
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.AddCommand(BuildReclaimCommand())

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{
		"reclaim",
		"--data-dir", dir,
		"--config-dir", dir,
		"--config", configPath,
	}, args...))
	err := root.Execute()
	return buf.String(), err
}

// reclaimCmdGeneration builds a minimal valid managed candidate with one text
// entry.
func reclaimCmdGeneration(t *testing.T, sid schema.SessionID, generationID string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	ref := schema.SourceEntryRef("e_" + generationID)
	text := "text for " + generationID
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           generationID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: 1, InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: []schema.SessionEntry{{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text, SourceEntryRef: ref}}},
		Content:              []indexformat.ContentRecord{{Ref: ref}},
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-0", Ref: ref}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{ref},
	}
	return indexformat.V2{Generation: generation}, map[schema.SourceEntryRef][]byte{ref: []byte(text)}
}

// fillReclaimCmdCandidate completes one candidate's content records from
// its staged bytes: the owned relative path, byte length, and integrity
// digest each blob file and catalog row carries.
func fillReclaimCmdCandidate(v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) indexformat.V2 {
	filled := append([]indexformat.ContentRecord(nil), v2.Generation.Content...)
	for i := range filled {
		payload, ok := blobs[filled[i].Ref]
		if !ok {
			continue
		}
		sum := sha256.Sum256(payload)
		digest := hex.EncodeToString(sum[:])
		if filled[i].RelativeBlob == "" {
			filled[i].RelativeBlob = "c_" + digest + ".blob"
		}
		if filled[i].ByteLength == 0 {
			filled[i].ByteLength = int64(len(payload))
		}
		if filled[i].Digest == "" {
			filled[i].Digest = digest
		}
	}
	v2.Generation.Content = filled
	return v2
}

// quoteReclaimLiteral escapes one SQL string literal for the file-backed
// seed: the candidate texts are fixed and quote-free, and the escaper keeps
// a future text change from breaking the seed.
func quoteReclaimLiteral(raw string) string {
	return "'" + strings.ReplaceAll(raw, "'", "''") + "'"
}

// seedFileBackedCmdGeneration installs one candidate into the file-backed
// storage Release N still reads: the projection generation row with its
// entry, content, alias, and section rows, plus the owned generation
// directory with its manifest and content blobs. The session points at the
// last generation seeded.
func seedFileBackedCmdGeneration(t *testing.T, conn *sqlite.Conn, ownedRoot string, sid schema.SessionID, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
	t.Helper()
	filled := fillReclaimCmdCandidate(v2, blobs)
	generation := filled.Generation
	execReclaimCmdSQL(t, conn, `INSERT INTO session_projection_generations(session_id, generation_id, metadata_json, title_refs_json, input_submission_count, source_evidence_digest, completeness, index_format_version, installed_at_ms, activated_at_ms) VALUES (`+
		quoteReclaimLiteral(string(sid))+`,`+quoteReclaimLiteral(generation.ID)+`,`+
		quoteReclaimLiteral(mustMarshalReclaimJSON(t, generation.Metadata))+`,`+
		quoteReclaimLiteral(mustMarshalReclaimJSON(t, generation.TitleRefs))+`,1,`+
		quoteReclaimLiteral(generation.SourceEvidenceDigest)+`,'complete',2,1,1);`)
	for _, entry := range generation.Main.Entries {
		execReclaimCmdSQL(t, conn, `INSERT INTO session_projection_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, entry_json) VALUES (`+
			quoteReclaimLiteral(string(sid))+`,`+quoteReclaimLiteral(generation.ID)+`,0,`+
			fmt.Sprintf("%d", entry.EntryIndex)+`,`+
			quoteReclaimLiteral(string(entry.SourceEntryRef))+`,`+
			quoteReclaimLiteral(mustMarshalReclaimJSON(t, entry))+`);`)
	}
	for _, record := range generation.Content {
		execReclaimCmdSQL(t, conn, `INSERT INTO session_projection_content(session_id, generation_id, source_entry_ref, relative_blob, byte_length, integrity_digest) VALUES (`+
			quoteReclaimLiteral(string(sid))+`,`+quoteReclaimLiteral(generation.ID)+`,`+
			quoteReclaimLiteral(string(record.Ref))+`,`+quoteReclaimLiteral(record.RelativeBlob)+`,`+
			fmt.Sprintf("%d", record.ByteLength)+`,`+
			quoteReclaimLiteral(record.Digest)+`);`)
	}
	for _, alias := range generation.Aliases {
		execReclaimCmdSQL(t, conn, `INSERT INTO session_projection_aliases(session_id, generation_id, native_key, source_entry_ref) VALUES (`+
			quoteReclaimLiteral(string(sid))+`,`+quoteReclaimLiteral(generation.ID)+`,`+
			quoteReclaimLiteral(alias.NativeKey)+`,`+quoteReclaimLiteral(string(alias.Ref))+`);`)
	}
	execReclaimCmdSQL(t, conn, `INSERT INTO session_projection_sections(session_id, generation_id, partition_id, earlier_state) VALUES (`+
		quoteReclaimLiteral(string(sid))+`,`+quoteReclaimLiteral(generation.ID)+`,0,NULL);`)
	genDir := filepath.Join(ownedRoot, string(sid), "generations", generation.ID)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), []byte(mustMarshalReclaimJSON(t, generation)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, record := range generation.Content {
		payload, ok := blobs[record.Ref]
		if !ok {
			t.Fatalf("no staged bytes for content record %q", record.Ref)
		}
		if err := os.WriteFile(filepath.Join(genDir, record.RelativeBlob), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	execReclaimCmdSQL(t, conn, `UPDATE sessions SET active_generation_id = `+quoteReclaimLiteral(generation.ID)+` WHERE session_id = `+quoteReclaimLiteral(string(sid))+`;`)
}

// mustMarshalReclaimJSON marshals one seed document for the file-backed
// catalog: the entries and metadata the pre-harmonized writer persisted.
func mustMarshalReclaimJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// execReclaimCmdSQL runs one seed script on the command test's direct
// database connection.
func execReclaimCmdSQL(t *testing.T, conn *sqlite.Conn, script string) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("exec seed SQL: %v\nscript: %.200s", err, script)
	}
}

// seedReclaimCmdStore seeds one session with a superseded and an active
// managed generation into the file-backed storage Release N still reads:
// the projection catalog rows plus the owned generation directories with
// their manifests and blobs. The sweep flag is set, exactly as the v62
// backfill does for sessions with a non-active projection row, so the
// flag-selected reclaim finds the session.
func seedReclaimCmdStore(t *testing.T, dir, ownedRoot string) {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	artifacts, err := store.NewOSGenerationArtifactStore(ownedRoot)
	if err != nil {
		t.Fatalf("open generation artifacts: %v", err)
	}
	locker, err := store.NewFileSessionLocker(ownedRoot)
	if err != nil {
		t.Fatalf("open session locker: %v", err)
	}
	db, err := openPreparedStore(t, dbPath, store.WithPoolSize(1), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	sid := schema.SessionID(reclaimCmdSessionID)
	storetest.SeedSession(t, db, reclaimCmdSessionID)
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	defer conn.Close()
	old, oldBlobs := reclaimCmdGeneration(t, sid, reclaimCmdOldGen)
	seedFileBackedCmdGeneration(t, conn, ownedRoot, sid, old, oldBlobs)
	active, activeBlobs := reclaimCmdGeneration(t, sid, reclaimCmdActiveGen)
	seedFileBackedCmdGeneration(t, conn, ownedRoot, sid, active, activeBlobs)
	execReclaimCmdSQL(t, conn, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = `+quoteReclaimLiteral(reclaimCmdSessionID)+`;`)
}

// seedReclaimCmdHarmonizedStore seeds one session with a superseded and an
// active harmonized generation through the production activation: no files,
// digest-addressed rows, and the sweep flag set by staging. The harmonized
// twins of the reclaim command tests run against it.
func seedReclaimCmdHarmonizedStore(t *testing.T, dir, ownedRoot string) {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	artifacts, err := store.NewOSGenerationArtifactStore(ownedRoot)
	if err != nil {
		t.Fatalf("open generation artifacts: %v", err)
	}
	locker, err := store.NewFileSessionLocker(ownedRoot)
	if err != nil {
		t.Fatalf("open session locker: %v", err)
	}
	db, err := openPreparedStore(t, dbPath, store.WithPoolSize(1), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	defer func() { _ = db.Close() }()

	sid := schema.SessionID(reclaimCmdSessionID)
	storetest.SeedSession(t, db, reclaimCmdSessionID)
	old, oldBlobs := reclaimCmdGeneration(t, sid, reclaimCmdOldGen)
	if _, err := db.ActivateGeneration(t.Context(), store.GenerationActivation{Generation: old, Blobs: oldBlobs, IndexerVersion: 1, IndexedAtMs: 1}); err != nil {
		t.Fatalf("activate superseded generation: %v", err)
	}
	active, activeBlobs := reclaimCmdGeneration(t, sid, reclaimCmdActiveGen)
	if _, err := db.ActivateGeneration(t.Context(), store.GenerationActivation{Generation: active, Blobs: activeBlobs, IndexerVersion: 1, IndexedAtMs: 1}); err != nil {
		t.Fatalf("activate active generation: %v", err)
	}
}

// TestReclaimCmdDryRunListsSupersededGenerations proves the dry-run surface
// reports the session, generation, rows and bytes and changes nothing.
func TestReclaimCmdDryRunListsSupersededGenerations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "managed")
	seedReclaimCmdStore(t, dir, ownedRoot)

	output, err := executeReclaimCmd(t, dir, ownedRoot, []string{"--dry-run"})
	if err != nil {
		t.Fatalf("reclaim --dry-run: %v\n%s", err, output)
	}
	for _, want := range []string{reclaimCmdSessionID, reclaimCmdActiveGen, "session_projection_entries", "dry run: no row and no directory was removed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(filepath.Join(ownedRoot, reclaimCmdSessionID, "generations", reclaimCmdOldGen)); err != nil {
		t.Fatalf("dry run removed a superseded generation directory: %v", err)
	}
}

// TestReclaimCmdApplyRemovesSupersededRowsAndDirectory proves the applied pass
// removes the superseded generation's rows and directory while the active
// generation stays readable, and prints the offline VACUUM reminder.
func TestReclaimCmdApplyRemovesSupersededRowsAndDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "managed")
	seedReclaimCmdStore(t, dir, ownedRoot)

	output, err := executeReclaimCmd(t, dir, ownedRoot, []string{"--confirm"})
	if err != nil {
		t.Fatalf("reclaim --confirm: %v\n%s", err, output)
	}
	if !strings.Contains(output, "reclaimed 1 session(s), 1 generation(s)") {
		t.Fatalf("apply output missing the reclaim summary:\n%s", output)
	}
	if !strings.Contains(output, "VACUUM") {
		t.Fatalf("apply output missing the offline VACUUM reminder:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(ownedRoot, reclaimCmdSessionID, "generations", reclaimCmdOldGen)); !os.IsNotExist(err) {
		t.Fatalf("superseded generation directory survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ownedRoot, reclaimCmdSessionID, "generations", reclaimCmdActiveGen)); err != nil {
		t.Fatalf("active generation directory was removed: %v", err)
	}

	// The active generation is still readable through the production snapshot.
	artifacts, err := store.NewOSGenerationArtifactStore(ownedRoot)
	if err != nil {
		t.Fatalf("open generation artifacts: %v", err)
	}
	locker, err := store.NewFileSessionLocker(ownedRoot)
	if err != nil {
		t.Fatalf("open session locker: %v", err)
	}
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)), store.WithPoolSize(1), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = db.Close() }()
	var visible string
	if err := db.WithSessionSnapshot(t.Context(), schema.SessionID(reclaimCmdSessionID), func(snapshot indexformat.ReadSnapshot) error {
		visible = snapshot.GenerationID
		return nil
	}); err != nil {
		t.Fatalf("read active snapshot: %v", err)
	}
	if visible != reclaimCmdActiveGen {
		t.Fatalf("visible generation = %q, want active %q", visible, reclaimCmdActiveGen)
	}
}

// TestReclaimCmdEmptyStoreReportsNothing proves an empty store is a no-op.
func TestReclaimCmdEmptyStoreReportsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "managed")
	// Prepare a golden database at the command's path with no generations.
	if _, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir))); err != nil {
		t.Fatalf("prepare empty store: %v", err)
	}
	output, err := executeReclaimCmd(t, dir, ownedRoot, []string{"--dry-run"})
	if err != nil {
		t.Fatalf("reclaim --dry-run on an empty store: %v\n%s", err, output)
	}
	if !strings.Contains(output, "no superseded managed generations to reclaim") {
		t.Fatalf("empty-store output = %q", output)
	}
}

// TestReclaimCmdDryRunJSON proves the machine-readable forecast is valid JSON
// with the session and totals the dry run reports.
func TestReclaimCmdDryRunJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "managed")
	seedReclaimCmdStore(t, dir, ownedRoot)

	output, err := executeReclaimCmd(t, dir, ownedRoot, []string{"--dry-run", "--json"})
	if err != nil {
		t.Fatalf("reclaim --dry-run --json: %v\n%s", err, output)
	}
	var decoded struct {
		DryRun   bool             `json:"dry_run"`
		Sessions []map[string]any `json:"sessions"`
		Totals   map[string]any   `json:"totals"`
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("decode dry-run JSON: %v\n%s", err, output)
	}
	if !decoded.DryRun || len(decoded.Sessions) != 1 || decoded.Totals["generations"] != float64(1) {
		t.Fatalf("unexpected dry-run JSON: %s", output)
	}
}

// TestReclaimCmdRefusesNonInteractiveConfirm proves the destructive path
// refuses to prompt on a non-terminal and deletes nothing.
func TestReclaimCmdRefusesNonInteractiveConfirm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "managed")
	seedReclaimCmdStore(t, dir, ownedRoot)

	output, err := executeReclaimCmd(t, dir, ownedRoot, nil)
	if err == nil {
		t.Fatalf("reclaim without --confirm on a non-terminal returned nil error:\n%s", output)
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("refusal error = %v, want a non-terminal refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(ownedRoot, reclaimCmdSessionID, "generations", reclaimCmdOldGen)); statErr != nil {
		t.Fatalf("refused reclaim removed a directory: %v", statErr)
	}
}

// reclaimCmdQueryInt runs one integer query against the command test's
// database file: the row-level assertions behind the CLI surface.
func reclaimCmdQueryInt(t *testing.T, dir, query string, args ...any) int64 {
	t.Helper()
	conn, err := sqlite.OpenConn(string(defaults.ResolveDBFilePathWith(dir)), sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open command database: %v", err)
	}
	defer conn.Close()
	var got int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return got
}

// TestReclaimCmdApplyRemovesHarmonizedSupersededRows is the harmonized twin
// of the apply test: the flag-selected reclaim deletes the superseded
// harmonized generation's rows, sweeps its orphan body, and clears the flag
// while the active generation stays intact. Harmonized activations write no
// files, so no directory assertions apply.
func TestReclaimCmdApplyRemovesHarmonizedSupersededRows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "managed")
	seedReclaimCmdHarmonizedStore(t, dir, ownedRoot)

	output, err := executeReclaimCmd(t, dir, ownedRoot, []string{"--confirm"})
	if err != nil {
		t.Fatalf("reclaim --confirm: %v\n%s", err, output)
	}
	if !strings.Contains(output, "reclaimed 1 session(s), 1 generation(s)") {
		t.Fatalf("apply output missing the reclaim summary:\n%s", output)
	}
	if got := reclaimCmdQueryInt(t, dir, `SELECT COUNT(*) FROM session_generations WHERE session_id = ? AND generation_id = ?`, reclaimCmdSessionID, reclaimCmdOldGen); got != 0 {
		t.Fatalf("superseded harmonized generation keeps %d catalog row(s)", got)
	}
	if got := reclaimCmdQueryInt(t, dir, `SELECT COUNT(*) FROM session_generations WHERE session_id = ? AND generation_id = ?`, reclaimCmdSessionID, reclaimCmdActiveGen); got != 1 {
		t.Fatalf("active harmonized generation keeps %d catalog row(s), want 1", got)
	}
	if got := reclaimCmdQueryInt(t, dir, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, reclaimCmdSessionID); got != 1 {
		t.Fatalf("%d bodies remain, want the active generation's 1", got)
	}
	if got := reclaimCmdQueryInt(t, dir, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, reclaimCmdSessionID); got != 0 {
		t.Fatalf("reclaimed session keeps its sweep flag: %d", got)
	}
}

// TestReclaimCmdApplyJSONReportsOrphanCounts is the harmonized twin of the
// JSON test: the machine-readable result reports the orphan bodies and
// blobs the pass swept alongside the per-table rows.
func TestReclaimCmdApplyJSONReportsOrphanCounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "managed")
	seedReclaimCmdHarmonizedStore(t, dir, ownedRoot)

	output, err := executeReclaimCmd(t, dir, ownedRoot, []string{"--confirm", "--json"})
	if err != nil {
		t.Fatalf("reclaim --confirm --json: %v\n%s", err, output)
	}
	var decoded struct {
		Sessions      int            `json:"sessions"`
		Generations   int            `json:"generations"`
		Rows          map[string]any `json:"rows"`
		BodiesDeleted int64          `json:"bodies_deleted"`
		BlobsDeleted  int64          `json:"blobs_deleted"`
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("decode apply JSON: %v\n%s", err, output)
	}
	if decoded.Sessions != 1 || decoded.Generations != 1 {
		t.Fatalf("unexpected apply JSON sessions/generations: %s", output)
	}
	if decoded.BodiesDeleted != 1 {
		t.Fatalf("apply JSON bodies_deleted = %d, want the superseded generation's 1 orphan body: %s", decoded.BodiesDeleted, output)
	}
	if decoded.BlobsDeleted != 0 {
		t.Fatalf("apply JSON blobs_deleted = %d, want 0: every ref is emitted: %s", decoded.BlobsDeleted, output)
	}
	if decoded.Rows["total"] == float64(0) {
		t.Fatalf("apply JSON rows total is zero with a superseded generation reclaimed: %s", output)
	}
}
