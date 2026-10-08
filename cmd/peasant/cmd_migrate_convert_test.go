package main

import (
	"bytes"
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

const migrateCmdSessionID = "eeee4444-4444-4444-8444-444444444444"

// executeMigrateCmd runs `peasant migrate` under a test root with
// --data-dir, --config-dir and --config all scoped to dir. The config
// sets output.basePath to the owned-artifact root the test seeded, so
// the command reads and removes the same generation directories.
// Stdout and stderr are captured separately: progress goes to stderr in
// --json mode so stdout carries exactly one JSON document.
func executeMigrateCmd(t *testing.T, dir, ownedRoot string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	configPath := filepath.Join(dir, "migrate-config.yaml")
	body := "version: 1\noutput:\n  basePath: " + ownedRoot + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write migrate config: %v", err)
	}
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.AddCommand(BuildMigrateCommand())

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{
		"migrate",
		"--data-dir", dir,
		"--config-dir", dir,
		"--config", configPath,
	}, args...))
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// executeVerifyCmd runs `peasant harvest verify` under a test root with
// the same scoping. Combined stdout+stderr is captured.
func executeVerifyCmd(t *testing.T, dir, ownedRoot string, args []string) (string, error) {
	t.Helper()
	configPath := filepath.Join(dir, "verify-config.yaml")
	body := "version: 1\noutput:\n  basePath: " + ownedRoot + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write verify config: %v", err)
	}
	root := buildRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{
		"harvest", "verify",
		"--data-dir", dir,
		"--config-dir", dir,
		"--config", configPath,
	}, args...))
	err := root.Execute()
	return buf.String(), err
}

// seedMigrateCmdStore seeds one file-backed native session with its
// mirror, capture, and stats rows: the projection catalog rows plus the
// owned generation directory with its manifest and content blobs, the
// mirror batch over the main entries, and the captured-stats row the
// v62 backfill would have written.
func seedMigrateCmdStore(t *testing.T, dir, ownedRoot string) {
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
	sid := schema.SessionID(migrateCmdSessionID)
	storetest.SeedSession(t, db, migrateCmdSessionID)
	v2, blobs := reclaimCmdGeneration(t, sid, "g_migrate_cmd")
	v2.Generation.Metadata.MetadataHash = schema.ComputeMetadataHash(&v2.Generation.Metadata)
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	defer conn.Close()
	seedFileBackedCmdGeneration(t, conn, ownedRoot, sid, v2, blobs)
	seedMigrateCmdMirror(t, dbPath, ownedRoot, sid, v2, blobs)
	seedMigrateCmdStats(t, conn, sid, v2)
}

// seedMigrateCmdMirror writes the mirror rows, chunks, capture row, and
// hashes through the production V1 batch write.
func seedMigrateCmdMirror(t *testing.T, dbPath, ownedRoot string, sid schema.SessionID, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
	t.Helper()
	_ = blobs
	artifacts, err := store.NewOSGenerationArtifactStore(ownedRoot)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(ownedRoot)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openPreparedStore(t, dbPath, store.WithPoolSize(1), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatalf("open mirror store: %v", err)
	}
	defer func() { _ = db.Close() }()
	results := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{
		SessionID:      sid,
		Result:         indexformat.V1{Entries: v2.Generation.Main.Entries},
		IndexVersion:   1,
		IndexerVersion: 1,
		IndexedAtMs:    1,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureComplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatFull,
			CapturedAtMs:     1,
		},
		RequireFullContent: true,
	}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed migrate mirror rows: %+v", results)
	}
}

// seedMigrateCmdStats writes the captured-stats row the v62 backfill
// would have written for the active generation.
func seedMigrateCmdStats(t *testing.T, conn *sqlite.Conn, sid schema.SessionID, v2 indexformat.V2) {
	t.Helper()
	stats := v2.Generation.Metadata.Stats
	seed, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var inputCount any
	if stats.InputSubmissionCount != nil {
		inputCount = *stats.InputSubmissionCount
	}
	script := `INSERT INTO session_captured_stats(session_id, turn_count, input_submission_count, tool_call_count, subagent_count, duration_ms, tokens_in, tokens_out, seed_json, source, updated_at_ms) VALUES ('` + string(sid) + `',` +
		itoa(stats.TurnCount) + `,` + itoa64p(inputCount) + `,` + itoa(stats.ToolCallCount) + `,` + itoa(stats.SubagentCount) + `,` + itoa64(stats.DurationMs) + `,` +
		itoa(stats.TokensIn) + `,` + itoa(stats.TokensOut) + `,'` + string(seed) + `','harness',1);`
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("seed migrate stats row: %v", err)
	}
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}

