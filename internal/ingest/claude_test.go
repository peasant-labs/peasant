package ingest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// --- Fixtures ---

// sampleClaudeJSONL builds a minimal two-line JSONL fixture for a root session.
func sampleClaudeJSONL(sessionID, version, cwd, branch string) []byte {
	lines := []string{
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"cwd":%q,"gitBranch":%q,"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2024-02-19T00:00:00Z","uuid":"uuid-1"}`,
			sessionID, version, cwd, branch),
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"hi"}]},"timestamp":"2024-02-19T00:01:00Z","uuid":"uuid-2"}`,
			sessionID, version),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// sampleClaudeJSONLWithToolCalls builds a JSONL fixture with one tool call.
func sampleClaudeJSONLWithToolCalls(sessionID, version, cwd, branch string) []byte {
	lines := []string{
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"cwd":%q,"gitBranch":%q,"type":"user","message":{"role":"user","content":"run bash"},"timestamp":"2024-02-19T00:00:00Z","uuid":"uuid-1"}`,
			sessionID, version, cwd, branch),
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"ls"}}]},"timestamp":"2024-02-19T00:02:00Z","uuid":"uuid-2"}`,
			sessionID, version),
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"user","message":{"role":"user","content":"thanks"},"timestamp":"2024-02-19T00:03:00Z","uuid":"uuid-3"}`,
			sessionID, version),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// setupClaudeFS populates a MemFS with a single root session under a project slug.
// Returns the path to the root session .jsonl file.
func setupClaudeFS(t *testing.T, mfs *testutil.MemFS, basePath, projectSlug, sessionID string, data []byte) string {
	t.Helper()
	path := fmt.Sprintf("%s/%s/%s.jsonl", basePath, projectSlug, sessionID)
	if err := mfs.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("setupClaudeFS: WriteFile: %v", err)
	}
	return path
}

// setupSubagentFS adds a subagent JSONL under {basePath}/{projectSlug}/{rootUUID}/subagents/agent-{hex}.jsonl.
func setupSubagentFS(t *testing.T, mfs *testutil.MemFS, basePath, projectSlug, rootUUID, subagentID string, data []byte) string {
	t.Helper()
	path := fmt.Sprintf("%s/%s/%s/subagents/%s.jsonl", basePath, projectSlug, rootUUID, subagentID)
	if err := mfs.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("setupSubagentFS: WriteFile: %v", err)
	}
	return path
}

// --- Tests ---

func TestClaudeAdapter_Provider(t *testing.T) {
	a := ingest.NewClaudeAdapter(testutil.NewMemFS(), testutil.DefaultGitResolver(), salt.Salt{})
	if got := a.Harness(); got != ingest.HarnessClaudeCode {
		t.Errorf("Harness() = %q, want %q", got, ingest.HarnessClaudeCode)
	}
}

func TestClaudeAdapter_Discover(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-myproject"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONL(sessionUUID, "2.1.14", "/home/test/myproject", "main")
	setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("Discover returned %d sessions, want 1", len(sessions))
	}

	s := sessions[0]
	if s.SessionID != ingest.SessionID(sessionUUID) {
		t.Errorf("SessionID = %q, want %q", s.SessionID, sessionUUID)
	}
	if s.Harness != ingest.HarnessClaudeCode {
		t.Errorf("Harness = %q, want %q", s.Harness, ingest.HarnessClaudeCode)
	}
	if s.SourceFormat != ingest.SourceFormatJSONL {
		t.Errorf("SourceFormat = %q, want %q", s.SourceFormat, ingest.SourceFormatJSONL)
	}
	if s.ParentUUID != nil {
		t.Errorf("ParentUUID = %v, want nil for root session", s.ParentUUID)
	}
	if len(s.SubagentPaths) != 0 {
		t.Errorf("SubagentPaths len = %d, want 0", len(s.SubagentPaths))
	}
	expectedPath := fmt.Sprintf("%s/%s/%s.jsonl", base, projectSlug, sessionUUID)
	if string(s.SourcePath) != expectedPath {
		t.Errorf("SourcePath = %q, want %q", s.SourcePath, expectedPath)
	}
}

func TestClaudeAdapter_Discover_EmptyDir(t *testing.T) {
	const base = "/home/test/.claude/projects"

	mfs := testutil.NewMemFS()
	// Create the base directory but add no files.
	if err := mfs.MkdirAll(base, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("Discover returned %d sessions, want 0", len(sessions))
	}
}

func TestClaudeAdapter_Discover_SkipsNonJSONL(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()

	// Write a .jsonl (valid).
	data := sampleClaudeJSONL(sessionUUID, "2.1.14", "/home/test/project", "main")
	setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	// Write non-JSONL files that should be ignored.
	nonJSONLPaths := []string{
		fmt.Sprintf("%s/%s/sessions-index.json", base, projectSlug),
		fmt.Sprintf("%s/%s/notes.txt", base, projectSlug),
		fmt.Sprintf("%s/%s/readme.md", base, projectSlug),
	}
	for _, p := range nonJSONLPaths {
		if err := mfs.WriteFile(p, []byte("ignore me"), 0644); err != nil {
			t.Fatalf("WriteFile %q: %v", p, err)
		}
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("Discover returned %d sessions, want 1 (only .jsonl)", len(sessions))
	}
	if sessions[0].SessionID != ingest.SessionID(sessionUUID) {
		t.Errorf("SessionID = %q, want %q", sessions[0].SessionID, sessionUUID)
	}
}

func TestClaudeAdapter_Discover_SubagentLinking(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		rootUUID    = testutil.TestSessionUUID
		subagentID  = testutil.TestSubagentID
	)

	mfs := testutil.NewMemFS()

	// Root session.
	rootData := sampleClaudeJSONL(rootUUID, "2.1.14", "/home/test/project", "main")
	setupClaudeFS(t, mfs, base, projectSlug, rootUUID, rootData)

	// Subagent session — use subagentID as the sessionId (not root's UUID).
	subData := sampleClaudeJSONL(subagentID, "2.1.14", "/home/test/project", "main")
	setupSubagentFS(t, mfs, base, projectSlug, rootUUID, subagentID, subData)

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Expect 2 sessions: root and subagent.
	if len(sessions) != 2 {
		t.Fatalf("Discover returned %d sessions, want 2 (root + subagent)", len(sessions))
	}

	// Find root and subagent entries.
	var root, sub *ingest.DiscoveredSession
	for i := range sessions {
		s := &sessions[i]
		if s.SessionID == ingest.SessionID(rootUUID) {
			root = s
		} else if s.SessionID == ingest.SessionID(subagentID) {
			sub = s
		}
	}

	if root == nil {
		t.Fatal("root session not found in results")
	}
	if sub == nil {
		t.Fatal("subagent session not found in results")
	}

	// Root should have the subagent in SubagentPaths.
	if len(root.SubagentPaths) != 1 {
		t.Errorf("root.SubagentPaths len = %d, want 1", len(root.SubagentPaths))
	}

	// Subagent should have ParentUUID pointing to root.
	if sub.ParentUUID == nil {
		t.Fatal("subagent ParentUUID = nil, want non-nil")
	}
	if *sub.ParentUUID != ingest.SessionID(rootUUID) {
		t.Errorf("subagent ParentUUID = %q, want %q", *sub.ParentUUID, rootUUID)
	}
	if sub.SourceFormat != ingest.SourceFormatJSONL {
		t.Errorf("subagent SourceFormat = %q, want %q", sub.SourceFormat, ingest.SourceFormatJSONL)
	}
}

func TestClaudeAdapter_Discover_MultipleProjects(t *testing.T) {
	const base = "/home/test/.claude/projects"

	mfs := testutil.NewMemFS()

	dataA := sampleClaudeJSONL(testutil.TestSessionUUID, "2.1.14", "/home/test/projectA", "main")
	setupClaudeFS(t, mfs, base, "-home-test-projectA", testutil.TestSessionUUID, dataA)

	// Use a second valid UUID for projectB.
	uuidB := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dataB := sampleClaudeJSONL(uuidB, "2.1.14", "/home/test/projectB", "dev")
	setupClaudeFS(t, mfs, base, "-home-test-projectB", uuidB, dataB)

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	}

	sessions, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("Discover returned %d sessions, want 2 (one per project)", len(sessions))
	}

	// Both session IDs should be present.
	found := make(map[ingest.SessionID]bool)
	for _, s := range sessions {
		found[s.SessionID] = true
	}
	if !found[ingest.SessionID(testutil.TestSessionUUID)] {
		t.Errorf("session %q not found", testutil.TestSessionUUID)
	}
	if !found[ingest.SessionID(uuidB)] {
		t.Errorf("session %q not found", uuidB)
	}
}

func TestClaudeAdapter_ExtractMetadata(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-myproject"
		sessionUUID = testutil.TestSessionUUID
		version     = "2.1.14"
		cwd         = "/home/test/myproject"
		branch      = "main"
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONL(sessionUUID, version, cwd, branch)
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	git := testutil.DefaultGitResolver()
	a := ingest.NewClaudeAdapter(mfs, git, salt.Salt{})

	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Schema version.
	if meta.SchemaVersion != ingest.CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", meta.SchemaVersion, ingest.CurrentSchemaVersion)
	}

	// Session identity.
	if meta.SessionID != sid {
		t.Errorf("SessionID = %q, want %q", meta.SessionID, sid)
	}
	if meta.ParentUUID != nil {
		t.Errorf("ParentUUID = %v, want nil for root session", meta.ParentUUID)
	}
	if meta.ModelHarness != ingest.HarnessClaudeCode {
		t.Errorf("ModelHarness = %q, want %q", meta.ModelHarness, ingest.HarnessClaudeCode)
	}

	// Version from JSONL.
	if meta.Version != version {
		t.Errorf("Version = %q, want %q", meta.Version, version)
	}

	// Model extracted from assistant message.
	if meta.Model != ingest.ModelID("claude-opus-4-6") {
		t.Errorf("Model = %q, want %q", meta.Model, "claude-opus-4-6")
	}

	// Timestamps: fixture uses 2024-02-19T00:00:00Z and 2024-02-19T00:01:00Z.
	wantStart := int64(1708300800000) // 2024-02-19T00:00:00Z in ms
	wantEnd := int64(1708300860000)   // 2024-02-19T00:01:00Z in ms
	if meta.Timestamp.Start != wantStart {
		t.Errorf("Timestamp.Start = %d, want %d", meta.Timestamp.Start, wantStart)
	}
	if meta.Timestamp.End != wantEnd {
		t.Errorf("Timestamp.End = %d, want %d", meta.Timestamp.End, wantEnd)
	}
	if meta.Timestamp.Ingested == nil || *meta.Timestamp.Ingested == 0 {
		t.Error("Timestamp.Ingested = 0, want non-zero")
	}

	// Duration: 1 minute = 60000 ms.
	if meta.Stats.DurationMs != 60000 {
		t.Errorf("Stats.DurationMs = %d, want 60000", meta.Stats.DurationMs)
	}

	// Turns: 2 lines (1 user + 1 assistant).
	if meta.Stats.TurnCount != 2 {
		t.Errorf("Stats.TurnCount = %d, want 2", meta.Stats.TurnCount)
	}

	// No tool calls in basic fixture.
	if meta.Stats.ToolCallCount != 0 {
		t.Errorf("Stats.ToolCallCount = %d, want 0", meta.Stats.ToolCallCount)
	}

	// Git info: branch comes from JSONL gitBranch field ("main") which matches.
	if meta.Git.Branch == nil || *meta.Git.Branch != branch {
		t.Errorf("Git.Branch = %v, want %q", meta.Git.Branch, branch)
	}
	if meta.Git.Remote == nil {
		t.Error("Git.Remote = nil, want non-nil")
	}

	// Tracking branch.
	if meta.Git.Tracking == nil {
		t.Error("Git.Tracking = nil, want non-nil")
	} else if *meta.Git.Tracking != "origin/main" {
		t.Errorf("Git.Tracking = %q, want %q", *meta.Git.Tracking, "origin/main")
	}

	// Source info.
	if meta.Source.FilePath != filePath {
		t.Errorf("Source.FilePath = %q, want %q", meta.Source.FilePath, filePath)
	}
	if meta.Source.Format != ingest.SourceFormatJSONL {
		t.Errorf("Source.Format = %q, want %q", meta.Source.Format, ingest.SourceFormatJSONL)
	}

	// Project hash must be a 64-char hex string.
	if len(string(meta.Project.Hash)) != 64 {
		t.Errorf("Project.Hash len = %d, want 64", len(string(meta.Project.Hash)))
	}

	// CWD stored from first JSONL line.
	if meta.CWD != cwd {
		t.Errorf("CWD = %q, want %q", meta.CWD, cwd)
	}

	// No diagnostics warnings.
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings = %v, want empty", meta.Diagnostics.Warnings)
	}
}

func TestClaudeAdapter_ExtractMetadata_ToolCallCount(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-proj"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONLWithToolCalls(sessionUUID, "2.1.14", "/home/test/proj", "main")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// 3 turns: user, assistant, user.
	if meta.Stats.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", meta.Stats.TurnCount)
	}
	// 1 tool call.
	if meta.Stats.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", meta.Stats.ToolCallCount)
	}
}

func TestClaudeAdapter_ExtractMetadata_NoGit(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONL(sessionUUID, "2.1.14", "/home/test/project", "feature-branch")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	// NoRemoteGitResolver: remote fails, branch+worktree succeed.
	a := ingest.NewClaudeAdapter(mfs, testutil.NoRemoteGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Remote should be nil since resolver errors.
	if meta.Git.Remote != nil {
		t.Errorf("Git.Remote = %v, want nil when remote not available", meta.Git.Remote)
	}

	// Branch should come from JSONL field (gitBranch = "feature-branch").
	if meta.Git.Branch == nil || *meta.Git.Branch != "feature-branch" {
		t.Errorf("Git.Branch = %v, want %q", meta.Git.Branch, "feature-branch")
	}

	// Project.Hash must still be populated (falls back to worktree/cwd-based hash).
	if len(string(meta.Project.Hash)) != 64 {
		t.Errorf("Project.Hash len = %d, want 64", len(string(meta.Project.Hash)))
	}
}

func TestClaudeAdapter_ExtractMetadata_NoGitAtAll(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONL(sessionUUID, "2.1.14", "/home/test/project", "main")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	// NoGitResolver: all git calls fail.
	a := ingest.NewClaudeAdapter(mfs, testutil.NoGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Remote should be nil.
	if meta.Git.Remote != nil {
		t.Errorf("Git.Remote = %v, want nil", meta.Git.Remote)
	}
	// Worktree should be nil.
	if meta.Git.Worktree != nil {
		t.Errorf("Git.Worktree = %v, want nil", meta.Git.Worktree)
	}

	// Branch: git resolver fails but gitBranch is set from JSONL ("main").
	if meta.Git.Branch == nil || *meta.Git.Branch != "main" {
		t.Errorf("Git.Branch = %v, want %q (from JSONL field)", meta.Git.Branch, "main")
	}

	// Project hash still present (derived from cwd).
	if len(string(meta.Project.Hash)) != 64 {
		t.Errorf("Project.Hash len = %d, want 64", len(string(meta.Project.Hash)))
	}

	// No errors / warnings expected from git failures (they are silent).
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", meta.Diagnostics.Warnings)
	}
}

func TestClaudeAdapter_ExtractMetadata_CorruptJSONL(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()

	// Mix of valid and corrupt lines.
	validLine := fmt.Sprintf(
		`{"sessionId":%q,"version":"2.1.14","cwd":"/home/test/project","gitBranch":"main","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2024-02-19T00:00:00Z","uuid":"uuid-1"}`,
		sessionUUID,
	)
	corruptLine := `{this is not valid json`
	anotherValidLine := fmt.Sprintf(
		`{"sessionId":%q,"version":"2.1.14","type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"hi"}]},"timestamp":"2024-02-19T00:01:00Z","uuid":"uuid-2"}`,
		sessionUUID,
	)

	data := []byte(validLine + "\n" + corruptLine + "\n" + anotherValidLine + "\n")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: unexpected error (should tolerate corrupt lines): %v", err)
	}

	// Should still parse partial data.
	if meta.SessionID != sid {
		t.Errorf("SessionID = %q, want %q", meta.SessionID, sid)
	}

	// Should have at least 1 warning for the corrupt line.
	if len(meta.Diagnostics.Warnings) == 0 {
		t.Error("Diagnostics.Warnings is empty, want at least 1 warning for corrupt line")
	}

	// The warning should have error type "parse_error".
	foundParseError := false
	for _, w := range meta.Diagnostics.Warnings {
		if w.ErrorType == "parse_error" {
			foundParseError = true
			break
		}
	}
	if !foundParseError {
		t.Errorf("no parse_error warning found in %v", meta.Diagnostics.Warnings)
	}

	// Valid lines still counted: 1 user + 1 assistant.
	if meta.Stats.TurnCount < 2 {
		t.Errorf("TurnCount = %d, want >= 2 (valid lines parsed)", meta.Stats.TurnCount)
	}
}

