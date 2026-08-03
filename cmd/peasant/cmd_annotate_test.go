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
	"github.com/peasant-labs/peasant/internal/testutil"
)

// executeAnnotateCmd is a test helper that runs the annotate cobra command under a
// test root with --data-dir=dir (parallel-safe; no t.Setenv), capturing combined
// stdout+stderr, and returns (output, error).
func executeAnnotateCmd(t *testing.T, dir string, args []string) (string, error) {
	t.Helper()
	return executeWithDataDir(t, BuildAnnotateCommand(), dir, args)
}

// seedTestSessionInto inserts a minimal session row into the store under the
// --data-dir-resolved path (dir/<AppName>/peasant.db), matching what a command
// run with --data-dir=dir reads. This is the parallel-safe analog of
// seedTestSession (which targets the env-resolved path): callers thread dir
// explicitly instead of mutating process-global XDG_DATA_HOME.
func seedTestSessionInto(t *testing.T, dir, sessionID string) {
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
				Hash:     testutil.TestProjectHash,
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

// TestAnnotateCmd_SubcommandsExist verifies that all subcommands are registered.
func TestAnnotateCmd_SubcommandsExist(t *testing.T) {
	t.Parallel()
	cmd := BuildAnnotateCommand()
	expected := []string{"list", "create", "list-project", "create-project", "prune"}
	for _, name := range expected {
		found := false
		for _, c := range cmd.Commands() {
			if c.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("subcommand %q not found; available: %v", name, subcommandNames(cmd))
		}
	}
}

// TestAnnotateCmd_ReactAlias verifies the "react" alias is registered.
func TestAnnotateCmd_ReactAlias(t *testing.T) {
	t.Parallel()
	cmd := BuildAnnotateCommand()
	if !containsString(cmd.Aliases, "react") {
		t.Errorf("expected alias 'react'; got aliases: %v", cmd.Aliases)
	}
}

// TestCLI_AnnotateListEmpty verifies that listing annotations for a session
// with no annotations shows "no annotations found".
func TestCLI_AnnotateListEmpty(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	testSID := testSessionUUID
	seedTestSessionInto(t, dataHome, testSID)

	output, err := executeAnnotateCmd(t, dataHome, []string{"list", testSID})
	if err != nil {
		t.Fatalf("annotate list: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "no annotations found") {
		t.Errorf("expected 'no annotations found'; got: %s", output)
	}
}

// TestCLI_AnnotateCreateAndList verifies creating a session annotation and listing it.
func TestCLI_AnnotateCreateAndList(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	testSID := testSessionUUID
	seedTestSessionInto(t, dataHome, testSID)

	// Create an annotation using a seeded type.
	output, err := executeAnnotateCmd(t, dataHome, []string{"create", testSID, testutil.TestTypeIDSessionApproval, "approve"})
	if err != nil {
		t.Fatalf("annotate create: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "created annotation") {
		t.Errorf("expected 'created annotation' confirmation; got: %s", output)
	}
	if !strings.Contains(output, testutil.TestTypeIDSessionApproval) {
		t.Errorf("expected type ID in output; got: %s", output)
	}

	// List to verify the annotation appears.
	output, err = executeAnnotateCmd(t, dataHome, []string{"list", testSID})
	if err != nil {
		t.Fatalf("annotate list after create: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, testutil.TestTypeIDSessionApproval) {
		t.Errorf("expected type ID in list output; got: %s", output)
	}
	if !strings.Contains(output, "approve") {
		t.Errorf("expected value 'approve' in list output; got: %s", output)
	}
	if !strings.Contains(output, humanCLIAnnotatorName) {
		t.Errorf("expected annotator name %q in list output; got: %s", humanCLIAnnotatorName, output)
	}
	if !strings.Contains(output, "1 annotation(s)") {
		t.Errorf("expected '1 annotation(s)' in list output; got: %s", output)
	}
}

// TestCLI_AnnotateCreateInvalidTypeID verifies that creating an annotation
// with a nonexistent type-id returns an error.
func TestCLI_AnnotateCreateInvalidTypeID(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	testSID := testSessionUUID
	seedTestSessionInto(t, dataHome, testSID)

	_, err := executeAnnotateCmd(t, dataHome, []string{"create", testSID, "nonexistent.type", "value"})
	if err == nil {
		t.Fatal("expected error for nonexistent type-id, got nil")
	}
	if !strings.Contains(err.Error(), "annotation type not found") {
		t.Errorf("expected 'annotation type not found' in error; got: %v", err)
	}
}

// TestCLI_AnnotateCreateInvalidValue verifies that creating an annotation
// with a value not in the type's permissible values returns an error listing allowed values.
func TestCLI_AnnotateCreateInvalidValue(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	testSID := testSessionUUID
	seedTestSessionInto(t, dataHome, testSID)

	// quality.session_approval only allows "approve" or "deny".
	_, err := executeAnnotateCmd(t, dataHome, []string{"create", testSID, testutil.TestTypeIDSessionApproval, "maybe"})
	if err == nil {
		t.Fatal("expected error for invalid value, got nil")
	}
	if !strings.Contains(err.Error(), "not in permissible values") {
		t.Errorf("expected 'not in permissible values' in error; got: %v", err)
	}
}

// TestCLI_AnnotateListProjectEmpty verifies listing project annotations with none present.
func TestCLI_AnnotateListProjectEmpty(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	// Seed a session so the DB is initialized.
	seedTestSessionInto(t, dataHome, testSessionUUID)

	output, err := executeAnnotateCmd(t, dataHome, []string{"list-project", string(testutil.TestProjectHash)})
	if err != nil {
		t.Fatalf("annotate list-project: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "no annotations found") {
		t.Errorf("expected 'no annotations found'; got: %s", output)
	}
}

// TestCLI_AnnotateCreateProjectAndListProject verifies creating a project annotation
// and listing it.
func TestCLI_AnnotateCreateProjectAndListProject(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	// Seed a session so the DB and project hash exist.
	seedTestSessionInto(t, dataHome, testSessionUUID)

	projectHash := string(testutil.TestProjectHash)

	// Create a project annotation.
	output, err := executeAnnotateCmd(t, dataHome, []string{"create-project", projectHash, testutil.TestTypeIDSessionApproval, "deny"})
	if err != nil {
		t.Fatalf("annotate create-project: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "created annotation") {
		t.Errorf("expected 'created annotation' confirmation; got: %s", output)
	}
	if !strings.Contains(output, "project "+projectHash) {
		t.Errorf("expected 'project %s' in output; got: %s", projectHash, output)
	}

	// List project annotations.
	output, err = executeAnnotateCmd(t, dataHome, []string{"list-project", projectHash})
	if err != nil {
		t.Fatalf("annotate list-project after create: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, testutil.TestTypeIDSessionApproval) {
		t.Errorf("expected type ID in list output; got: %s", output)
	}
	if !strings.Contains(output, "deny") {
		t.Errorf("expected value 'deny' in list output; got: %s", output)
	}
	if !strings.Contains(output, "1 annotation(s)") {
		t.Errorf("expected '1 annotation(s)' in list output; got: %s", output)
	}
}

// TestCLI_AnnotateCreateProjectInvalidValue verifies project create with invalid value.
func TestCLI_AnnotateCreateProjectInvalidValue(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	seedTestSessionInto(t, dataHome, testSessionUUID)

	_, err := executeAnnotateCmd(t, dataHome, []string{"create-project", string(testutil.TestProjectHash), testutil.TestTypeIDSessionApproval, "invalid-value"})
	if err == nil {
		t.Fatal("expected error for invalid value, got nil")
	}
	if !strings.Contains(err.Error(), "not in permissible values") {
		t.Errorf("expected 'not in permissible values' in error; got: %v", err)
	}
}
