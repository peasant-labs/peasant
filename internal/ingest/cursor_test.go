package ingest_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

const testCursorProjectsRoot = "/home/test/.cursor/projects"

func sampleCursorJSONL() []byte {
	lines := []string{
		`{"role":"user","timestamp":"2026-06-04T10:00:00Z","message":{"content":[{"type":"text","text":"Hello"}]}}`,
		`{"role":"assistant","timestamp":"2026-06-04T10:00:05Z","message":{"model":"cursor-agent","content":[{"type":"text","text":"Hello! How may I help?."},{"type":"tool_use","id":"tool-1","name":"ReadFile","input":{"path":"/tmp/example"}}],"usage":{"input_tokens":10,"output_tokens":20}}}`,
		`{"role":"user","timestamp":"2026-06-04T10:01:00Z","message":{"content":[{"type":"text","text":"Thanks"}]}}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func setupCursorRootFS(t *testing.T, mfs *testutil.MemFS, basePath, workspace, sessionID string, data []byte) string {
	t.Helper()
	path := fmt.Sprintf("%s/%s/agent-transcripts/%s/%s.jsonl", basePath, workspace, sessionID, sessionID)
	if err := mfs.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("setupCursorRootFS: WriteFile: %v", err)
	}
	return path
}

func setupCursorSubagentFS(t *testing.T, mfs *testutil.MemFS, basePath, workspace, rootID, subID string, data []byte) string {
	t.Helper()
	path := fmt.Sprintf("%s/%s/agent-transcripts/%s/subagents/%s.jsonl", basePath, workspace, rootID, subID)
	if err := mfs.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("setupCursorSubagentFS: WriteFile: %v", err)
	}
	return path
}

func TestCursorAdapter_Discover_NestedLayoutAndSubagents(t *testing.T) {
	const (
		base      = testCursorProjectsRoot
		workspace = "home-test-testrepo"
		rootID    = testutil.TestSessionUUID
		subID     = testutil.TestSessionUUID2
	)
	mfs := testutil.NewMemFS()
	if err := mfs.MkdirAll("/home/test/testrepo", 0755); err != nil {
		t.Fatalf("MkdirAll project: %v", err)
	}
	rootPath := setupCursorRootFS(t, mfs, base, workspace, rootID, sampleCursorJSONL())
	subPath := setupCursorSubagentFS(t, mfs, base, workspace, rootID, subID, sampleCursorJSONL())

	adapter := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("Discover returned %d sessions, want 2", len(sessions))
	}

	var root, sub *ingest.DiscoveredSession
	for i := range sessions {
		switch sessions[i].SessionID {
		case ingest.SessionID(rootID):
			root = &sessions[i]
		case ingest.SessionID(subID):
			sub = &sessions[i]
		}
	}
	if root == nil {
		t.Fatal("root session not discovered")
	}
	if sub == nil {
		t.Fatal("subagent session not discovered")
	}
	if root.SourcePath.String() != rootPath {
		t.Errorf("root.SourcePath = %q, want %q", root.SourcePath, rootPath)
	}
	if root.Harness != ingest.HarnessCursor {
		t.Errorf("root.Harness = %q, want %q", root.Harness, ingest.HarnessCursor)
	}
	if root.SourceFormat != ingest.SourceFormatJSONL {
		t.Errorf("root.SourceFormat = %q, want %q", root.SourceFormat, ingest.SourceFormatJSONL)
	}
	if root.CWD != "/home/test/testrepo" {
		t.Errorf("root.CWD = %q, want decoded project path", root.CWD)
	}
	if root.ProjectName != "testrepo" {
		t.Errorf("root.ProjectName = %q, want testrepo", root.ProjectName)
	}
	if root.Title != "Hello" {
		t.Errorf("root.Title = %q", root.Title)
	}
	if len(root.SubagentPaths) != 1 || root.SubagentPaths[0].String() != subPath {
		t.Fatalf("root.SubagentPaths = %v, want [%s]", root.SubagentPaths, subPath)
	}
	if sub.ParentUUID == nil || *sub.ParentUUID != ingest.SessionID(rootID) {
		t.Fatalf("sub.ParentUUID = %v, want %s", sub.ParentUUID, rootID)
	}
}

func TestCursorAdapter_ExtractMetadata(t *testing.T) {
	const (
		base      = testCursorProjectsRoot
		workspace = "home-test-testrepo"
		rootID    = testutil.TestSessionUUID
		subID     = testutil.TestSessionUUID2
	)
	mfs := testutil.NewMemFS()
	if err := mfs.MkdirAll("/home/test/testrepo", 0755); err != nil {
		t.Fatalf("MkdirAll project: %v", err)
	}
	setupCursorRootFS(t, mfs, base, workspace, rootID, sampleCursorJSONL())
	setupCursorSubagentFS(t, mfs, base, workspace, rootID, subID, sampleCursorJSONL())

	adapter := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var root ingest.DiscoveredSession
	for _, session := range sessions {
		if session.SessionID == ingest.SessionID(rootID) {
			root = session
			break
		}
	}
	if root.SessionID == "" {
		t.Fatal("root session not found")
	}

	meta, err := adapter.ExtractMetadata(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if meta.ModelHarness != ingest.HarnessCursor {
		t.Errorf("ModelHarness = %q, want %q", meta.ModelHarness, ingest.HarnessCursor)
	}
	if meta.Source.Format != ingest.SourceFormatJSONL {
		t.Errorf("Source.Format = %q, want %q", meta.Source.Format, ingest.SourceFormatJSONL)
	}
	if meta.Model.String() != "cursor-agent" {
		t.Errorf("Model = %q, want cursor-agent", meta.Model)
	}
	if meta.Stats.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", meta.Stats.TurnCount)
	}
	if meta.Stats.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", meta.Stats.ToolCallCount)
	}
	if meta.Stats.SubagentCount != 1 {
		t.Errorf("SubagentCount = %d, want 1", meta.Stats.SubagentCount)
	}
	if meta.Stats.TokensIn != 10 || meta.Stats.TokensOut != 20 {
		t.Errorf("Tokens = (%d,%d), want (10,20)", meta.Stats.TokensIn, meta.Stats.TokensOut)
	}
	if meta.CWD != "/home/test/testrepo" {
		t.Errorf("CWD = %q, want decoded project path", meta.CWD)
	}
	if meta.Project.Name != testutil.TestProjectName {
		t.Errorf("Project.Name = %q, want %q", meta.Project.Name, testutil.TestProjectName)
	}
	if len(meta.Subagents) != 1 || meta.Subagents[0].SessionID != ingest.SessionID(subID) {
		t.Fatalf("Subagents = %+v, want %s", meta.Subagents, subID)
	}
}

func TestCursorAdapter_ExtractMetadata_WarnsWhenCursorOmitsModel(t *testing.T) {
	const (
		base      = testCursorProjectsRoot
		workspace = "home-test-testrepo"
		rootID    = testutil.TestSessionUUID
	)
	data := []byte(strings.Join([]string{
		`{"role":"user","timestamp":"2026-06-04T10:00:00Z","message":{"content":[{"type":"text","text":"Hello"}]}}`,
		`{"role":"assistant","timestamp":"2026-06-04T10:00:05Z","message":{"content":[{"type":"text","text":"Hello!"}]}}`,
	}, "\n") + "\n")
	mfs := testutil.NewMemFS()
	if err := mfs.MkdirAll("/home/test/testrepo", 0755); err != nil {
		t.Fatalf("MkdirAll project: %v", err)
	}
	setupCursorRootFS(t, mfs, base, workspace, rootID, data)

	adapter := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover returned %d sessions, want 1", len(sessions))
	}

	meta, err := adapter.ExtractMetadata(context.Background(), sessions[0])
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if meta.Model != "" {
		t.Errorf("Model = %q, want empty model so push ErrNoModel gate holds the session", meta.Model)
	}
	if !hasDiagnosticWarning(meta.Diagnostics.Warnings, "missing_model") {
		t.Fatalf("Diagnostics.Warnings = %+v, want missing_model warning", meta.Diagnostics.Warnings)
	}
}

func hasDiagnosticWarning(warnings []ingest.DiagnosticEntry, errorType string) bool {
	for _, warning := range warnings {
		if warning.ErrorType == errorType {
			return true
		}
	}
	return false
}

func TestCursorIndexer_BasicAndFullDepth(t *testing.T) {
	const sessionID = testutil.TestSessionUUID
	mfs := testutil.NewMemFS()
	path := "/cursor/session.jsonl"
	if err := mfs.WriteFile(path, sampleCursorJSONL(), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:    ingest.SessionID(sessionID),
		Harness:      ingest.HarnessCursor,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}

	idx := ingest.NewCursorIndexer(mfs, ingest.WithCursorFullDepth(true))
	entries, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 7 {
		t.Fatalf("expected 7 entries with full-depth blocks, got %d", len(entries))
	}
	if entries[0].Harness != ingest.HarnessCursor {
		t.Errorf("entries[0].Harness = %q, want %q", entries[0].Harness, ingest.HarnessCursor)
	}
	if entries[0].Role != ingest.RoleUser || entries[0].EntryType != ingest.EntryTypeText {
		t.Errorf("entry 0 role/type = %s/%s", entries[0].Role, entries[0].EntryType)
	}
	if entries[0].ContentPreview == nil || *entries[0].ContentPreview != "Hello" {
		t.Errorf("entry 0 preview = %v", entries[0].ContentPreview)
	}
	if entries[2].Role != ingest.RoleAssistant || entries[2].EntryType != ingest.EntryTypeToolUse {
		t.Errorf("entry 2 role/type = %s/%s, want assistant/tool_use", entries[2].Role, entries[2].EntryType)
	}
	if entries[2].ToolNamesCSV == nil || *entries[2].ToolNamesCSV != "ReadFile" {
		t.Errorf("entry 2 ToolNamesCSV = %v, want ReadFile", entries[2].ToolNamesCSV)
	}
	if entries[4].EntryType != ingest.EntryTypeToolUse {
		t.Errorf("entry 4 EntryType = %s, want tool_use", entries[4].EntryType)
	}
	if entries[4].ToolKind == nil || entries[4].ToolKind.String() != "read" {
		t.Errorf("entry 4 ToolKind = %v, want read", entries[4].ToolKind)
	}
	if entries[4].ToolInput == nil || !strings.Contains(*entries[4].ToolInput, "/tmp/example") {
		t.Errorf("entry 4 ToolInput = %v", entries[4].ToolInput)
	}
}

func TestCursorIndexer_MalformedLineSkip(t *testing.T) {
	const sessionID = testutil.TestSessionUUID
	mfs := testutil.NewMemFS()
	path := "/cursor/malformed.jsonl"
	data := []byte(strings.Join([]string{
		`not valid json`,
		`{"role":"user","message":{"content":[{"type":"text","text":"valid line"}]}}`,
	}, "\n") + "\n")
	if err := mfs.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:  ingest.SessionID(sessionID),
		SourcePath: ingest.ResolvedPath(path),
	}

	entries, err := ingest.NewCursorIndexer(mfs).IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 valid entry, got %d", len(entries))
	}
	if entries[0].EntryIndex != 1 {
		t.Errorf("EntryIndex = %d, want 1", entries[0].EntryIndex)
	}
}

func TestCursorIndexer_PartTypeSet(t *testing.T) {
	mfs := testutil.NewMemFS()
	path := "/cursor/parttype.jsonl"
	data := []byte(strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"read_file","id":"tu1","input":{"file_path":"/a.go"}}]}}`,
	}, "\n") + "\n")
	if err := mfs.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:  ingest.SessionID(testutil.TestSessionUUID),
		SourcePath: ingest.ResolvedPath(path),
	}

	entries, err := ingest.NewCursorIndexer(mfs, ingest.WithCursorFullDepth(true)).IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	// All depth=1 entries must have PartType set.
	for _, e := range entries {
		if e.Depth == 1 && e.PartType == nil {
			t.Errorf("entry %d (depth=1, type=%s) has nil PartType", e.EntryIndex, e.EntryType)
		}
	}

	// Spot-check: the text block should have PartType="text", tool_use should have PartType="tool_use".
	wantParts := map[schema.EntryType]string{
		schema.EntryTypeText:    "text",
		schema.EntryTypeToolUse: "tool_use",
	}
	for _, e := range entries {
		if e.Depth != 1 {
			continue
		}
		if want, ok := wantParts[e.EntryType]; ok {
			if e.PartType == nil || *e.PartType != want {
				t.Errorf("entry %d EntryType=%s: PartType = %v, want %q", e.EntryIndex, e.EntryType, e.PartType, want)
			}
		}
	}
}

