package ingest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// codexFixtureOpts parameterizes the synthetic rollout the helper emits. The
// defaults exercise the full event-type vocabulary so adapter changes that
// silently drop a case will fail observable assertions.
type codexFixtureOpts struct {
	sessionID       string
	cwd             string
	model           string
	cliVersion      string
	repositoryURL   string
	branch          string
	startISO        string // session_meta.payload.timestamp
	endISO          string // last envelope timestamp; drives meta.Timestamp.End
	inputTokens     int
	cachedTokens    int
	outputTokens    int
	reasoningTokens int
	omitSessionMeta bool // when true, skip the session_meta line entirely
	omitGit         bool // when true, emit session_meta without the git block
}

// codexRolloutJSONL builds a representative Codex rollout JSONL covering every
// event type the adapter dispatches on (session_meta, turn_context,
// response_item.{message,function_call,function_call_output,custom_tool_call,
// custom_tool_call_output,reasoning}, event_msg.{token_count,task_complete}).
func codexRolloutJSONL(o codexFixtureOpts) []byte {
	var lines []string

	if !o.omitSessionMeta {
		gitFragment := fmt.Sprintf(`,"git":{"commit_hash":"abc123","branch":%q,"repository_url":%q}`, o.branch, o.repositoryURL)
		if o.omitGit {
			gitFragment = ""
		}
		lines = append(lines, fmt.Sprintf(
			`{"timestamp":"2026-05-30T04:29:24.992Z","type":"session_meta","payload":{"id":%q,"timestamp":%q,"cwd":%q,"originator":"codex_vscode","cli_version":%q,"source":"vscode","thread_source":"user","model_provider":"openai"%s}}`,
			o.sessionID, o.startISO, o.cwd, o.cliVersion, gitFragment))
	}

	lines = append(lines,
		`{"timestamp":"2026-05-30T04:29:25.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"019e7724-57c2-71f1-a865-4bf8dee6a242","started_at":1780115364,"model_context_window":258400}}`,
		fmt.Sprintf(
			`{"timestamp":"2026-05-30T04:29:25.001Z","type":"turn_context","payload":{"cwd":%q,"current_date":"2026-05-29","timezone":"UTC","model":%q,"effort":"medium","turn_id":"019e7724-57c2-71f1-a865-4bf8dee6a242"}}`,
			o.cwd, o.model),
		`{"timestamp":"2026-05-30T04:29:25.002Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first prompt"}]}}`,
		`{"timestamp":"2026-05-30T04:29:26.556Z","type":"response_item","payload":{"type":"reasoning","summary":[],"content":null,"encrypted_content":"opaque-blob"}}`,
		`{"timestamp":"2026-05-30T04:29:31.611Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first response"}],"phase":"commentary"}}`,
		`{"timestamp":"2026-05-30T04:29:31.612Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}","call_id":"call_test_aaa"}}`,
		`{"timestamp":"2026-05-30T04:29:31.777Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_test_aaa","output":"ok"}}`,
		`{"timestamp":"2026-05-30T04:29:32.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"+++ b/file.txt","call_id":"call_test_bbb"}}`,
		`{"timestamp":"2026-05-30T04:29:32.100Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_test_bbb","output":"applied"}}`,
		fmt.Sprintf(
			`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d,"total_tokens":%d},"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d,"total_tokens":%d},"model_context_window":258400}}}`,
			o.endISO,
			o.inputTokens, o.cachedTokens, o.outputTokens, o.reasoningTokens, o.inputTokens+o.outputTokens,
			o.inputTokens, o.cachedTokens, o.outputTokens, o.reasoningTokens, o.inputTokens+o.outputTokens),
	)

	return []byte(strings.Join(lines, "\n") + "\n")
}

// codexMultiAgentCoordinationToolNames is the full documented set of Codex's
// multi-agent coordination tool names. They are currently classified
// ToolCallKindOther pending a category decision. Exported as its own entry (not just
// inlined into the fixture) so the pinning test in codex_indexer_test.go can
// assert ALL of them, not a single representative — a partial migration
// (some names re-categorized, others left behind) must fail this test just
// as loudly as leaving all of them unmapped would pass it today.
var codexMultiAgentCoordinationToolNames = []string{
	"wait_agent", "send_message", "followup_task", "list_agents",
	"wait", "spawn_agent", "interrupt_agent", "request_user_input",
}

