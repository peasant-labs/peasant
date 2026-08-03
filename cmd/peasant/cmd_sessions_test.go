package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// testSessionUUID mirrors testutil.TestSessionUUID for use in package main tests.
const testSessionUUID = "99d59925-36bc-424c-a789-8be54d9702ba"

// executeSessionsCmd is a test helper that runs the sessions cobra command with the
// given arguments under a test root with --data-dir=dir (parallel-safe; no t.Setenv),
// captures combined output, and returns (output, error).
func executeSessionsCmd(t *testing.T, dir string, args []string) (string, error) {
	t.Helper()
	return executeWithDataDir(t, BuildSessionsCommand(), dir, args)
}

// TestSessionsCmd_Help verifies that peasant sessions tag --help shows usage information.
func TestSessionsCmd_Help(t *testing.T) {
	t.Parallel()
	output, err := executeSessionsCmd(t, t.TempDir(), []string{"tag", "--help"})
	if err != nil {
		t.Fatalf("sessions tag --help: unexpected error: %v\noutput: %s", err, output)
	}

	// Verify key sections are present.
	if !strings.Contains(output, "tag") {
		t.Error("help output should mention 'tag'")
	}
}

// TestSessionsCmd_TagSubcommandExists verifies the tag subcommand is registered.
func TestSessionsCmd_TagSubcommandExists(t *testing.T) {
	t.Parallel()
	cmd := BuildSessionsCommand()
	subCmd, _, err := cmd.Find([]string{"tag"})
	if err != nil {
		t.Fatalf("sessions tag subcommand not found: %v", err)
	}
	if subCmd.Name() != "tag" {
		t.Errorf("expected subcommand name 'tag', got %q", subCmd.Name())
	}
}

// TestSessionsCmd_TagSubcommandsExist verifies the tag sub-subcommands
// (add, remove, list) are registered under sessions tag.
func TestSessionsCmd_TagSubcommandsExist(t *testing.T) {
	t.Parallel()
	cmd := BuildSessionsCommand()
	tagCmd, _, err := cmd.Find([]string{"tag"})
	if err != nil {
		t.Fatalf("sessions tag subcommand not found: %v", err)
	}

	expectedSubcmds := []string{"add", "remove", "list"}
	for _, name := range expectedSubcmds {
		found := false
		for _, c := range tagCmd.Commands() {
			if c.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sessions tag subcommand %q not found; available: %v", name, subcommandNames(tagCmd))
		}
	}
}

// TestSessionsCmd_TagAddHelp verifies that peasant sessions tag add --help works.
func TestSessionsCmd_TagAddHelp(t *testing.T) {
	t.Parallel()
	output, err := executeSessionsCmd(t, t.TempDir(), []string{"tag", "add", "--help"})
	if err != nil {
		t.Fatalf("sessions tag add --help: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "add") {
		t.Error("help output should mention 'add'")
	}
}

// TestSessionsCmd_TagRemoveHelp verifies that peasant sessions tag remove --help works.
func TestSessionsCmd_TagRemoveHelp(t *testing.T) {
	t.Parallel()
	output, err := executeSessionsCmd(t, t.TempDir(), []string{"tag", "remove", "--help"})
	if err != nil {
		t.Fatalf("sessions tag remove --help: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "remove") {
		t.Error("help output should mention 'remove'")
	}
}

// TestSessionsCmd_TagListHelp verifies that peasant sessions tag list --help works.
func TestSessionsCmd_TagListHelp(t *testing.T) {
	t.Parallel()
	output, err := executeSessionsCmd(t, t.TempDir(), []string{"tag", "list", "--help"})
	if err != nil {
		t.Fatalf("sessions tag list --help: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "list") {
		t.Error("help output should mention 'list'")
	}
}

// seedTestSession inserts a minimal session row into the store under the given
// data-dir override (dir, the same value passed as --data-dir to the command) so
// the tag CLI commands have a session to operate on.
func seedTestSession(t *testing.T, dir, sessionID string) {
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

	sid := ingest.SessionID(sessionID)
	ingested := time.Now().UnixMilli()
	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: 1,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         ingest.ModelID("claude-opus-4-6"),
			HostSlug:      ingest.HostSlug("github.com--test--repo"),
			Timestamp: ingest.TimestampInfo{
				Start:    time.Now().UnixMilli(),
				End:      time.Now().UnixMilli(),
				Ingested: &ingested,
			},
			Project: ingest.ProjectInfo{
				Hash:     ingest.ProjectHash("testhash"),
				Name:     "test-project",
				FilePath: "/test/path",
			},
			Source: ingest.SourceInfo{
				Format: ingest.SourceFormatJSONL,
			},
		},
	}
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("seed: insert session: %v", err)
	}
}