func TestCursorIndexer_UserQueryTagStripped(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantIn  string
		wantOut string
	}{
		{
			name:    "query-only",
			text:    "<user_query>\nCan you fix this issue?\n</user_query>",
			wantIn:  "Can you fix this issue?",
			wantOut: "<user_query>",
		},
		{
			name:    "timestamp-prefix",
			text:    "<timestamp>Sunday, May 31, 2026, 12:30 PM (UTC-7)</timestamp>\n<user_query>\nComparing this branch to develop\n</user_query>",
			wantIn:  "Comparing this branch to develop",
			wantOut: "<user_query>",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			path := "/cursor/strip.jsonl"
			line := `{"role":"user","message":{"content":[{"type":"text","text":` + `"` + strings.ReplaceAll(tc.text, "\n", `\n`) + `"` + `}]}}`
			if err := mfs.WriteFile(path, []byte(line+"\n"), 0644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			session := ingest.DiscoveredSession{
				SessionID:  ingest.SessionID(testutil.TestSessionUUID),
				SourcePath: ingest.ResolvedPath(path),
			}
			entries, err := ingest.NewCursorIndexer(mfs).IndexTranscript(context.Background(), session)
			if err != nil {
				t.Fatalf("IndexTranscript: %v", err)
			}
			if len(entries) == 0 {
				t.Fatal("expected at least one entry")
			}
			preview := ""
			if entries[0].ContentPreview != nil {
				preview = *entries[0].ContentPreview
			}
			if !strings.Contains(preview, tc.wantIn) {
				t.Errorf("ContentPreview %q does not contain %q", preview, tc.wantIn)
			}
			if strings.Contains(preview, tc.wantOut) {
				t.Errorf("ContentPreview %q should not contain %q (tag should be stripped)", preview, tc.wantOut)
			}
		})
	}
}