// codexMultiAgentRolloutJSONL builds a synthetic representative rollout: several
// leading developer-role messages (permissions instructions, persona,
// multi_agent_mode) followed by TWO injected user-role messages that are
// session-bootstrap content (AGENTS.md, an orchestrator skill file) — not
// human-typed — before the first human message, plus a Codex `exec`
// tool call and one function_call per documented multi-agent coordination
// tool name (codexMultiAgentCoordinationToolNames). This is the exact shape
// that covers both tool classification (`exec`, not `exec_command`) and role attribution
// (do developer-role bootstrap messages ever leak into RoleUser?) — see
// internal/ingest/codex_indexer_test.go.
func codexMultiAgentRolloutJSONL(o codexFixtureOpts) []byte {
	lines := []string{
		fmt.Sprintf(
			`{"timestamp":"2026-05-30T04:29:24.992Z","type":"session_meta","payload":{"id":%q,"timestamp":%q,"cwd":%q,"originator":"codex_cli_rs","cli_version":%q,"source":"cli","thread_source":"user","model_provider":"openai"}}`,
			o.sessionID, o.startISO, o.cwd, o.cliVersion),
		`{"timestamp":"2026-05-30T04:29:25.000Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions> filesystem sandboxing defines which files can be read or written."}]}}`,
		`{"timestamp":"2026-05-30T04:29:25.001Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are the primary agent in a team of agents collaborating."}]}}`,
		`{"timestamp":"2026-05-30T04:29:25.002Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<multi_agent_mode>Proactive multi-agent delegation is active.</multi_agent_mode>"}]}}`,
		`{"timestamp":"2026-05-30T04:29:25.003Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /home/test/myproject"}]}}`,
		`{"timestamp":"2026-05-30T04:29:25.004Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"---\nname: epoch\ndescription: Master orchestrator skill\n---"}]}}`,
		`{"timestamp":"2026-05-30T04:29:25.005Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Targeted"}]}}`,
		`{"timestamp":"2026-05-30T04:29:26.556Z","type":"response_item","payload":{"type":"reasoning","summary":[],"content":null,"encrypted_content":"opaque-blob"}}`,
		`{"timestamp":"2026-05-30T04:29:31.611Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"On it."}]}}`,
		`{"timestamp":"2026-05-30T04:29:31.612Z","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{\"cmd\":\"pwd\"}","call_id":"call_test_exec"}}`,
		`{"timestamp":"2026-05-30T04:29:31.777Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_test_exec","output":"ok"}}`,
	}
	for i, name := range codexMultiAgentCoordinationToolNames {
		callID := fmt.Sprintf("call_test_%s", name)
		lines = append(lines,
			fmt.Sprintf(`{"timestamp":"2026-05-30T04:29:%02d.000Z","type":"response_item","payload":{"type":"function_call","name":%q,"arguments":"{}","call_id":%q}}`, 32+i, name, callID),
			fmt.Sprintf(`{"timestamp":"2026-05-30T04:29:%02d.100Z","type":"response_item","payload":{"type":"function_call_output","call_id":%q,"output":"done"}}`, 32+i, callID),
		)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// codexRolloutPath builds the rollout-{ISO-timestamp}-{sessionID}.jsonl path
// under {base}/YYYY/MM/DD/ matching the Codex CLI on-disk layout.
func codexRolloutPath(base, year, month, day, isoStamp, sessionID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/rollout-%s-%s.jsonl", base, year, month, day, isoStamp, sessionID)
}

// codexSessionIndexPath returns the path Codex CLI uses for its sidecar
// thread-name index, one level above the dated sessions tree.
func codexSessionIndexPath(base string) string {
	return fmt.Sprintf("%s/session_index.jsonl", strings.TrimSuffix(base, "/sessions"))
}

// writeCodexSessionIndex seeds the {parent}/session_index.jsonl file with the
// given session_id → thread_name pairs, matching the real Codex CLI format.
func writeCodexSessionIndex(t *testing.T, mfs *testutil.MemFS, base string, titles map[string]string) {
	t.Helper()
	var lines []string
	for id, name := range titles {
		lines = append(lines, fmt.Sprintf(
			`{"id":%q,"thread_name":%q,"updated_at":"2026-05-30T04:14:49.751917951Z"}`, id, name))
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	if err := mfs.WriteFile(codexSessionIndexPath(base), data, 0644); err != nil {
		t.Fatalf("writeCodexSessionIndex: %v", err)
	}
}

// defaultCodexFixtureOpts returns options exercising the happy-path adapter
// scan: 1 user + 1 assistant turn, 2 tool calls (function + custom), populated
// git context, populated token totals.
func defaultCodexFixtureOpts() codexFixtureOpts {
	return codexFixtureOpts{
		sessionID:       testutil.TestSessionUUID,
		cwd:             "/home/test/myproject",
		model:           "gpt-5.5",
		cliVersion:      "0.133.0-alpha.1",
		repositoryURL:   testutil.TestGitRemote,
		branch:          "main",
		startISO:        "2026-05-30T04:29:04.063Z",
		endISO:          "2026-05-30T04:29:40.000Z",
		inputTokens:     100,
		cachedTokens:    40,
		outputTokens:    20,
		reasoningTokens: 5,
	}
}

// --- Tests ---

func TestCodexAdapter_Harness(t *testing.T) {
	a := ingest.NewCodexAdapter(testutil.NewMemFS(), testutil.DefaultGitResolver(), salt.Salt{})
	if got := a.Harness(); got != ingest.HarnessCodex {
		t.Errorf("Harness() = %q, want %q", got, ingest.HarnessCodex)
	}
}

func TestCodexAdapter_Discover_SingleRollout(t *testing.T) {
	const base = "/home/test/.codex/sessions"
	opts := defaultCodexFixtureOpts()

	mfs := testutil.NewMemFS()
	path := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(path, codexRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed: WriteFile: %v", err)
	}

	a := ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	rp, _ := ingest.NewResolvedPath(base)
	got, err := a.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{rp}, Enabled: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Discover: expected 1 session, got %d", len(got))
	}
	ds := got[0]
	if ds.SessionID.String() != opts.sessionID {
		t.Errorf("SessionID: got %q, want %q", ds.SessionID, opts.sessionID)
	}
	if ds.Harness != ingest.HarnessCodex {
		t.Errorf("Harness: got %q, want %q", ds.Harness, ingest.HarnessCodex)
	}
	if ds.SourceFormat != ingest.SourceFormatJSONL {
		t.Errorf("SourceFormat: got %q, want %q", ds.SourceFormat, ingest.SourceFormatJSONL)
	}
	if ds.ParentUUID != nil {
		t.Errorf("ParentUUID: expected nil (Codex has no subagents), got %v", ds.ParentUUID)
	}
	if string(ds.SourcePath) != path {
		t.Errorf("SourcePath: got %q, want %q", ds.SourcePath, path)
	}
}

func TestCodexAdapter_Discover_MultipleDays(t *testing.T) {
	const base = "/home/test/.codex/sessions"
	a := ingest.NewCodexAdapter(testutil.NewMemFS(), testutil.DefaultGitResolver(), salt.Salt{})

	mfs := testutil.NewMemFS()
	opts1 := defaultCodexFixtureOpts()
	opts2 := defaultCodexFixtureOpts()
	opts2.sessionID = testutil.TestSessionUUID2

	p1 := codexRolloutPath(base, "2026", "05", "28", "2026-05-28T10-00-00", opts1.sessionID)
	p2 := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T11-00-00", opts2.sessionID)
	if err := mfs.WriteFile(p1, codexRolloutJSONL(opts1), 0644); err != nil {
		t.Fatalf("seed p1: %v", err)
	}
	if err := mfs.WriteFile(p2, codexRolloutJSONL(opts2), 0644); err != nil {
		t.Fatalf("seed p2: %v", err)
	}

	a = ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	rp, _ := ingest.NewResolvedPath(base)
	got, err := a.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{rp}, Enabled: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Discover: expected 2 sessions, got %d", len(got))
	}

	seen := map[string]bool{}
	for _, ds := range got {
		seen[ds.SessionID.String()] = true
	}
	for _, want := range []string{opts1.sessionID, opts2.sessionID} {
		if !seen[want] {
			t.Errorf("Discover: missing session %q in %v", want, seen)
		}
	}
}

// TestCodexAdapter_Discover_PopulatesHintsAndTitle verifies that the wizard's
// session-list rendering fields (Title, ProjectName, Branch, CWD) are filled
// in during discovery. Title comes from session_index.jsonl; the others come
// from each rollout's session_meta line.
func TestCodexAdapter_Discover_PopulatesHintsAndTitle(t *testing.T) {
	const base = "/home/test/.codex/sessions"
	opts := defaultCodexFixtureOpts()
	const wantTitle = "Summarize project"

	mfs := testutil.NewMemFS()
	rollout := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(rollout, codexRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed rollout: %v", err)
	}
	writeCodexSessionIndex(t, mfs, base, map[string]string{opts.sessionID: wantTitle})

	a := ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	rp, _ := ingest.NewResolvedPath(base)
	got, err := a.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{rp}, Enabled: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d", len(got))
	}
	ds := got[0]
	if ds.Title != wantTitle {
		t.Errorf("Title: got %q, want %q (from session_index)", ds.Title, wantTitle)
	}
	if ds.ProjectName != "myproject" {
		t.Errorf("ProjectName: got %q, want %q (from basename of session_meta.cwd)", ds.ProjectName, "myproject")
	}
	if ds.Branch != opts.branch {
		t.Errorf("Branch: got %q, want %q (from session_meta.git.branch)", ds.Branch, opts.branch)
	}
	if ds.CWD != opts.cwd {
		t.Errorf("CWD: got %q, want %q (from session_meta.cwd)", ds.CWD, opts.cwd)
	}
}

