package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

const (
	pruneTestSessionA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	pruneTestSessionB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	pruneTestProject  = "1111111111111111111111111111111111111111111111111111111111111111"
)

// executePruneCmd runs the prune command under a test root with --data-dir=dir
// AND --config-dir=dir (via the shared executeWithDataDir helper), capturing
// combined stdout+stderr as the first return value. The prune command resolves
// both its DB (openDB) and its peasant-sync directory (pruneFilesystem) from the
// --data-dir flag, so this is fully parallel-safe.
func executePruneCmd(t *testing.T, dir string, args []string) (stdout string, stderr string, err error) {
	t.Helper()
	out, err := executeWithDataDir(t, BuildPruneCommand(), dir, args)
	return out, "", err
}

// dataDirCmd returns a throwaway command carrying a --data-dir flag set to dir,
// so pruneFilesystem(cmd, ...) resolves its output directory to <dir>/peasant
// without touching process-global env. Parallel-safe.
func dataDirCmd(t *testing.T, dir string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().String("data-dir", "", "")
	if err := cmd.Flags().Set("data-dir", dir); err != nil {
		t.Fatalf("dataDirCmd: set --data-dir: %v", err)
	}
	return cmd
}

// seedPruneTestSessions inserts two sessions using a single DB open, under the
// data directory derived from dir (matching the command's --data-dir=dir).
func seedPruneTestSessions(t *testing.T, dir string) {
	t.Helper()
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("seed: create data directory: %v", err)
	}
	storetest.CopyGoldenTo(t, dbPath)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed: open store: %v", err)
	}
	defer db.Close()

	startA := int64(1700000000000)
	startB := int64(1700100000000)
	ingestedA := startA + 120000
	ingestedB := startB + 120000

	entries := []ingest.StoreEntry{
		{
			Metadata: &schema.UnifiedMetadata{
				SchemaVersion: ingest.CurrentSchemaVersion,
				SessionID:     schema.SessionID(pruneTestSessionA),
				ModelHarness:  defaults.HarnessClaudeCode,
				Model:         schema.ModelID("claude-opus-4-6"),
				HostSlug:      schema.HostSlug("github.com-test-prune"),
				Project: schema.ProjectContext{
					Hash:     schema.ProjectHash(pruneTestProject),
					Name:     "prune-test",
					FilePath: "/test/prune",
				},
				Timestamp: schema.TimestampInfo{Start: startA, End: startA + 60000, Ingested: &ingestedA},
				Source:    schema.SourceInfo{FilePath: "/test/a.jsonl", Format: schema.SourceFormatJSONL},
			},
		},
		{
			Metadata: &schema.UnifiedMetadata{
				SchemaVersion: ingest.CurrentSchemaVersion,
				SessionID:     schema.SessionID(pruneTestSessionB),
				ModelHarness:  defaults.HarnessOpenCode,
				Model:         schema.ModelID("claude-opus-4-6"),
				HostSlug:      schema.HostSlug("github.com-test-prune"),
				Project: schema.ProjectContext{
					Hash:     schema.ProjectHash(pruneTestProject),
					Name:     "prune-test",
					FilePath: "/test/prune",
				},
				Timestamp: schema.TimestampInfo{Start: startB, End: startB + 60000, Ingested: &ingestedB},
				Source:    schema.SourceInfo{FilePath: "/test/b.jsonl", Format: schema.SourceFormatJSONL},
			},
		},
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatalf("seed: insert sessions: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Safety model tests
// ---------------------------------------------------------------------------

func TestPruneCmd_NoFilter_Error(t *testing.T) {
	t.Parallel()
	_, _, err := executePruneCmd(t, t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error when no filter provided")
	}
	if !strings.Contains(err.Error(), "at least one filter") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPruneCmd_InvalidHarness_Error(t *testing.T) {
	t.Parallel()
	_, _, err := executePruneCmd(t, t.TempDir(), []string{"--harness", "invalid-harness"})
	if err == nil {
		t.Fatal("expected error for invalid harness")
	}
	wantSubstr := "must be one of " + joinHarnesses(defaults.AllHarnesses)
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("invalid --harness error should contain %q; got: %v", wantSubstr, err)
	}
}

func TestPruneCmd_InvalidDate_Error(t *testing.T) {
	t.Parallel()
	_, _, err := executePruneCmd(t, t.TempDir(), []string{"--before", "not-a-date"})
	if err == nil {
		t.Fatal("expected error for invalid date")
	}
	if !strings.Contains(err.Error(), "invalid --before date") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPruneCmd_Help(t *testing.T) {
	t.Parallel()
	stdout, _, err := executePruneCmd(t, t.TempDir(), []string{"--help"})
	if err != nil {
		t.Fatalf("prune --help: unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "prune") {
		t.Error("help output should mention 'prune'")
	}
	if !strings.Contains(stdout, "--session") {
		t.Error("help output should mention --session flag")
	}
	if !strings.Contains(stdout, "--dry-run") {
		t.Error("help output should mention --dry-run flag")
	}
}

func TestPruneCmd_DryRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	stdout, _, err := executePruneCmd(t, dir, []string{"--all", "--dry-run"})
	if err != nil {
		t.Fatalf("prune --all --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "dry run") {
		t.Errorf("expected 'dry run' in output; got: %s", stdout)
	}
	if !strings.Contains(stdout, "2 session(s) would be deleted") {
		t.Errorf("expected '2 session(s) would be deleted'; got: %s", stdout)
	}

	// Verify sessions still exist (dry run didn't delete).
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 sessions after dry run, got %d", len(rows))
	}
}

func TestPruneCmd_DryRun_JSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	stdout, _, err := executePruneCmd(t, dir, []string{"--all", "--dry-run", "--json"})
	if err != nil {
		t.Fatalf("prune --all --dry-run --json: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("JSON parse error: %v\noutput: %s", err, stdout)
	}
	if result["dry_run"] != true {
		t.Errorf("expected dry_run=true; got: %v", result)
	}
	sessions, ok := result["sessions"].([]any)
	if !ok || len(sessions) != 2 {
		t.Errorf("expected 2 sessions in JSON; got: %v", result)
	}

	// Each row must have a correctly typed "harness" key; "provider" must be absent.
	wantHarnesses := map[string]bool{
		string(defaults.HarnessClaudeCode): true,
		string(defaults.HarnessOpenCode):   true,
	}
	for i, raw := range sessions {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("session[%d] is not a map: %T", i, raw)
			continue
		}
		harnessVal, hasHarness := row["harness"]
		if !hasHarness {
			t.Errorf("session[%d]: expected JSON key \"harness\" but it was absent; row: %v", i, row)
		} else if !wantHarnesses[harnessVal.(string)] {
			t.Errorf("session[%d]: unexpected harness value %q; want one of %v", i, harnessVal, wantHarnesses)
		}
		if _, hasProvider := row["provider"]; hasProvider {
			t.Errorf("session[%d]: JSON key \"provider\" must be absent but was present; row: %v", i, row)
		}
	}

	// Preview output is not present in --json mode, so verify
	// the non-JSON dry-run path renders HARNESS in a separate test (TestPruneCmd_DryRun_HasHARNESSHeader).
}

// TestPruneCmd_DryRun_HasHARNESSHeader verifies the table preview uses HARNESS (not PROVIDER).
func TestPruneCmd_DryRun_HasHARNESSHeader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	stdout, _, err := executePruneCmd(t, dir, []string{"--all", "--dry-run"})
	if err != nil {
		t.Fatalf("prune --all --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "HARNESS") {
		t.Errorf("expected preview header to contain HARNESS; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "PROVIDER") {
		t.Errorf("preview header must not contain PROVIDER; got:\n%s", stdout)
	}
}

func TestPruneCmd_Confirm_Delete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	// A transcript directory with no database row. An EMPTY host-slug directory
	// is deliberately not enough: it holds no transcripts, so reporting it would
	// be noise rather than an orphaned-session result.
	orphan := loadPruneExactFixtures(t).ConsentWindow.OrphanSession
	syncDir := filepath.Join(string(defaults.ResolveDataDirPathWith(dir)), "peasant-sync")
	orphanRelative := filepath.Join(orphan.OutputPath, orphan.SessionID)
	if err := os.MkdirAll(filepath.Join(syncDir, orphanRelative), 0o700); err != nil {
		t.Fatalf("create unplanned transcript residue: %v", err)
	}
	stdout, _, err := executePruneCmd(t, dir, []string{"--all", "--confirm"})
	if err != nil {
		t.Fatalf("prune --all --confirm: %v", err)
	}
	if !strings.Contains(stdout, "deleted 2 session(s)") {
		t.Errorf("expected 'deleted 2 session(s)'; got: %s", stdout)
	}
	// The notice has to name the specific path it found, not just count it: a
	// count with nothing beside it tells the user something remains without
	// telling them what, so they cannot act on it without going and looking.
	if !strings.Contains(stdout, syncDir) || !strings.Contains(stdout, orphanRelative) {
		t.Errorf("the leftover-transcript notice does not name the orphaned directory %q; got output: %s", orphanRelative, stdout)
	}

	// Verify sessions are gone.
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 sessions after prune, got %d", len(rows))
	}
}

func TestPruneCmd_Confirm_JSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	stdout, _, err := executePruneCmd(t, dir, []string{"--all", "--confirm", "--json"})
	if err != nil {
		t.Fatalf("prune --all --confirm --json: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("JSON parse error: %v\noutput: %s", err, stdout)
	}
	deleted, ok := result["deleted"].(float64)
	if !ok || deleted != 2 {
		t.Errorf("expected deleted=2; got: %v", result)
	}
}

func TestPruneCmd_SessionFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	// Prune only session A.
	stdout, _, err := executePruneCmd(t, dir, []string{"--session", pruneTestSessionA, "--confirm"})
	if err != nil {
		t.Fatalf("prune --session: %v", err)
	}
	if !strings.Contains(stdout, "deleted 1 session(s)") {
		t.Errorf("expected 'deleted 1 session(s)'; got: %s", stdout)
	}

	// Session B should still exist.
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session remaining, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneTestSessionB) {
		t.Errorf("expected session B to remain, got %s", rows[0].SessionID)
	}
}