func TestCursorIndexer_ToolKindSnakeCase(t *testing.T) {
	mfs := testutil.NewMemFS()
	path := "/cursor/toolkind.jsonl"
	data := []byte(strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ndo stuff\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[` +
			`{"type":"tool_use","name":"read_file","id":"t1","input":{"file_path":"/a.go"}},` +
			`{"type":"tool_use","name":"run_terminal_cmd","id":"t2","input":{"command":"go test"}},` +
			`{"type":"tool_use","name":"grep_search","id":"t3","input":{"query":"foo"}},` +
			`{"type":"tool_use","name":"edit_file","id":"t4","input":{"file_path":"/a.go"}}` +
			`]}}`,
	}, "\n") + "\n")
	if err := mfs.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:  ingest.SessionID(testutil.TestSessionUUID),
		SourcePath: ingest.ResolvedPath(path),
	}

	entries, err := ingest.NewCursorIndexer(mfs, ingest.WithCursorFullDepth(true)).IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}

	wantKinds := map[string]schema.ToolCallKind{
		"read_file":        schema.ToolCallKindRead,
		"run_terminal_cmd": schema.ToolCallKindExecute,
		"grep_search":      schema.ToolCallKindSearch,
		"edit_file":        schema.ToolCallKindEdit,
	}

	for _, e := range entries {
		if e.Depth != 1 || e.EntryType != schema.EntryTypeToolUse || e.ToolNamesCSV == nil {
			continue
		}
		name := *e.ToolNamesCSV
		want, ok := wantKinds[name]
		if !ok {
			continue
		}
		if e.ToolKind == nil {
			t.Errorf("tool %q: ToolKind is nil, want %q", name, want)
			continue
		}
		if *e.ToolKind != want {
			t.Errorf("tool %q: ToolKind = %q, want %q", name, *e.ToolKind, want)
		}
	}
}

func TestCursorIndexer_FullDepth_UserQueryStripped(t *testing.T) {
	mfs := testutil.NewMemFS()
	path := "/cursor/fulldepth_strip.jsonl"
	line := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nFix the bug\n</user_query>"}]}}`
	if err := mfs.WriteFile(path, []byte(line+"\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:  ingest.SessionID(testutil.TestSessionUUID),
		SourcePath: ingest.ResolvedPath(path),
	}

	entries, err := ingest.NewCursorIndexer(mfs, ingest.WithCursorFullDepth(true)).IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	// Expect depth=0 + depth=1 entries for the single user message.
	if len(entries) < 2 {
		t.Fatalf("expected >=2 entries (depth=0 + depth=1), got %d", len(entries))
	}

	const want = "Fix the bug"
	for _, e := range entries {
		if e.ContentPreview == nil {
			continue
		}
		preview := *e.ContentPreview
		if strings.Contains(preview, "<user_query>") {
			t.Errorf("entry %d (depth=%d): ContentPreview contains raw <user_query> tag: %q", e.EntryIndex, e.Depth, preview)
		}
		if !strings.Contains(preview, want) {
			t.Errorf("entry %d (depth=%d): ContentPreview = %q, want it to contain %q", e.EntryIndex, e.Depth, preview, want)
		}
	}

	// Both depth=0 and depth=1 should have identical content (enabling EntriesToTurns dedup).
	var depth0, depth1 string
	for _, e := range entries {
		if e.ContentPreview == nil {
			continue
		}
		if e.Depth == 0 {
			depth0 = *e.ContentPreview
		} else if e.Depth == 1 {
			depth1 = *e.ContentPreview
		}
	}
	if depth0 != depth1 {
		t.Errorf("depth=0 preview %q != depth=1 preview %q; dedup in EntriesToTurns will not fire", depth0, depth1)
	}
}