// TestCodexAdapter_Discover_NoSessionIndex verifies the title falls back to
// empty (and the rest of the hints still populate) when session_index.jsonl
// is absent — the wizard will render the date instead, which is the current
// graceful degradation contract.
func TestCodexAdapter_Discover_NoSessionIndex(t *testing.T) {
	const base = "/home/test/.codex/sessions"
	opts := defaultCodexFixtureOpts()

	mfs := testutil.NewMemFS()
	rollout := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(rollout, codexRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed rollout: %v", err)
	}
	// Deliberately NOT seeding the session_index file.

	a := ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	rp, _ := ingest.NewResolvedPath(base)
	got, err := a.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{rp}, Enabled: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d", len(got))
	}
	ds := got[0]
	if ds.Title != "" {
		t.Errorf("Title: got %q, want empty (no session_index)", ds.Title)
	}
	// Hints from session_meta should still populate — index absence must not
	// suppress them.
	if ds.ProjectName == "" || ds.Branch == "" || ds.CWD == "" {
		t.Errorf("hints from session_meta lost: project=%q branch=%q cwd=%q", ds.ProjectName, ds.Branch, ds.CWD)
	}
}

// TestCodexAdapter_Discover_SessionMetaNoGit verifies that a rollout whose
// session_meta omits the git block still discovers cleanly: CWD/ProjectName
// populate, Branch stays empty rather than panicking on a nil git pointer.
func TestCodexAdapter_Discover_SessionMetaNoGit(t *testing.T) {
	const base = "/home/test/.codex/sessions"
	opts := defaultCodexFixtureOpts()
	opts.omitGit = true

	mfs := testutil.NewMemFS()
	rollout := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(rollout, codexRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed rollout: %v", err)
	}

	a := ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	rp, _ := ingest.NewResolvedPath(base)
	got, err := a.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{rp}, Enabled: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d", len(got))
	}
	ds := got[0]
	if ds.Branch != "" {
		t.Errorf("Branch: got %q, want empty (session_meta had no git block)", ds.Branch)
	}
	if ds.CWD != opts.cwd {
		t.Errorf("CWD should still populate without git: got %q, want %q", ds.CWD, opts.cwd)
	}
	if ds.ProjectName == "" {
		t.Error("ProjectName should still derive from cwd even without git")
	}
}