func TestPruneCmd_HarnessFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	// Prune only opencode sessions.
	stdout, _, err := executePruneCmd(t, dir, []string{"--harness", string(defaults.HarnessOpenCode), "--confirm"})
	if err != nil {
		t.Fatalf("prune --harness: %v", err)
	}
	if !strings.Contains(stdout, "deleted 1 session(s)") {
		t.Errorf("expected 'deleted 1 session(s)'; got: %s", stdout)
	}
}

func TestPruneCmd_BeforeFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	// Session A is at 2023-11-14, B is at 2023-11-16.
	// Prune before 2023-11-15 should only get A.
	stdout, _, err := executePruneCmd(t, dir, []string{"--before", "2023-11-15", "--confirm"})
	if err != nil {
		t.Fatalf("prune --before: %v", err)
	}
	if !strings.Contains(stdout, "deleted 1 session(s)") {
		t.Errorf("expected 'deleted 1 session(s)'; got: %s", stdout)
	}
}

func TestPruneCmd_NoMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	// Use a date far in the past so nothing matches.
	stdout, _, err := executePruneCmd(t, dir, []string{"--before", "2020-01-01", "--confirm"})
	if err != nil {
		t.Fatalf("prune --before 2020: %v", err)
	}
	if !strings.Contains(stdout, "no sessions match") {
		t.Errorf("expected 'no sessions match'; got: %s", stdout)
	}
}

