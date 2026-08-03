package ingest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// ── Fixture helpers ───────────────────────────────────────────────────────────

func openCodeProjectJSON(id, worktree string) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"worktree":%q,"vcs":"git","time":{"created":1770000000000,"updated":1770000100000}}`,
		id, worktree,
	))
}

func openCodeSessionJSON(id, projectID, directory, parentID string, created, updated int64) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"slug":"test","version":"1.1.53","projectID":%q,"directory":%q,"parentID":%q,"time":{"created":%d,"updated":%d}}`,
		id, projectID, directory, parentID, created, updated,
	))
}

func openCodeMessageJSON(id, sessionID, role, modelID string, created int64) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"sessionID":%q,"role":%q,"modelID":%q,"time":{"created":%d}}`,
		id, sessionID, role, modelID, created,
	))
}

func openCodeMessageWithTokensJSON(id, sessionID, role, modelID string, created int64, tokensIn, tokensOut int) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%q,"sessionID":%q,"role":%q,"modelID":%q,"time":{"created":%d},"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		id, sessionID, role, modelID, created, tokensIn, tokensOut,
	))
}

// populateOpenCodeStorage writes a minimal OpenCode storage layout into a MemFS.
//
//	root/storage/project/{projHash}.json
//	root/storage/session/{projHash}/ses_{sesID}.json
//	root/storage/message/ses_{sesID}/msg_{msgID}.json  (repeated for each message)
func populateOpenCodeStorage(
	t *testing.T,
	fs *testutil.MemFS,
	root string,
	projHash string,
	projWorktree string,
	sesID string,
	directory string,
	parentID string,
	created int64,
	updated int64,
	messages []struct{ id, role, model string },
) {
	t.Helper()

	// project JSON
	projectPath := root + "/storage/project/" + projHash + ".json"
	if err := fs.WriteFile(projectPath, openCodeProjectJSON(projHash, projWorktree), 0644); err != nil {
		t.Fatalf("write project JSON: %v", err)
	}

	// session JSON
	sesPath := root + "/storage/session/" + projHash + "/" + sesID + ".json"
	if err := fs.WriteFile(sesPath, openCodeSessionJSON(sesID, projHash, directory, parentID, created, updated), 0644); err != nil {
		t.Fatalf("write session JSON: %v", err)
	}
	// set a known mod time so tests can assert it
	fs.ModTimes[sesPath] = time.Unix(updated/1000, 0)

	// message JSON files
	for _, msg := range messages {
		msgPath := root + "/storage/message/" + sesID + "/" + msg.id + ".json"
		if err := fs.WriteFile(msgPath, openCodeMessageJSON(msg.id, sesID, msg.role, msg.model, created), 0644); err != nil {
			t.Fatalf("write message JSON: %v", err)
		}
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestOpenCodeAdapter_Provider(t *testing.T) {
	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})

	got := a.Harness()
	if got != ingest.HarnessOpenCode {
		t.Errorf("Harness() = %q, want %q", got, ingest.HarnessOpenCode)
	}
}

func TestOpenCodeAdapter_Discover(t *testing.T) {
	const (
		root      = "/opencode"
		projHash  = "abc123hash"
		sesID     = "ses_3cd91f52effeXd3QAJ54jOyzv5"
		directory = "/home/test/project"
	)

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", 1770000000000, 1770000100000, nil)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	got := sessions[0]

	if got.SessionID != ingest.SessionID(sesID) {
		t.Errorf("SessionID = %q, want %q", got.SessionID, sesID)
	}
	if got.Harness != ingest.HarnessOpenCode {
		t.Errorf("Harness = %q, want %q", got.Harness, ingest.HarnessOpenCode)
	}
	if got.SourceFormat != ingest.SourceFormatJSON {
		t.Errorf("SourceFormat = %q, want %q", got.SourceFormat, ingest.SourceFormatJSON)
	}
	if got.ParentUUID != nil {
		t.Errorf("ParentUUID = %v, want nil", got.ParentUUID)
	}
	expectedPath := root + "/storage/session/" + projHash + "/" + sesID + ".json"
	if got.SourcePath.String() != expectedPath {
		t.Errorf("SourcePath = %q, want %q", got.SourcePath, expectedPath)
	}
	if got.OriginalRoot != ingest.ResolvedPath(root) {
		t.Errorf("OriginalRoot = %q, want %q", got.OriginalRoot, root)
	}
}

func TestOpenCodeAdapter_Discover_EmptyStorage(t *testing.T) {
	const root = "/empty-storage"

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})

	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() on empty storage returned unexpected error: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("Discover() returned %d sessions, want 0", len(sessions))
	}
}

func TestOpenCodeAdapter_Discover_SubagentLinking(t *testing.T) {
	const (
		root        = "/opencode"
		projHash    = "projhash1"
		parentSesID = "ses_parentABC123"
		childSesID  = "ses_childXYZ456"
		directory   = "/home/test/project"
	)

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Write parent session (no parentID).
	populateOpenCodeStorage(t, fs, root, projHash, directory, parentSesID, directory, "", 1770000000000, 1770000050000, nil)

	// Write child session (parentID = parentSesID).
	populateOpenCodeStorage(t, fs, root, projHash, directory, childSesID, directory, parentSesID, 1770000050000, 1770000100000, nil)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("Discover() returned %d sessions, want 2", len(sessions))
	}

	// Find child session and verify ParentUUID.
	var child *ingest.DiscoveredSession
	for i := range sessions {
		if sessions[i].SessionID == ingest.SessionID(childSesID) {
			child = &sessions[i]
			break
		}
	}
	if child == nil {
		t.Fatalf("child session %q not found in discovered sessions", childSesID)
	}
	if child.ParentUUID == nil {
		t.Fatalf("child session ParentUUID = nil, want %q", parentSesID)
	}
	if *child.ParentUUID != ingest.SessionID(parentSesID) {
		t.Errorf("child ParentUUID = %q, want %q", *child.ParentUUID, parentSesID)
	}

	// Parent session must have nil ParentUUID.
	var parent *ingest.DiscoveredSession
	for i := range sessions {
		if sessions[i].SessionID == ingest.SessionID(parentSesID) {
			parent = &sessions[i]
			break
		}
	}
	if parent == nil {
		t.Fatalf("parent session %q not found", parentSesID)
	}
	if parent.ParentUUID != nil {
		t.Errorf("parent ParentUUID = %v, want nil", parent.ParentUUID)
	}
}

func TestOpenCodeAdapter_ExtractMetadata(t *testing.T) {
	const (
		root      = "/opencode"
		projHash  = "projhash1"
		sesID     = "ses_3cd91f52effeXd3QAJ54jOyzv5"
		directory = "/home/test/project"
		modelName = "claude-opus-4-5"
		created   = int64(1770000000000)
		updated   = int64(1770000100000)
	)

	messages := []struct{ id, role, model string }{
		{"msg_001", "user", ""},
		{"msg_002", "assistant", modelName},
	}

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", created, updated, messages)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() error = %v", err)
	}

	// SessionID
	if md.SessionID != ingest.SessionID(sesID) {
		t.Errorf("SessionID = %q, want %q", md.SessionID, sesID)
	}
	// ModelHarness
	if md.ModelHarness != ingest.HarnessOpenCode {
		t.Errorf("ModelHarness = %q, want %q", md.ModelHarness, ingest.HarnessOpenCode)
	}
	// Model
	if md.Model != ingest.ModelID(modelName) {
		t.Errorf("Model = %q, want %q", md.Model, modelName)
	}
	// Version
	if md.Version != "1.1.53" {
		t.Errorf("Version = %q, want %q", md.Version, "1.1.53")
	}
	// ParentUUID
	if md.ParentUUID != nil {
		t.Errorf("ParentUUID = %v, want nil", md.ParentUUID)
	}
	// Timestamps
	if md.Timestamp.Start != created {
		t.Errorf("Timestamp.Start = %d, want %d", md.Timestamp.Start, created)
	}
	if md.Timestamp.End != updated {
		t.Errorf("Timestamp.End = %d, want %d", md.Timestamp.End, updated)
	}
	if md.Timestamp.Ingested == nil || *md.Timestamp.Ingested == 0 {
		t.Error("Timestamp.Ingested must be non-zero")
	}
	// Source
	expectedSesPath := root + "/storage/session/" + projHash + "/" + sesID + ".json"
	if md.Source.FilePath != expectedSesPath {
		t.Errorf("Source.FilePath = %q, want %q", md.Source.FilePath, expectedSesPath)
	}
	if md.Source.Format != ingest.SourceFormatJSON {
		t.Errorf("Source.Format = %q, want %q", md.Source.Format, ingest.SourceFormatJSON)
	}
	// Git fields non-nil (default git resolver succeeds)
	if md.Git.Branch == nil {
		t.Error("Git.Branch = nil, want non-nil")
	}
	if md.Git.Remote == nil {
		t.Error("Git.Remote = nil, want non-nil")
	}
	// Tracking branch.
	if md.Git.Tracking == nil {
		t.Error("Git.Tracking = nil, want non-nil")
	} else if *md.Git.Tracking != "origin/main" {
		t.Errorf("Git.Tracking = %q, want %q", *md.Git.Tracking, "origin/main")
	}
	// Project hash non-empty
	if md.Project.Hash == "" {
		t.Error("Project.Hash must not be empty")
	}
	// CWD stored from session directory.
	if md.CWD != directory {
		t.Errorf("CWD = %q, want %q", md.CWD, directory)
	}
	// Schema version
	if md.SchemaVersion != ingest.CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", md.SchemaVersion, ingest.CurrentSchemaVersion)
	}
	// Diagnostics initialized
	if md.Diagnostics.Warnings == nil {
		t.Error("Diagnostics.Warnings must be non-nil (empty slice)")
	}
}

func TestOpenCodeAdapter_ExtractMetadata_NoGit(t *testing.T) {
	const (
		root      = "/opencode"
		projHash  = "projhashNoGit"
		sesID     = "ses_nogitABCXYZ123"
		directory = "/home/test/project"
		created   = int64(1770000000000)
		updated   = int64(1770000100000)
	)

	fs := testutil.NewMemFS()
	git := testutil.NoGitResolver()

	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", created, updated, nil)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() with no git should succeed, got error: %v", err)
	}

	// Git fields must all be nil when git is unavailable.
	if md.Git.Branch != nil {
		t.Errorf("Git.Branch = %q, want nil", *md.Git.Branch)
	}
	if md.Git.Remote != nil {
		t.Errorf("Git.Remote = %q, want nil", *md.Git.Remote)
	}
	if md.Git.Worktree != nil {
		t.Errorf("Git.Worktree = %q, want nil", *md.Git.Worktree)
	}

	// Project hash must still be set (derived from directory path).
	if md.Project.Hash == "" {
		t.Error("Project.Hash must not be empty even without git")
	}
	// SessionID must still be correct.
	if md.SessionID != ingest.SessionID(sesID) {
		t.Errorf("SessionID = %q, want %q", md.SessionID, sesID)
	}
}

func TestOpenCodeAdapter_ExtractMetadata_CountsMessages(t *testing.T) {
	const (
		root      = "/opencode"
		projHash  = "projhashCounts"
		sesID     = "ses_countsABC123XYZ"
		directory = "/home/test/project"
		created   = int64(1770000000000)
		updated   = int64(1770000100000)
	)

	// 3 user messages + 3 assistant messages = 6 total turns
	messages := []struct{ id, role, model string }{
		{"msg_u1", "user", ""},
		{"msg_a1", "assistant", "claude-opus-4-5"},
		{"msg_u2", "user", ""},
		{"msg_a2", "assistant", "claude-opus-4-5"},
		{"msg_u3", "user", ""},
		{"msg_a3", "assistant", "claude-opus-4-5"},
	}

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", created, updated, messages)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() error = %v", err)
	}

	const wantTurns = 6
	if md.Stats.TurnCount != wantTurns {
		t.Errorf("TurnCount = %d, want %d", md.Stats.TurnCount, wantTurns)
	}
	// No parts files written → tool call count should be 0.
	if md.Stats.ToolCallCount != 0 {
		t.Errorf("ToolCallCount = %d, want 0 (no parts files)", md.Stats.ToolCallCount)
	}
}

// TestOpenCodeAdapter_ExtractMetadata_MessageDirNamingContract verifies that
// countMessages uses the raw session ID string (from the JSON "id" field) as
// the message directory name verbatim, with no prefix added or stripped.
//
// On-disk layout: {root}/message/{sessionID}/msg_{id}.json where {sessionID}
// is the full "id" value from the session JSON — e.g. "ses_3cd91f52effeXd3QAJ54jOyzv5".
// The "ses_" prefix is part of the session ID value itself; countMessages must
// not prepend an additional "ses_" prefix.
//
// This test uses a session ID that already contains a distinguishing infix to
// make the directory name easy to verify: if the code were to add a second
// "ses_" prefix the messages directory would not be found, and TurnCount would
// be 0 instead of the expected 2.
func TestOpenCodeAdapter_ExtractMetadata_MissingModel(t *testing.T) {
	const (
		root      = "/opencode"
		projHash  = "projhashNoModel"
		sesID     = "ses_noModelABC123XYZ"
		directory = "/home/test/project"
		created   = int64(1770000000000)
		updated   = int64(1770000100000)
	)

	// Messages with no modelID on the assistant message.
	messages := []struct{ id, role, model string }{
		{"msg_u1", "user", ""},
		{"msg_a1", "assistant", ""}, // no model ID
	}

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", created, updated, messages)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() error = %v", err)
	}

	// Model should be empty.
	if md.Model != "" {
		t.Errorf("Model = %q, want empty", md.Model)
	}

	// Diagnostics should contain a missing_model warning.
	foundMissingModel := false
	for _, w := range md.Diagnostics.Warnings {
		if w.ErrorType == "missing_model" {
			foundMissingModel = true
			break
		}
	}
	if !foundMissingModel {
		t.Errorf("expected missing_model warning in diagnostics, got %v", md.Diagnostics.Warnings)
	}
}

func TestOpenCodeAdapter_ExtractMetadata_MessageDirNamingContract(t *testing.T) {
	const (
		root     = "/opencode"
		projHash = "projhashNaming"
		// Full session ID including the "ses_" prefix — this is the exact value
		// stored in the JSON "id" field and used as the message directory name.
		sesID     = "ses_namingContract777XYZ"
		directory = "/home/test/project"
		created   = int64(1770000000000)
		updated   = int64(1770000100000)
	)

	messages := []struct{ id, role, model string }{
		{"msg_n1", "user", ""},
		{"msg_n2", "assistant", "claude-opus-4-5"},
	}

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// populateOpenCodeStorage writes message files under {root}/message/{sesID}/
	// using the sesID verbatim. The session JSON "id" field is also set to sesID.
	// countMessages must look up {root}/message/ses_namingContract777XYZ/ —
	// not {root}/message/ses_ses_namingContract777XYZ/ (double prefix).
	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", created, updated, messages)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() error = %v", err)
	}

	// If countMessages incorrectly prepends an extra "ses_" prefix the directory
	// lookup will fail and TurnCount will be 0 instead of 2.
	const wantTurns = 2
	if md.Stats.TurnCount != wantTurns {
		t.Errorf("TurnCount = %d, want %d — message dir lookup may have mangled the session ID (double prefix?)", md.Stats.TurnCount, wantTurns)
	}
}

func TestOpenCodeAdapter_ExtractMetadata_EmptyDirectory(t *testing.T) {
	const (
		root     = "/opencode"
		projHash = "projhashEmptyDir"
		sesID    = "ses_emptyDirABC123"
		created  = int64(1770000000000)
		updated  = int64(1770000100000)
	)

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Populate with empty directory field — this triggers the fallback path.
	populateOpenCodeStorage(t, fs, root, projHash, "/some/worktree", sesID,
		"", // empty directory
		"", created, updated, nil)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() with empty directory should not error, got: %v", err)
	}

	// SessionID must still be correct.
	if md.SessionID != ingest.SessionID(sesID) {
		t.Errorf("SessionID = %q, want %q", md.SessionID, sesID)
	}
	// Project hash must be populated (derived from storage root fallback).
	if md.Project.Hash == "" {
		t.Error("Project.Hash must not be empty even with empty directory")
	}
	// HostSlug must not be empty.
	if md.HostSlug == "" {
		t.Error("HostSlug must not be empty even with empty directory")
	}
}

func TestOpenCodeAdapter_ExtractMetadata_CountToolCallParts(t *testing.T) {
	const (
		root      = "/opencode"
		projHash  = "projhashParts"
		sesID     = "ses_partsABC123"
		directory = "/home/test/project"
		created   = int64(1770000000000)
		updated   = int64(1770000100000)
	)

	// Two messages: user + assistant. The assistant message has 3 tool call part files.
	messages := []struct{ id, role, model string }{
		{"msg_u1", "user", ""},
		{"msg_a1", "assistant", "claude-opus-4-5"},
	}

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", created, updated, messages)

	// Write 3 tool call part files under {root}/storage/part/{msgID}/
	partDir := root + "/storage/part/msg_a1"
	for _, name := range []string{"part_001.json", "part_002.json", "part_003.json"} {
		if err := fs.WriteFile(partDir+"/"+name, []byte(`{"type":"tool_use"}`), 0644); err != nil {
			t.Fatalf("write part file %s: %v", name, err)
		}
	}
	// Also write a non-JSON file that should be ignored.
	if err := fs.WriteFile(partDir+"/readme.txt", []byte("not a part"), 0644); err != nil {
		t.Fatalf("write non-json file: %v", err)
	}

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() error = %v", err)
	}

	// 3 .json part files → ToolCallCount should be 3 (the .txt file is ignored).
	const wantToolCalls = 3
	if md.Stats.ToolCallCount != wantToolCalls {
		t.Errorf("ToolCallCount = %d, want %d", md.Stats.ToolCallCount, wantToolCalls)
	}

	// Verify turn count is also correct (2 messages = 2 turns).
	const wantTurns = 2
	if md.Stats.TurnCount != wantTurns {
		t.Errorf("TurnCount = %d, want %d", md.Stats.TurnCount, wantTurns)
	}
}

func TestOpenCodeAdapter_ExtractMetadata_TokenCounts(t *testing.T) {
	const (
		root      = "/opencode"
		projHash  = "projhashTokens"
		sesID     = "ses_tokensABC123XYZ"
		directory = "/home/test/project"
		modelName = "claude-opus-4-5"
		created   = int64(1770000000000)
		updated   = int64(1770000100000)
	)

	// Populate session + project via the standard helper (no messages yet).
	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", created, updated, nil)

	// Write message files with token data manually — assistant messages have tokens, user does not.
	writeMsg := func(id, role, model string, withTokens bool, tokIn, tokOut int) {
		msgPath := root + "/storage/message/" + sesID + "/" + id + ".json"
		var data []byte
		if withTokens {
			data = openCodeMessageWithTokensJSON(id, sesID, role, model, created, tokIn, tokOut)
		} else {
			data = openCodeMessageJSON(id, sesID, role, model, created)
		}
		if err := fs.WriteFile(msgPath, data, 0644); err != nil {
			t.Fatalf("write message %s: %v", id, err)
		}
	}

	// user message (no tokens)
	writeMsg("msg_u1", "user", "", false, 0, 0)
	// assistant message with 150 input, 80 output
	writeMsg("msg_a1", "assistant", modelName, true, 150, 80)
	// user message (no tokens)
	writeMsg("msg_u2", "user", "", false, 0, 0)
	// assistant message with 200 input, 120 output
	writeMsg("msg_a2", "assistant", modelName, true, 200, 120)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() error = %v", err)
	}

	// Token totals: 150+200=350 in, 80+120=200 out
	if md.Stats.TokensIn != 350 {
		t.Errorf("Stats.TokensIn = %d, want 350", md.Stats.TokensIn)
	}
	if md.Stats.TokensOut != 200 {
		t.Errorf("Stats.TokensOut = %d, want 200", md.Stats.TokensOut)
	}

	// Turn count should still be correct: 4 messages (2 user + 2 assistant).
	if md.Stats.TurnCount != 4 {
		t.Errorf("Stats.TurnCount = %d, want 4", md.Stats.TurnCount)
	}
}

func TestOpenCodeAdapter_ExtractMetadata_NoTokenField(t *testing.T) {
	const (
		root      = "/opencode"
		projHash  = "projhashNoTokens"
		sesID     = "ses_noTokensABC123"
		directory = "/home/test/project"
		modelName = "claude-opus-4-5"
		created   = int64(1770000000000)
		updated   = int64(1770000100000)
	)

	// Messages without any tokens field — the standard fixture helper omits it.
	messages := []struct{ id, role, model string }{
		{"msg_u1", "user", ""},
		{"msg_a1", "assistant", modelName},
		{"msg_u2", "user", ""},
		{"msg_a2", "assistant", modelName},
	}

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()
	populateOpenCodeStorage(t, fs, root, projHash, directory, sesID, directory, "", created, updated, messages)

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover() returned %d sessions, want 1", len(sessions))
	}

	md, err := a.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata() error = %v", err)
	}

	// No tokens field → both should be 0, no error.
	if md.Stats.TokensIn != 0 {
		t.Errorf("Stats.TokensIn = %d, want 0", md.Stats.TokensIn)
	}
	if md.Stats.TokensOut != 0 {
		t.Errorf("Stats.TokensOut = %d, want 0", md.Stats.TokensOut)
	}
}

func TestOpenCodeAdapter_Discover_CorruptSessionJSON(t *testing.T) {
	const root = "/opencode"

	fs := testutil.NewMemFS()
	git := testutil.DefaultGitResolver()

	// Write a corrupt session JSON file.
	corruptPath := root + "/storage/session/somehash/ses_corrupt123.json"
	if err := fs.WriteFile(corruptPath, []byte(`{this is not valid json`), 0644); err != nil {
		t.Fatalf("WriteFile corrupt JSON: %v", err)
	}

	a := ingest.NewOpenCodeAdapter(fs, git, salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(root)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover() should skip corrupt JSON without error, got: %v", err)
	}
	// Corrupt session should be skipped, not returned.
	if len(sessions) != 0 {
		t.Errorf("Discover() returned %d sessions, want 0 (corrupt JSON skipped)", len(sessions))
	}
}