func TestClaudeAdapter_ExtractMetadata_SubagentRef(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		rootUUID    = testutil.TestSessionUUID
		subagentID  = testutil.TestSubagentID
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONL(rootUUID, "2.1.14", "/home/test/project", "main")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, rootUUID, data)

	subPath := fmt.Sprintf("%s/%s/%s/subagents/%s.jsonl", base, projectSlug, rootUUID, subagentID)
	// Use subagentID as the sessionId — in production each subagent has its own ID.
	subData := sampleClaudeJSONL(subagentID, "2.1.14", "/home/test/project", "main")
	if err := mfs.WriteFile(subPath, subData, 0644); err != nil {
		t.Fatalf("WriteFile subagent: %v", err)
	}

	sid, err := ingest.NewSessionID(rootUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{ingest.ResolvedPath(subPath)},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	if meta.Stats.SubagentCount != 1 {
		t.Errorf("Stats.SubagentCount = %d, want 1", meta.Stats.SubagentCount)
	}

	if len(meta.Subagents) != 1 {
		t.Fatalf("Subagents len = %d, want 1", len(meta.Subagents))
	}

	ref := meta.Subagents[0]
	if ref.SessionID != ingest.SessionID(subagentID) {
		t.Errorf("Subagents[0].SessionID = %q, want %q", ref.SessionID, subagentID)
	}
	if ref.ParentUUID != sid {
		t.Errorf("Subagents[0].ParentUUID = %q, want %q", ref.ParentUUID, sid)
	}
}