func TestPruneCmd_NonTTY_NoConfirm_Error(t *testing.T) {
	// NOT parallel: this test mutates the process-global os.Stdin. It uses
	// --data-dir flag injection (no t.Setenv) but stays serial because of the
	// os.Stdin swap below.
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	// Redirect stdin to /dev/null to simulate non-TTY.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer devNull.Close()

	oldStdin := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = oldStdin }()

	_, _, err = executePruneCmd(t, dir, []string{"--all"})
	if err == nil {
		t.Fatal("expected error for non-TTY without --confirm")
	}
	if !strings.Contains(err.Error(), "non-interactive terminal") {
		t.Errorf("unexpected error: %v", err)
	}
	// The refusal tells the user "nothing was deleted". That is a promise made
	// at a destructive boundary, so it is asserted rather than trusted.
	if !strings.Contains(err.Error(), "nothing was deleted") {
		t.Errorf("the refusal must say what state the store is in; got: %v", err)
	}
	db, openErr := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if openErr != nil {
		t.Fatalf("open store after refused prompt: %v", openErr)
	}
	defer db.Close()
	remaining, queryErr := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if queryErr != nil {
		t.Fatalf("count sessions after refused prompt: %v", queryErr)
	}
	if len(remaining) == 0 {
		t.Error("the refusal claims nothing was deleted, but every session is gone")
	}
}