func TestCodexAdapter_Discover_IgnoresUnrelatedFiles(t *testing.T) {
	const base = "/home/test/.codex/sessions"
	mfs := testutil.NewMemFS()

	// One valid rollout — the only entry that should be discovered.
	opts := defaultCodexFixtureOpts()
	rollout := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(rollout, codexRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed rollout: %v", err)
	}

	// Sibling files that must be ignored:
	//   * non-rollout JSONL at the right depth (no rollout- prefix)
	//   * rollout-*.jsonl at the wrong depth (not under YYYY/MM/DD/)
	//   * config.toml at root
	if err := mfs.WriteFile(base+"/2026/05/29/notes.jsonl", []byte("{}\n"), 0644); err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	if err := mfs.WriteFile(base+"/rollout-shallow.jsonl", []byte("{}\n"), 0644); err != nil {
		t.Fatalf("seed shallow: %v", err)
	}
	if err := mfs.WriteFile(base+"/../config.toml", []byte("[]"), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	rp, _ := ingest.NewResolvedPath(base)
	got, err := a.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{rp}, Enabled: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Discover: expected 1 session (only the valid rollout), got %d", len(got))
	}
	if got[0].SessionID.String() != opts.sessionID {
		t.Errorf("discovered session: got %q, want %q", got[0].SessionID, opts.sessionID)
	}
}