func TestClaudeAdapter_Discover_CancelledContext(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONL(sessionUUID, "2.1.14", "/home/test/project", "main")
	setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	cfg := ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	}

	// Create a pre-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.Discover(ctx, cfg)
	if err == nil {
		t.Fatal("Discover with cancelled context should return error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Discover error = %q, want containing %q", err.Error(), "context canceled")
	}
}

// sampleClaudeJSONLNoCWD builds a JSONL fixture with no CWD field in any line.
// This simulates older Claude sessions or sessions without a CWD field.
func sampleClaudeJSONLNoCWD(sessionID, version string) []byte {
	lines := []string{
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2024-02-19T00:00:00Z","uuid":"uuid-1"}`,
			sessionID, version),
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"hi"}]},"timestamp":"2024-02-19T00:01:00Z","uuid":"uuid-2"}`,
			sessionID, version),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// TestClaudeAdapter_ExtractMetadata_NoCWD_DecodesProjectDir verifies that when a
// JSONL session has no CWD field and the source file is under a Claude project
// directory, the adapter decodes the Claude project dir name back to the real
// filesystem path for git resolution.
func TestClaudeAdapter_ExtractMetadata_NoCWD_DecodesProjectDir(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-dev-myproject"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()

	// Create the real project directory so the greedy decoder can find it.
	// Path segments: home / test / dev / myproject
	if err := mfs.MkdirAll("/home/test/dev/myproject", 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Write a JSONL with NO CWD field.
	data := sampleClaudeJSONLNoCWD(sessionUUID, "2.1.14")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	git := testutil.DefaultGitResolver()
	a := ingest.NewClaudeAdapter(mfs, git, salt.Salt{})

	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// The decoded path should be /home/test/dev/myproject, which the
	// DefaultGitResolver will resolve to a valid remote. So HostSlug
	// should be derived from the git remote, not the fallback path.
	if meta.HostSlug == "" {
		t.Fatal("HostSlug is empty, want non-empty")
	}
	if strings.Contains(string(meta.HostSlug), "__peasant-untracked__") {
		t.Errorf("HostSlug = %q, should NOT contain __peasant-untracked__ when project dir decoding succeeds", meta.HostSlug)
	}

	// With the DefaultGitResolver, the host slug should be testuser--testrepo.
	wantSlug := testutil.TestHostSlug
	if string(meta.HostSlug) != wantSlug {
		t.Errorf("HostSlug = %q, want %q", meta.HostSlug, wantSlug)
	}

	// Project.FilePath should be the decoded real path (or worktree from git).
	// With DefaultGitResolver, worktree returns /home/test/testrepo.
	if meta.Project.FilePath == "" {
		t.Error("Project.FilePath is empty, want non-empty")
	}
}

// TestClaudeAdapter_ExtractMetadata_NoCWD_DashInDirName verifies that the greedy
// decoder correctly handles directory names containing literal dashes.
func TestClaudeAdapter_ExtractMetadata_NoCWD_DashInDirName(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-my-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()

	// Create the real project directory with a dash in its name.
	// The encoded form is: -home-test-my-project
	// The ambiguity: "my" + "project" could be /home/test/my/project or /home/test/my-project
	// The greedy decoder should try /home/test/my first, and if that doesn't exist,
	// merge "my" + "project" into "my-project".
	if err := mfs.MkdirAll("/home/test/my-project", 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	data := sampleClaudeJSONLNoCWD(sessionUUID, "2.1.14")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	git := testutil.DefaultGitResolver()
	a := ingest.NewClaudeAdapter(mfs, git, salt.Salt{})

	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Should decode to /home/test/my-project and get the git remote.
	if strings.Contains(string(meta.HostSlug), "__peasant-untracked__") {
		t.Errorf("HostSlug = %q, should NOT contain __peasant-untracked__ for dash-in-dirname case", meta.HostSlug)
	}

	wantSlug := testutil.TestHostSlug
	if string(meta.HostSlug) != wantSlug {
		t.Errorf("HostSlug = %q, want %q", meta.HostSlug, wantSlug)
	}
}

// TestClaudeAdapter_ExtractMetadata_NoCWD_DecodeFails verifies that when the
// project dir cannot be decoded (no matching filesystem path), the adapter
// falls back to the existing behavior (using the Claude project dir as cwd).
func TestClaudeAdapter_ExtractMetadata_NoCWD_DecodeFails(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-nonexistent-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()

	// Do NOT create the real project directory — decoding should fail.
	data := sampleClaudeJSONLNoCWD(sessionUUID, "2.1.14")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	// Use NoGitResolver to simulate no git at the Claude dir either.
	a := ingest.NewClaudeAdapter(mfs, testutil.NoGitResolver(), salt.Salt{})

	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Decode failed, git also fails → should get __peasant-untracked__ slug.
	if !strings.Contains(string(meta.HostSlug), "__peasant-untracked__") {
		t.Errorf("HostSlug = %q, want to contain __peasant-untracked__ when decode fails", meta.HostSlug)
	}
}

// TestClaudeAdapter_ExtractMetadata_NoCWD_NestedDashes verifies decoding when
// the project path has multiple directories with dashes in their names.
func TestClaudeAdapter_ExtractMetadata_NoCWD_NestedDashes(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-codebases-acme-dev-nix-openclaw-vm"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()

	// Create the real project directory: /home/test/codebases/acme-dev/nix-openclaw-vm
	// The encoded form: -home-test-codebases-acme-dev-nix-openclaw-vm
	// Ambiguity: nix/openclaw/vm vs nix-openclaw/vm vs nix-openclaw-vm
	// Greedy decoder tries: "nix" doesn't exist as dir under acme-dev,
	// so it tries "nix-openclaw" (doesn't exist), then "nix-openclaw-vm" (exists!).
	if err := mfs.MkdirAll("/home/test/codebases/acme-dev/nix-openclaw-vm", 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	data := sampleClaudeJSONLNoCWD(sessionUUID, "2.1.14")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	git := testutil.DefaultGitResolver()
	a := ingest.NewClaudeAdapter(mfs, git, salt.Salt{})

	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Should decode to /home/test/codebases/acme-dev/nix-openclaw-vm and get the git remote.
	if strings.Contains(string(meta.HostSlug), "__peasant-untracked__") {
		t.Errorf("HostSlug = %q, should NOT contain __peasant-untracked__", meta.HostSlug)
	}

	wantSlug := testutil.TestHostSlug
	if string(meta.HostSlug) != wantSlug {
		t.Errorf("HostSlug = %q, want %q", meta.HostSlug, wantSlug)
	}
}

// TestClaudeAdapter_ExtractMetadata_WithCWD_SkipsDecode verifies that when CWD
// is present in the JSONL, the decode logic is NOT triggered (CWD takes precedence).
func TestClaudeAdapter_ExtractMetadata_WithCWD_SkipsDecode(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-dev-myproject"
		sessionUUID = testutil.TestSessionUUID
		cwd         = "/home/test/dev/myproject"
		branch      = "main"
	)

	mfs := testutil.NewMemFS()

	// Note: we do NOT create the real project directory. If decode were
	// triggered incorrectly, it would fail and cwd would remain the
	// original. But cwd comes from JSONL, which is NOT under .claude/projects/,
	// so decoding should never run.
	data := sampleClaudeJSONL(sessionUUID, "2.1.14", cwd, branch)
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	git := testutil.DefaultGitResolver()
	a := ingest.NewClaudeAdapter(mfs, git, salt.Salt{})

	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// CWD is set and is a real path, so git resolution should work normally.
	wantSlug := testutil.TestHostSlug
	if string(meta.HostSlug) != wantSlug {
		t.Errorf("HostSlug = %q, want %q", meta.HostSlug, wantSlug)
	}
}

// sampleClaudeJSONLWithUsage builds a JSONL fixture with usage data in assistant messages.
// Two assistant messages with usage fields to verify accumulation across multiple messages.
func sampleClaudeJSONLWithUsage(sessionID, version, cwd, branch string) []byte {
	lines := []string{
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"cwd":%q,"gitBranch":%q,"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2024-02-19T00:00:00Z","uuid":"uuid-1"}`,
			sessionID, version, cwd, branch),
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":50}},"timestamp":"2024-02-19T00:01:00Z","uuid":"uuid-2"}`,
			sessionID, version),
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"user","message":{"role":"user","content":"more"},"timestamp":"2024-02-19T00:02:00Z","uuid":"uuid-3"}`,
			sessionID, version),
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"sure"}],"usage":{"input_tokens":200,"output_tokens":150}},"timestamp":"2024-02-19T00:03:00Z","uuid":"uuid-4"}`,
			sessionID, version),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestClaudeAdapter_ExtractMetadata_TokenCounts(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONLWithUsage(sessionUUID, "2.1.14", "/home/test/project", "main")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Two assistant messages: (100+200) input, (50+150) output.
	if meta.Stats.TokensIn != 300 {
		t.Errorf("Stats.TokensIn = %d, want 300", meta.Stats.TokensIn)
	}
	if meta.Stats.TokensOut != 200 {
		t.Errorf("Stats.TokensOut = %d, want 200", meta.Stats.TokensOut)
	}

	// Turns: 2 user + 2 assistant = 4.
	if meta.Stats.TurnCount != 4 {
		t.Errorf("Stats.TurnCount = %d, want 4", meta.Stats.TurnCount)
	}

	// No warnings.
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings = %v, want empty", meta.Diagnostics.Warnings)
	}
}