// buildPruneFilter is tested indirectly through CLI tests above.
// This tests the date parsing directly.
func TestBuildPruneFilter_DateParsing(t *testing.T) {
	t.Parallel()
	filter, err := buildPruneFilter(nil, "", "", "2024-01-15", "2024-03-20", false)
	if err != nil {
		t.Fatalf("buildPruneFilter: %v", err)
	}

	expectedBefore := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC).UnixMilli()
	if filter.Before == nil || *filter.Before != expectedBefore {
		t.Errorf("before: expected %d, got %v", expectedBefore, filter.Before)
	}

	expectedAfter := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC).UnixMilli()
	if filter.After == nil || *filter.After != expectedAfter {
		t.Errorf("after: expected %d, got %v", expectedAfter, filter.After)
	}
}

// ---------------------------------------------------------------------------
// Filesystem cleanup tests
// ---------------------------------------------------------------------------

func TestPruneFilesystem_ExactPlanLeavesSyncRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	syncDir := filepath.Join(dir, "peasant", "peasant-sync")
	hostSlugDir := filepath.Join(syncDir, "github.com--test--repo")
	sessionDir := filepath.Join(hostSlugDir, pruneTestSessionA)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	sessions := []ingest.PruneSessionRow{
		{SessionID: schema.SessionID(pruneTestSessionA), OutputPath: "github.com--test--repo"},
	}

	errs := pruneFilesystem(dataDirCmd(t, dir), sessions)
	if len(errs) > 0 {
		t.Fatalf("pruneFilesystem errors: %v", errs)
	}

	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("expected planned session directory to be removed; stat err = %v", err)
	}
	if _, err := os.Stat(syncDir); err != nil {
		t.Errorf("expected sync root to remain available for concurrent ingest: %v", err)
	}
}