func TestCodexAdapter_ExtractMetadata(t *testing.T) {
	const base = "/home/test/.codex/sessions"
	opts := defaultCodexFixtureOpts()

	mfs := testutil.NewMemFS()
	rollout := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(rollout, codexRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed rollout: %v", err)
	}

	a := ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	sid, _ := ingest.NewSessionID(opts.sessionID)
	ds := ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(rollout),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	meta, err := a.ExtractMetadata(context.Background(), ds)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	if meta.ModelHarness != ingest.HarnessCodex {
		t.Errorf("ModelHarness: got %q, want %q", meta.ModelHarness, ingest.HarnessCodex)
	}
	if meta.Source.Format != ingest.SourceFormatJSONL {
		t.Errorf("Source.Format: got %q, want %q", meta.Source.Format, ingest.SourceFormatJSONL)
	}
	if meta.Model.String() != opts.model {
		t.Errorf("Model: got %q, want %q (from turn_context)", meta.Model, opts.model)
	}
	if meta.Version != opts.cliVersion {
		t.Errorf("Version: got %q, want %q (from session_meta.cli_version)", meta.Version, opts.cliVersion)
	}
	if meta.CWD != opts.cwd {
		t.Errorf("CWD: got %q, want %q", meta.CWD, opts.cwd)
	}
	if meta.Git.Branch == nil || *meta.Git.Branch != opts.branch {
		t.Errorf("Git.Branch: got %v, want pointer to %q", meta.Git.Branch, opts.branch)
	}
	if meta.Git.Remote == nil || *meta.Git.Remote != opts.repositoryURL {
		t.Errorf("Git.Remote: got %v, want pointer to %q", meta.Git.Remote, opts.repositoryURL)
	}
	if meta.Project.Name != "myproject" {
		t.Errorf("Project.Name: got %q, want %q", meta.Project.Name, "myproject")
	}
	if meta.Project.FilePath != opts.cwd {
		t.Errorf("Project.FilePath: got %q, want %q", meta.Project.FilePath, opts.cwd)
	}

	// Stats — fixture is 1 user + 1 assistant message, 1 function_call, 1
	// custom_tool_call. function_call_output / custom_tool_call_output do NOT
	// count as additional tool calls (they are results, not invocations).
	if meta.Stats.TurnCount != 2 {
		t.Errorf("Stats.TurnCount: got %d, want 2", meta.Stats.TurnCount)
	}
	if meta.Stats.ToolCallCount != 2 {
		t.Errorf("Stats.ToolCallCount: got %d, want 2", meta.Stats.ToolCallCount)
	}
	if meta.Stats.TokensIn != opts.inputTokens {
		t.Errorf("Stats.TokensIn: got %d, want %d", meta.Stats.TokensIn, opts.inputTokens)
	}
	if meta.Stats.TokensOut != opts.outputTokens {
		t.Errorf("Stats.TokensOut: got %d, want %d", meta.Stats.TokensOut, opts.outputTokens)
	}
	if meta.Stats.CachedReadTokens == nil || *meta.Stats.CachedReadTokens != opts.cachedTokens {
		t.Errorf("Stats.CachedReadTokens: got %v, want pointer to %d", meta.Stats.CachedReadTokens, opts.cachedTokens)
	}
	if meta.Stats.ThoughtTokens == nil || *meta.Stats.ThoughtTokens != opts.reasoningTokens {
		t.Errorf("Stats.ThoughtTokens: got %v, want pointer to %d", meta.Stats.ThoughtTokens, opts.reasoningTokens)
	}

	// Timestamps + duration.
	if meta.Timestamp.Start <= 0 {
		t.Errorf("Timestamp.Start: expected > 0, got %d", meta.Timestamp.Start)
	}
	if meta.Timestamp.End <= meta.Timestamp.Start {
		t.Errorf("Timestamp.End (%d) must be > Start (%d)", meta.Timestamp.End, meta.Timestamp.Start)
	}
	if meta.Stats.DurationMs <= 0 {
		t.Errorf("Stats.DurationMs: expected > 0, got %d", meta.Stats.DurationMs)
	}

	// No diagnostics on the happy path.
	if len(meta.Diagnostics.Warnings) != 0 {
		t.Errorf("Diagnostics.Warnings: expected none, got %+v", meta.Diagnostics.Warnings)
	}

	// ParentUUID always nil for Codex.
	if meta.ParentUUID != nil {
		t.Errorf("ParentUUID: expected nil, got %v", meta.ParentUUID)
	}
}