// sampleCursorJSONLTwoAssistants builds a 4-line JSONL stream (user + assistant + user + assistant)
// with explicit usage on both assistant messages, for token-accumulation tests.
func sampleCursorJSONLTwoAssistants(in1, out1, in2, out2 int) []byte {
	lines := []string{
		`{"role":"user","timestamp":"2026-06-04T10:00:00Z","message":{"content":[{"type":"text","text":"first"}]}}`,
		fmt.Sprintf(`{"role":"assistant","timestamp":"2026-06-04T10:00:05Z","message":{"model":"cursor-agent","content":[{"type":"text","text":"reply1"}],"usage":{"input_tokens":%d,"output_tokens":%d}}}`, in1, out1),
		`{"role":"user","timestamp":"2026-06-04T10:01:00Z","message":{"content":[{"type":"text","text":"second"}]}}`,
		fmt.Sprintf(`{"role":"assistant","timestamp":"2026-06-04T10:01:05Z","message":{"model":"cursor-agent","content":[{"type":"text","text":"reply2"}],"usage":{"input_tokens":%d,"output_tokens":%d}}}`, in2, out2),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestCursorAdapter_Provider(t *testing.T) {
	a := ingest.NewCursorAdapter(testutil.NewMemFS(), testutil.DefaultGitResolver(), salt.Salt{})
	if got := a.Harness(); got != ingest.HarnessCursor {
		t.Errorf("Harness() = %q, want %q", got, ingest.HarnessCursor)
	}
}

func TestCursorAdapter_Discover_EmptyDir(t *testing.T) {
	const base = testCursorProjectsRoot
	mfs := testutil.NewMemFS()
	if err := mfs.MkdirAll(base, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	a := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	sessions, err := a.Discover(context.Background(), ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("Discover returned %d sessions, want 0", len(sessions))
	}
}

func TestCursorAdapter_Discover_MalformedWorkspace(t *testing.T) {
	const (
		base      = testCursorProjectsRoot
		workspace = "no-such-dir-xyz"
		rootID    = testutil.TestSessionUUID
	)
	mfs := testutil.NewMemFS()
	// Do NOT create any dirs matching the workspace slug — forces the fallback path.
	setupCursorRootFS(t, mfs, base, workspace, rootID, sampleCursorJSONL())

	a := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	sessions, err := a.Discover(context.Background(), ingest.SourceConfig{
		Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(base)},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Discover returned %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	wantCWD := filepath.Join(base, workspace)
	if s.CWD != wantCWD {
		t.Errorf("CWD = %q, want %q (fallback to raw workspace path)", s.CWD, wantCWD)
	}
	if s.ProjectName != workspace {
		t.Errorf("ProjectName = %q, want %q", s.ProjectName, workspace)
	}
	if s.Harness != ingest.HarnessCursor {
		t.Errorf("Harness = %q, want %q", s.Harness, ingest.HarnessCursor)
	}
}

func TestCursorAdapter_ExtractMetadata_NoGit(t *testing.T) {
	const filePath = "/cursor/nogit.jsonl"
	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(filePath, sampleCursorJSONL(), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessCursor,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		CWD:           testutil.TestDefaultWorktreeDir,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewCursorAdapter(mfs, testutil.NoRemoteGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if meta.Git.Remote != nil {
		t.Errorf("Git.Remote = %v, want nil when remote not available", meta.Git.Remote)
	}
	if meta.Git.Branch == nil || *meta.Git.Branch != testutil.TestDefaultBranch {
		t.Errorf("Git.Branch = %v, want %q", meta.Git.Branch, testutil.TestDefaultBranch)
	}
	if meta.Git.Worktree == nil {
		t.Error("Git.Worktree = nil, want non-nil")
	}
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings = %v, want none", meta.Diagnostics.Warnings)
	}
}

func TestCursorAdapter_ExtractMetadata_NoGitAtAll(t *testing.T) {
	const filePath = "/cursor/nogitall.jsonl"
	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(filePath, sampleCursorJSONL(), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessCursor,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		CWD:           testutil.TestDefaultWorktreeDir,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewCursorAdapter(mfs, testutil.NoGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if meta.Git.Remote != nil {
		t.Errorf("Git.Remote = %v, want nil", meta.Git.Remote)
	}
	if meta.Git.Branch != nil {
		t.Errorf("Git.Branch = %v, want nil", meta.Git.Branch)
	}
	if meta.Git.Worktree != nil {
		t.Errorf("Git.Worktree = %v, want nil", meta.Git.Worktree)
	}
	if meta.Git.Tracking != nil {
		t.Errorf("Git.Tracking = %v, want nil", meta.Git.Tracking)
	}
	if meta.ModelHarness != ingest.HarnessCursor {
		t.Errorf("ModelHarness = %q, want %q", meta.ModelHarness, ingest.HarnessCursor)
	}
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings = %v, want none", meta.Diagnostics.Warnings)
	}
}

func TestCursorAdapter_ExtractMetadata_CorruptJSONL(t *testing.T) {
	const filePath = "/cursor/corrupt.jsonl"
	validLine1 := `{"role":"user","timestamp":"2026-06-04T10:00:00Z","message":{"content":[{"type":"text","text":"hello"}]}}`
	corruptLine := `{not valid json`
	validLine2 := `{"role":"assistant","timestamp":"2026-06-04T10:00:05Z","message":{"model":"cursor-agent","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":10}}}`
	data := []byte(validLine1 + "\n" + corruptLine + "\n" + validLine2 + "\n")

	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(filePath, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessCursor,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if len(meta.Diagnostics.Warnings) == 0 {
		t.Error("Diagnostics.Warnings is empty, want at least 1 parse_error warning")
	}
	foundParseError := false
	for _, w := range meta.Diagnostics.Warnings {
		if w.ErrorType == "parse_error" {
			foundParseError = true
			break
		}
	}
	if !foundParseError {
		t.Errorf("no parse_error warning found in %+v", meta.Diagnostics.Warnings)
	}
	if meta.Stats.TurnCount < 2 {
		t.Errorf("TurnCount = %d, want >= 2 (valid lines still counted)", meta.Stats.TurnCount)
	}
}

func TestCursorAdapter_ExtractMetadata_TokenCounts(t *testing.T) {
	const filePath = "/cursor/tokens.jsonl"
	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(filePath, sampleCursorJSONLTwoAssistants(100, 50, 200, 150), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessCursor,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if meta.Stats.TokensIn != 300 {
		t.Errorf("TokensIn = %d, want 300 (100+200)", meta.Stats.TokensIn)
	}
	if meta.Stats.TokensOut != 200 {
		t.Errorf("TokensOut = %d, want 200 (50+150)", meta.Stats.TokensOut)
	}
	if meta.Stats.TurnCount != 4 {
		t.Errorf("TurnCount = %d, want 4", meta.Stats.TurnCount)
	}
}

func TestCursorAdapter_ExtractMetadata_ZeroUsage(t *testing.T) {
	const filePath = "/cursor/zerousage.jsonl"
	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(filePath, sampleCursorJSONLTwoAssistants(0, 0, 0, 0), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessCursor,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if meta.Stats.TokensIn != 0 {
		t.Errorf("TokensIn = %d, want 0", meta.Stats.TokensIn)
	}
	if meta.Stats.TokensOut != 0 {
		t.Errorf("TokensOut = %d, want 0", meta.Stats.TokensOut)
	}
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings = %v, want none", meta.Diagnostics.Warnings)
	}
}

func TestCursorAdapter_ExtractMetadata_NoUsageField(t *testing.T) {
	const filePath = "/cursor/nousage.jsonl"
	lines := []string{
		`{"role":"user","timestamp":"2026-06-04T10:00:00Z","message":{"content":[{"type":"text","text":"hello"}]}}`,
		`{"role":"assistant","timestamp":"2026-06-04T10:00:05Z","message":{"model":"cursor-agent","content":[{"type":"text","text":"hi"}]}}`,
	}
	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	session := ingest.DiscoveredSession{
		SessionID:     sid,
		Harness:       ingest.HarnessCursor,
		SourcePath:    ingest.ResolvedPath(filePath),
		SourceFormat:  ingest.SourceFormatJSONL,
		SubagentPaths: []ingest.ResolvedPath{},
		DebugPaths:    []ingest.ResolvedPath{},
	}

	a := ingest.NewCursorAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	meta, err := a.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if meta.Stats.TokensIn != 0 {
		t.Errorf("TokensIn = %d, want 0", meta.Stats.TokensIn)
	}
	if meta.Stats.TokensOut != 0 {
		t.Errorf("TokensOut = %d, want 0", meta.Stats.TokensOut)
	}
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings = %v, want none", meta.Diagnostics.Warnings)
	}
}