// sampleClaudeJSONLWithZeroUsage builds a JSONL fixture where usage fields are present
// but have zero values. Distinct from NoUsageField (field absent) — verifies that
// explicit zero tokens are recorded correctly.
func sampleClaudeJSONLWithZeroUsage(sessionID, version, cwd, branch string) []byte {
	lines := []string{
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"cwd":%q,"gitBranch":%q,"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2024-02-19T00:00:00Z","uuid":"uuid-1"}`,
			sessionID, version, cwd, branch),
		fmt.Sprintf(`{"sessionId":%q,"version":%q,"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":0,"output_tokens":0}},"timestamp":"2024-02-19T00:01:00Z","uuid":"uuid-2"}`,
			sessionID, version),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestClaudeAdapter_ExtractMetadata_ZeroUsage(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONLWithZeroUsage(sessionUUID, "2.1.14", "/home/test/project", "main")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Zero usage present → tokens should be 0.
	if meta.Stats.TokensIn != 0 {
		t.Errorf("Stats.TokensIn = %d, want 0", meta.Stats.TokensIn)
	}
	if meta.Stats.TokensOut != 0 {
		t.Errorf("Stats.TokensOut = %d, want 0", meta.Stats.TokensOut)
	}

	// No warnings — zero usage is valid, not an error.
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings = %v, want empty", meta.Diagnostics.Warnings)
	}
}

func TestClaudeAdapter_ExtractMetadata_NoUsageField(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	mfs := testutil.NewMemFS()
	// Use the original fixture which has no usage field in messages.
	data := sampleClaudeJSONL(sessionUUID, "2.1.14", "/home/test/project", "main")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// No usage fields → tokens should be zero.
	if meta.Stats.TokensIn != 0 {
		t.Errorf("Stats.TokensIn = %d, want 0", meta.Stats.TokensIn)
	}
	if meta.Stats.TokensOut != 0 {
		t.Errorf("Stats.TokensOut = %d, want 0", meta.Stats.TokensOut)
	}

	// No warnings.
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings = %v, want empty", meta.Diagnostics.Warnings)
	}
}

func TestClaudeAdapter_ExtractMetadata_IngestedTimestamp(t *testing.T) {
	const (
		base        = "/home/test/.claude/projects"
		projectSlug = "-home-test-project"
		sessionUUID = testutil.TestSessionUUID
	)

	before := time.Now().UnixMilli()

	mfs := testutil.NewMemFS()
	data := sampleClaudeJSONL(sessionUUID, "2.1.14", "/home/test/project", "main")
	filePath := setupClaudeFS(t, mfs, base, projectSlug, sessionUUID, data)

	sid, err := ingest.NewSessionID(sessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessClaudeCode,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	after := time.Now().UnixMilli()

	if meta.Timestamp.Ingested == nil || *meta.Timestamp.Ingested < before || *meta.Timestamp.Ingested > after {
		t.Errorf("Timestamp.Ingested = %v, want in range [%d, %d]",
			meta.Timestamp.Ingested, before, after)
	}
}