// TestCLI_SessionsTagList verifies that listing tags on a session with no tags
// shows "no tags", and after adding tags shows them.
func TestCLI_SessionsTagList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	testSID := testSessionUUID
	seedTestSession(t, dir, testSID)

	// List empty: should show "no tags".
	output, err := executeSessionsCmd(t, dir, []string{"tag", "list", testSID})
	if err != nil {
		t.Fatalf("tag list on empty: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "no tags") {
		t.Errorf("expected 'no tags' for empty session; got: %s", output)
	}
}

// TestCLI_SessionsTagAdd verifies adding a tag and listing to confirm it.
func TestCLI_SessionsTagAdd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	testSID := testSessionUUID
	seedTestSession(t, dir, testSID)

	// Add a tag.
	output, err := executeSessionsCmd(t, dir, []string{"tag", "add", testSID, "bugfix"})
	if err != nil {
		t.Fatalf("tag add: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "tag \"bugfix\" added") {
		t.Errorf("expected confirmation message; got: %s", output)
	}

	// List to verify the tag is present.
	output, err = executeSessionsCmd(t, dir, []string{"tag", "list", testSID})
	if err != nil {
		t.Fatalf("tag list after add: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "bugfix") {
		t.Errorf("expected 'bugfix' in tag list; got: %s", output)
	}
}

// TestCLI_SessionsTagRemove verifies adding, then removing a tag, and confirming removal.
func TestCLI_SessionsTagRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	testSID := testSessionUUID
	seedTestSession(t, dir, testSID)

	// Add then remove.
	_, err := executeSessionsCmd(t, dir, []string{"tag", "add", testSID, "wip"})
	if err != nil {
		t.Fatalf("tag add: %v", err)
	}

	output, err := executeSessionsCmd(t, dir, []string{"tag", "remove", testSID, "wip"})
	if err != nil {
		t.Fatalf("tag remove: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "tag \"wip\" removed") {
		t.Errorf("expected removal confirmation; got: %s", output)
	}

	// List should show "no tags" again.
	output, err = executeSessionsCmd(t, dir, []string{"tag", "list", testSID})
	if err != nil {
		t.Fatalf("tag list after remove: %v", err)
	}
	if !strings.Contains(output, "no tags") {
		t.Errorf("expected 'no tags' after removal; got: %s", output)
	}
}

// TestCLI_SessionsTagSet verifies setting multiple tags at once.
func TestCLI_SessionsTagSet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	testSID := testSessionUUID
	seedTestSession(t, dir, testSID)

	// Set multiple tags.
	output, err := executeSessionsCmd(t, dir, []string{"tag", "set", testSID, "feature", "v2", "reviewed"})
	if err != nil {
		t.Fatalf("tag set: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "tags set on") {
		t.Errorf("expected set confirmation; got: %s", output)
	}

	// List to verify all three tags.
	output, err = executeSessionsCmd(t, dir, []string{"tag", "list", testSID})
	if err != nil {
		t.Fatalf("tag list after set: %v", err)
	}
	for _, tag := range []string{"feature", "v2", "reviewed"} {
		if !strings.Contains(output, tag) {
			t.Errorf("expected tag %q in list; got: %s", tag, output)
		}
	}
}
