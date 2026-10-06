package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
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

// seedReclaimCmdStore seeds one session with a superseded and an active managed
// generation into the database and owned-artifact root the command will use.
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