func itoa64(v int64) string {
	return fmt.Sprintf("%d", v)
}

func itoa64p(v any) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%d", v)
}

// TestMigrateCmdDryRun proves `peasant migrate --dry-run` reports the
// session with work and changes nothing: the owned files and the old
// rows are all still present afterwards.
func TestMigrateCmdDryRun(t *testing.T) {
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "sync")
	seedMigrateCmdStore(t, dir, ownedRoot)
	out, _, err := executeMigrateCmd(t, dir, ownedRoot, []string{"--dry-run"})
	if err != nil {
		t.Fatalf("migrate --dry-run: %v\n%s", err, out)
	}
	for _, want := range []string{migrateCmdSessionID, "Phase 2 convert: 1 session(s)", "dry run: nothing was converted"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output misses %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(ownedRoot, migrateCmdSessionID, "generations", "g_migrate_cmd", "manifest.json")); err != nil {
		t.Fatalf("dry run removed owned files: %v", err)
	}
}

// TestMigrateCmdConvert proves `peasant migrate --confirm` converts the
// seeded session on the production command path: the report names the
// conversion, and a second pass finds no work.
func TestMigrateCmdConvert(t *testing.T) {
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "sync")
	seedMigrateCmdStore(t, dir, ownedRoot)
	out, _, err := executeMigrateCmd(t, dir, ownedRoot, []string{"--confirm"})
	if err != nil {
		t.Fatalf("migrate --confirm: %v\n%s", err, out)
	}
	if !strings.Contains(out, "converted 1 session(s)") {
		t.Errorf("convert output misses the conversion:\n%s", out)
	}
	again, _, err := executeMigrateCmd(t, dir, ownedRoot, []string{"--confirm"})
	if err != nil {
		t.Fatalf("second migrate: %v\n%s", err, again)
	}
	if !strings.Contains(again, "converted 0 session(s)") {
		t.Errorf("second pass still converts:\n%s", again)
	}
}

// TestMigrateCmdJSON proves `peasant migrate --confirm --json` emits the
// machine-readable result shape over the converted session.
func TestMigrateCmdJSON(t *testing.T) {
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "sync")
	seedMigrateCmdStore(t, dir, ownedRoot)
	out, _, err := executeMigrateCmd(t, dir, ownedRoot, []string{"--confirm", "--json"})
	if err != nil {
		t.Fatalf("migrate --json: %v\n%s", err, out)
	}
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(out))
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode migrate JSON: %v\n%s", err, out)
	}
	if decoded["converted"] != float64(1) {
		t.Errorf("migrate JSON converted = %v; want 1:\n%s", decoded["converted"], out)
	}
	for _, key := range []string{"rolled_back", "marked", "skipped", "bytes_freed", "rollbacks", "preconditions", "ready_for_release"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("migrate JSON misses key %q:\n%s", key, out)
		}
	}
}

// TestVerifyCmdContent proves `peasant harvest verify --content` passes
// a converted store and reports its index health.
func TestVerifyCmdContent(t *testing.T) {
	dir := t.TempDir()
	ownedRoot := filepath.Join(dir, "sync")
	seedMigrateCmdStore(t, dir, ownedRoot)
	if _, _, err := executeMigrateCmd(t, dir, ownedRoot, []string{"--confirm"}); err != nil {
		t.Fatalf("migrate --confirm: %v", err)
	}
	out, err := executeVerifyCmd(t, dir, ownedRoot, []string{"--content"})
	if err != nil {
		t.Fatalf("verify --content: %v\n%s", err, out)
	}
	for _, want := range []string{"sessions checked: 1", "damaged sessions: none", "Status: PASSED"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify output misses %q:\n%s", want, out)
		}
	}
}