func TestPruneFilesystem_Selective_RemovesEmptyParent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	syncDir := filepath.Join(dir, "peasant", "peasant-sync")
	// Host slug A: will become empty after prune.
	slugA := filepath.Join(syncDir, "github.com--test--repo-a")
	if err := os.MkdirAll(filepath.Join(slugA, pruneTestSessionA), 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Host slug B: will still have a session after prune (not pruned).
	slugB := filepath.Join(syncDir, "github.com--test--repo-b")
	if err := os.MkdirAll(filepath.Join(slugB, pruneTestSessionB), 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	sessions := []ingest.PruneSessionRow{
		{SessionID: schema.SessionID(pruneTestSessionA), OutputPath: "github.com--test--repo-a"},
	}

	errs := pruneFilesystem(dataDirCmd(t, dir), sessions)
	if len(errs) > 0 {
		t.Fatalf("pruneFilesystem errors: %v", errs)
	}

	// Slug A should be gone (was emptied).
	if _, err := os.Stat(slugA); !os.IsNotExist(err) {
		t.Errorf("expected empty host slug dir to be removed; stat err = %v", err)
	}
	// Slug B should remain (still has sessions).
	if _, err := os.Stat(slugB); err != nil {
		t.Errorf("expected non-empty host slug dir to remain; stat err = %v", err)
	}
	// peasant-sync/ should remain.
	if _, err := os.Stat(syncDir); err != nil {
		t.Errorf("expected peasant-sync/ to remain; stat err = %v", err)
	}
}

func TestPruneFilesystem_Selective_PreservesNonEmptyParent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	syncDir := filepath.Join(dir, "peasant", "peasant-sync")
	slugDir := filepath.Join(syncDir, "github.com--test--repo")
	// Two sessions under same host slug — only prune one.
	if err := os.MkdirAll(filepath.Join(slugDir, pruneTestSessionA), 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(slugDir, pruneTestSessionB), 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	sessions := []ingest.PruneSessionRow{
		{SessionID: schema.SessionID(pruneTestSessionA), OutputPath: "github.com--test--repo"},
	}

	errs := pruneFilesystem(dataDirCmd(t, dir), sessions)
	if len(errs) > 0 {
		t.Fatalf("pruneFilesystem errors: %v", errs)
	}

	// Session A gone.
	if _, err := os.Stat(filepath.Join(slugDir, pruneTestSessionA)); !os.IsNotExist(err) {
		t.Errorf("expected session A dir to be removed; stat err = %v", err)
	}
	// Session B remains.
	if _, err := os.Stat(filepath.Join(slugDir, pruneTestSessionB)); err != nil {
		t.Errorf("expected session B dir to remain; stat err = %v", err)
	}
	// Host slug dir remains (not empty).
	if _, err := os.Stat(slugDir); err != nil {
		t.Errorf("expected host slug dir to remain; stat err = %v", err)
	}
}

// executePruneCmdWithConfig runs BuildPruneCommand under a root that has the
// --config persistent flag. This allows tests that use --unselected to pass
// a config file path just like the real CLI.
func executePruneCmdWithConfig(t *testing.T, dir, cfgPath string, args []string) (stdout string, stderr string, err error) {
	t.Helper()
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", cfgPath, "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	cmd := BuildPruneCommand()
	root.AddCommand(cmd)

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "prune"}, args...))
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// seedPruneTestSessionsWithGitRemote inserts two sessions:
//   - Session C: claude provider, git_remote="git@github.com:selected/repo.git" (selected)
//   - Session D: opencode provider, no git remote, project name "unselected-project" (not selected)
//
// The caller writes a config that selects only claude + the above git remote.
func seedPruneTestSessionsWithGitRemote(t *testing.T, dir string) {
	t.Helper()
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("seed: create data directory: %v", err)
	}
	storetest.CopyGoldenTo(t, dbPath)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed: open store: %v", err)
	}
	defer db.Close()

	startC := int64(1700200000000)
	startD := int64(1700300000000)
	ingestedC := startC + 120000
	ingestedD := startD + 120000
	selectedRemote := "git@github.com:selected/repo.git"
	selectedPath := filepath.Join(dir, "projects", "selected")
	unselectedPath := filepath.Join(dir, "projects", "unselected")
	for _, projectPath := range []string{selectedPath, unselectedPath} {
		if err := os.MkdirAll(projectPath, 0o755); err != nil {
			t.Fatalf("seed: create project path: %v", err)
		}
	}

	entries := []ingest.StoreEntry{
		{
			Metadata: &schema.UnifiedMetadata{
				SchemaVersion: ingest.CurrentSchemaVersion,
				SessionID:     schema.SessionID(pruneTestSessionA),
				ModelHarness:  defaults.HarnessClaudeCode,
				Model:         schema.ModelID("claude-opus-4-6"),
				HostSlug:      schema.HostSlug("github.com-selected-repo"),
				Git: schema.GitContext{
					Remote: &selectedRemote,
				},
				Project: schema.ProjectContext{
					Hash:     schema.ProjectHash(pruneTestProject),
					Name:     "selected-project",
					FilePath: selectedPath,
				},
				Timestamp: schema.TimestampInfo{Start: startC, End: startC + 60000, Ingested: &ingestedC},
				Source:    schema.SourceInfo{FilePath: "/test/c.jsonl", Format: schema.SourceFormatJSONL},
			},
		},
		{
			Metadata: &schema.UnifiedMetadata{
				SchemaVersion: ingest.CurrentSchemaVersion,
				SessionID:     schema.SessionID(pruneTestSessionB),
				ModelHarness:  defaults.HarnessOpenCode,
				Model:         schema.ModelID("claude-opus-4-6"),
				HostSlug:      schema.HostSlug("github.com-unselected-repo"),
				Project: schema.ProjectContext{
					Hash:     schema.ProjectHash("2222222222222222222222222222222222222222222222222222222222222222"),
					Name:     "unselected-project",
					FilePath: unselectedPath,
				},
				Timestamp: schema.TimestampInfo{Start: startD, End: startD + 60000, Ingested: &ingestedD},
				Source:    schema.SourceInfo{FilePath: "/test/d.jsonl", Format: schema.SourceFormatJSONL},
			},
		},
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatalf("seed: insert sessions: %v", err)
	}
}