func TestCodexAdapter_ExtractMetadata_MissingSessionMeta(t *testing.T) {
	const base = "/home/test/.codex/sessions"
	opts := defaultCodexFixtureOpts()
	opts.omitSessionMeta = true

	mfs := testutil.NewMemFS()
	rollout := codexRolloutPath(base, "2026", "05", "29", "2026-05-29T21-14-24", opts.sessionID)
	if err := mfs.WriteFile(rollout, codexRolloutJSONL(opts), 0644); err != nil {
		t.Fatalf("seed rollout: %v", err)
	}

	a := ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
	sid, _ := ingest.NewSessionID(opts.sessionID)
	ds := ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(rollout),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	meta, err := a.ExtractMetadata(context.Background(), ds)
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}

	// Adapter must emit a missing_session_meta diagnostic, not error.
	var hasDiag bool
	for _, w := range meta.Diagnostics.Warnings {
		if w.ErrorType == "missing_session_meta" {
			hasDiag = true
			break
		}
	}
	if !hasDiag {
		t.Errorf("expected missing_session_meta diagnostic, got %+v", meta.Diagnostics.Warnings)
	}

	// Stats should still populate from later lines — turn_context gave us the
	// model, response_items gave us counts, token_count gave us totals.
	if meta.Model.String() != opts.model {
		t.Errorf("Model still expected from turn_context: got %q, want %q", meta.Model, opts.model)
	}
	if meta.Stats.TurnCount != 2 {
		t.Errorf("TurnCount: got %d, want 2", meta.Stats.TurnCount)
	}
}

// TestCodexAdapter_RegisteredInDefaultRegistry guards against silent
// regressions where someone removes the factory entry, which would cause
// Codex sessions to be discovered nowhere.
func TestCodexAdapter_RegisteredInDefaultRegistry(t *testing.T) {
	factory, ok := ingest.DefaultAdapterRegistry[defaults.HarnessCodex]
	if !ok {
		t.Fatalf("DefaultAdapterRegistry missing entry for HarnessCodex")
	}
	a := factory(testutil.NewMemFS(), testutil.DefaultGitResolver(), salt.Salt{})
	if a.Harness() != ingest.HarnessCodex {
		t.Errorf("registered adapter harness = %q, want %q", a.Harness(), ingest.HarnessCodex)
	}
}