// writeSelectionConfig writes a config.yaml that selects only claude sessions
// with the given git remote into the config directory derived from XDG_CONFIG_HOME.
// It uses YAML marshalling (not string concatenation) so that special characters
// in gitRemote are safely quoted.
func writeSelectionConfig(t *testing.T, dir, gitRemote string) string {
	t.Helper()
	cfgDir := string(defaults.ResolveConfigDirPathWith(dir))
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("writeSelectionConfig: mkdir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, string(defaults.Config.FileName))

	cfg := config.Config{
		Version: 1,
		Redaction: config.RedactionConfig{
			Level: redact.Standard,
		},
		Sources: config.SourcesConfig{
			ClaudeCode: config.SourceProviderConfig{
				Enabled: true,
				Paths:   []string{"~/.claude/projects"},
			},
			OpenCode: config.SourceProviderConfig{
				Enabled: false,
				Paths:   []string{},
			},
		},
		Output: config.OutputConfig{
			BasePath:              "~/.local/share/peasant",
			StalenessThresholdSec: 60,
		},
		Selection: config.SelectionConfig{
			Mode: config.SelectionModeSelected,
			Harnesses: map[string]config.SelectionHarnessConfig{
				string(defaults.HarnessClaudeCode): {
					Projects: []config.ProjectSelection{
						{GitRemote: gitRemote},
					},
				},
			},
		},
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("writeSelectionConfig: marshal: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatalf("writeSelectionConfig: write: %v", err)
	}
	return cfgPath
}

// ---------------------------------------------------------------------------
// --unselected flag tests
// ---------------------------------------------------------------------------

func TestPruneCmd_Unselected_DryRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Seed: session A (claude, selected git remote) + session B (opencode, unselected).
	seedPruneTestSessionsWithGitRemote(t, dir)

	// Write config: only claude sessions with the selected git remote are wanted.
	cfgPath := writeSelectionConfig(t, dir, "git@github.com:selected/repo.git")

	// Dry run: should list only session B (opencode/unselected) as a candidate.
	stdout, _, err := executePruneCmdWithConfig(t, dir, cfgPath, []string{"--unselected", "--dry-run"})
	if err != nil {
		t.Fatalf("prune --unselected --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "dry run") {
		t.Errorf("expected 'dry run' in output; got: %s", stdout)
	}
	if !strings.Contains(stdout, "1 session(s) would be deleted") {
		t.Errorf("expected '1 session(s) would be deleted'; got: %s", stdout)
	}
	// The selected session (A) must NOT appear in output.
	if strings.Contains(stdout, pruneTestSessionA) {
		t.Errorf("selected session A should not appear in --unselected output; got: %s", stdout)
	}
	// The unselected session (B) should appear.
	if !strings.Contains(stdout, pruneTestSessionB) {
		t.Errorf("unselected session B should appear in --unselected output; got: %s", stdout)
	}

	// Verify DB is unchanged after dry run.
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 sessions after dry run, got %d", len(rows))
	}
}

func TestPruneCmd_Unselected_Confirm_DeletesOnlyUnselected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Seed: session A (claude, selected git remote) + session B (opencode, unselected).
	seedPruneTestSessionsWithGitRemote(t, dir)

	// Write config: only claude sessions with the selected git remote are wanted.
	cfgPath := writeSelectionConfig(t, dir, "git@github.com:selected/repo.git")

	// Execute deletion of unselected sessions.
	stdout, _, err := executePruneCmdWithConfig(t, dir, cfgPath, []string{"--unselected", "--confirm"})
	if err != nil {
		t.Fatalf("prune --unselected --confirm: %v", err)
	}
	if !strings.Contains(stdout, "deleted 1 session(s)") {
		t.Errorf("expected 'deleted 1 session(s)'; got: %s", stdout)
	}

	// Verify DB state: session A (selected) must remain, session B (unselected) must be gone.
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("QueryPrunableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session remaining after --unselected --confirm, got %d", len(rows))
	}
	if rows[0].SessionID != schema.SessionID(pruneTestSessionA) {
		t.Errorf("expected selected session A (%s) to remain, got %s", pruneTestSessionA, rows[0].SessionID)
	}
}

func TestPruneCmd_Unselected_RequiresSelectedMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write config with mode=all (should be rejected).
	cfgDir := string(defaults.ResolveConfigDirPathWith(dir))
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, string(defaults.Config.FileName))
	cfgContent := `version: 1
redaction:
  level: standard
sources:
  claude-code:
    enabled: true
    paths: ["~/.claude/projects"]
  opencode:
    enabled: false
    paths: []
output:
  basePath: "~/.local/share/peasant"
  stalenessThresholdSec: 60
selection:
  mode: all
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, _, err := executePruneCmdWithConfig(t, dir, cfgPath, []string{"--unselected", "--dry-run"})
	if err == nil {
		t.Fatal("expected error when selection.mode=all with --unselected")
	}
	if !strings.Contains(err.Error(), "selection.mode") {
		t.Errorf("expected error mentioning 'selection.mode'; got: %v", err)
	}
}

// Suppress unused import warnings.
var _ = time.Now
