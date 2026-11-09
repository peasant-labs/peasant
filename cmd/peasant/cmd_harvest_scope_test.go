package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/spf13/cobra"
)

//go:embed testdata/session_scope_errors/cases.yaml
var sessionScopeGoldenYAML []byte

const (
	scopeTestSessionID = "5a1f0c3e-7b2d-4e8a-9c61-0d4b3e2f1a70"
	scopeTestChildID   = "agent-a1a1a1a1a1a1a1a1a"
	scopeTestUnknownID = "11111111-2222-4333-8444-555566667777"
)

// scopeGoldenFixture is the command-level case table. Every golden is the exact
// bytes a command prints, so a fixture case never carries the text itself.
type scopeGoldenFixture struct {
	RequiredNames  []string          `yaml:"required_names"`
	SessionRecord  string            `yaml:"session_record"`
	SubagentRecord string            `yaml:"subagent_record"`
	Cases          []scopeGoldenCase `yaml:"cases"`
}

type scopeGoldenCase struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command"` // harvest, index, open
	Setup   string   `yaml:"setup"`
	Args    []string `yaml:"args"`
	Kind    string   `yaml:"kind"`
}

// goldenKind reports whether a case kind pins a printed golden.
func goldenKind(kind string) bool {
	switch kind {
	case "miss_block", "miss_line", "warning", "open_warning", "open_hook_warning":
		return true
	default:
		return false
	}
}

func loadScopeGoldenFixture(t *testing.T) scopeGoldenFixture {
	t.Helper()
	var fixture scopeGoldenFixture
	if err := testutil.DecodeFixtureYAML(sessionScopeGoldenYAML, &fixture); err != nil {
		t.Fatalf("decode command-level scope fixture: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if c.Name == "" || present[c.Name] {
			t.Fatalf("invalid command-level scope fixture case %q", c.Name)
		}
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("testdata/session_scope_errors/cases.yaml", "command-level scope", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	kinds := make(map[string]string, len(fixture.Cases))
	for _, c := range fixture.Cases {
		kinds[c.Name] = c.Kind
	}
	if os.Getenv("UPDATE_SCOPE_GOLDENS") != "1" {
		for _, name := range fixture.RequiredNames {
			if !goldenKind(kinds[name]) {
				continue
			}
			if _, err := os.Stat(filepath.Join("testdata", "session_scope_errors", name+".golden")); err != nil {
				t.Fatalf("case %q has no golden: %v", name, err)
			}
		}
	}
	return fixture
}

// TestSessionScopeCommandGoldens drives every command form through its
// production entry point and pins the printed bytes.
func TestSessionScopeCommandGoldens(t *testing.T) {
	t.Parallel()
	fixture := loadScopeGoldenFixture(t)
	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			sandbox := newScopeSandbox(t, fixture)
			sandbox.applySetup(tc)
			sandbox.assertCase(t, tc)
		})
	}
}

// scopeWalkRecorder records the roots a run walks, so a scoped index run can be
// held to "zero WalkDir under the output root". It is a test-only decorator.
type scopeWalkRecorder struct {
	ingest.FileSystem
	mu    sync.Mutex
	walks []string
}

func (r *scopeWalkRecorder) WalkDir(root string, fn fs.WalkDirFunc) error {
	r.mu.Lock()
	r.walks = append(r.walks, root)
	r.mu.Unlock()
	return r.FileSystem.WalkDir(root, fn)
}

func (r *scopeWalkRecorder) walkedUnder(root string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []string
	for _, walked := range r.walks {
		if walked == root || strings.HasPrefix(walked, root+string(os.PathSeparator)) {
			found = append(found, walked)
		}
	}
	return found
}

// scopeSandbox is one isolated data/config/state root with a Claude Code source
// tree. It is the only setup every case shares.
type scopeSandbox struct {
	t       *testing.T
	fixture scopeGoldenFixture
	root    string
}

func newScopeSandbox(t *testing.T, fixture scopeGoldenFixture) *scopeSandbox {
	t.Helper()
	root := t.TempDir()
	if dir := os.Getenv("KEEP_SCOPE_SANDBOX"); dir != "" {
		var err error
		root, err = os.MkdirTemp(dir, "scope-")
		if err != nil {
			t.Fatal(err)
		}
	}
	return &scopeSandbox{t: t, fixture: fixture, root: root}
}

func (s *scopeSandbox) dataDir() string    { return filepath.Join(s.root, "data") }
func (s *scopeSandbox) configDir() string  { return filepath.Join(s.root, "config") }
func (s *scopeSandbox) stateDir() string   { return filepath.Join(s.root, "state") }
func (s *scopeSandbox) recordings() string { return filepath.Join(s.root, "recordings") }
func (s *scopeSandbox) outputDir() string {
	return filepath.Join(s.dataDir(), "peasant", "peasant-sync")
}
func (s *scopeSandbox) configPath() string {
	return filepath.Join(s.configDir(), "peasant", "config.yaml")
}
func (s *scopeSandbox) dbPath() string     { return string(defaults.ResolveDBFilePathWith(s.dataDir())) }
func (s *scopeSandbox) projectDir() string { return filepath.Join(s.recordings(), "-workspace") }
func (s *scopeSandbox) rootTranscript() string {
	return filepath.Join(s.projectDir(), scopeTestSessionID+".jsonl")
}
func (s *scopeSandbox) childTranscript() string {
	return filepath.Join(s.projectDir(), scopeTestSessionID, "subagents", scopeTestChildID+".jsonl")
}

func (s *scopeSandbox) writeConfig(claude, opencode bool, opencodeRoot string) {
	s.t.Helper()
	enabled := func(on bool) string {
		if on {
			return "true"
		}
		return "false"
	}
	claudePaths := ""
	if claude {
		claudePaths = fmt.Sprintf("\n    paths: [%q]", s.recordings())
	}
	opencodePaths := ""
	if opencode {
		opencodePaths = fmt.Sprintf("\n    paths: [%q]", opencodeRoot)
	}
	document := fmt.Sprintf(`version: 1
sources:
  claude-code:
    enabled: %s%s
  opencode:
    enabled: %s%s
  cursor: {enabled: false}
  codex: {enabled: false}
  strike: {enabled: false}
  pi: {enabled: false}
output:
  basePath: %q
`, enabled(claude), claudePaths, enabled(opencode), opencodePaths, s.outputDir())
	if err := os.MkdirAll(filepath.Dir(s.configPath()), 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(s.configPath(), []byte(document), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// writeSession writes the recorded root session, and optionally one recorded
// subagent transcript under it.
func (s *scopeSandbox) writeSession(withChild bool) {
	s.t.Helper()
	if err := os.MkdirAll(s.projectDir(), 0o700); err != nil {
		s.t.Fatal(err)
	}
	root := strings.ReplaceAll(s.fixture.SessionRecord, "SESSION_ID", scopeTestSessionID)
	if err := os.WriteFile(s.rootTranscript(), []byte(root), 0o600); err != nil {
		s.t.Fatal(err)
	}
	if !withChild {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.childTranscript()), 0o700); err != nil {
		s.t.Fatal(err)
	}
	child := strings.ReplaceAll(s.fixture.SubagentRecord, "SESSION_ID", scopeTestSessionID)
	child = strings.ReplaceAll(child, "AGENT_NAME", strings.TrimPrefix(scopeTestChildID, "agent-"))
	child = strings.ReplaceAll(child, "AGENT_ID", scopeTestChildID)
	if err := os.WriteFile(s.childTranscript(), []byte(child), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// appendChildTurn makes the subagent the only changed member of the cohort.
func (s *scopeSandbox) appendChildTurn() {
	s.t.Helper()
	record := fmt.Sprintf("{\"sessionId\":%q,\"agentId\":%q,\"isSidechain\":true,\"type\":\"user\",\"uuid\":\"%s-user-2\",\"timestamp\":\"2026-01-01T00:00:05Z\",\"cwd\":\"/workspace\",\"message\":{\"role\":\"user\",\"content\":\"another turn\"}}\n",
		scopeTestSessionID, strings.TrimPrefix(scopeTestChildID, "agent-"), scopeTestChildID)
	file, err := os.OpenFile(s.childTranscript(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(record); err != nil {
		s.t.Fatal(err)
	}
}

// deleteSource removes the recorded transcripts, so the store's recorded path
// no longer holds them.
func (s *scopeSandbox) deleteSource(withChild bool) {
	s.t.Helper()
	if err := os.Remove(s.rootTranscript()); err != nil {
		s.t.Fatal(err)
	}
	if withChild {
		if err := os.Remove(s.childTranscript()); err != nil {
			s.t.Fatal(err)
		}
	}
}

// deleteSavedPair removes the saved pair from the output tree, so the store
// records a session whose saved copy is gone.
func (s *scopeSandbox) deleteSavedPair() {
	s.t.Helper()
	matches, err := filepath.Glob(filepath.Join(s.outputDir(), "*", scopeTestSessionID))
	if err != nil {
		s.t.Fatal(err)
	}
	if len(matches) != 1 {
		s.t.Fatalf("saved pair directories = %v, want exactly one", matches)
	}
	if err := os.RemoveAll(matches[0]); err != nil {
		s.t.Fatal(err)
	}
}

// plantOutsideRoots points the stored row at a path no configured root holds.
func (s *scopeSandbox) plantOutsideRoots() {
	s.t.Helper()
	db, err := openPreparedStore(s.t, s.dbPath())
	if err != nil {
		s.t.Fatalf("open the seeded store: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		s.t.Fatalf("take a connection: %v", err)
	}
	defer db.Pool().Put(conn)
	path := "/outside/roots/" + scopeTestSessionID + ".jsonl"
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET source_path = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{path, scopeTestSessionID}}); err != nil {
		s.t.Fatalf("plant the out-of-roots source path: %v", err)
	}
}

// writeBrokenOpenCodeDatabase makes an enabled OpenCode source whose database
// cannot be read, so a by-name lookup fails.
func (s *scopeSandbox) writeBrokenOpenCodeDatabase() string {
	s.t.Helper()
	root := filepath.Join(s.root, string(ingest.HarnessOpenCode))
	if err := os.MkdirAll(root, 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "opencode.db"), []byte("not a database\n"), 0o600); err != nil {
		s.t.Fatal(err)
	}
	return root
}

func (s *scopeSandbox) applySetup(tc scopeGoldenCase) {
	s.t.Helper()
	switch tc.Setup {
	case "empty":
		s.writeConfig(true, false, "")
	case "lookup_failed":
		s.writeConfig(false, true, s.writeBrokenOpenCodeDatabase())
	case "stored":
		s.writeConfig(true, false, "")
		s.writeSession(false)
		s.harvest()
	case "stored_old":
		s.writeConfig(true, false, "")
		s.writeSession(false)
		old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(s.rootTranscript(), old, old); err != nil {
			s.t.Fatal(err)
		}
		s.harvest()
	case "stored_gone":
		s.writeConfig(true, false, "")
		s.writeSession(false)
		s.harvest()
		s.deleteSource(false)
	case "stored_gone_child":
		s.writeConfig(true, false, "")
		s.writeSession(true)
		s.harvest()
		s.deleteSource(true)
	case "stored_disabled":
		s.writeConfig(true, false, "")
		s.writeSession(false)
		s.harvest()
		s.writeConfig(false, false, "")
	case "outside_roots":
		s.writeConfig(true, false, "")
		s.writeSession(false)
		s.harvest()
		s.plantOutsideRoots()
	case "index_pair_gone":
		s.writeConfig(true, false, "")
		s.writeSession(false)
		s.harvest()
		s.deleteSavedPair()
	case "logs_pair":
		s.writeConfig(true, false, "")
		s.writeSession(false)
		s.harvestLogs()
	case "logs_pair_child":
		s.writeConfig(true, false, "")
		s.writeSession(true)
		s.harvestLogs()
	case "stored_child_changed":
		s.writeConfig(true, false, "")
		s.writeSession(true)
		s.harvest()
		s.appendChildTurn()
	default:
		s.t.Fatalf("case %q names an unknown setup %q", tc.Name, tc.Setup)
	}
}

// commandRoot builds the production root for one invocation, with this
// sandbox's directories passed as flags.
func (s *scopeSandbox) commandRoot(sub *cobra.Command) *cobra.Command {
	root := newTestRoot()
	root.AddCommand(sub)
	root.SetArgs(nil)
	return root
}

func (s *scopeSandbox) dirFlags() []string {
	return []string{
		"--config", s.configPath(),
		"--config-dir", s.configDir(),
		"--data-dir", s.dataDir(),
		"--state-dir", s.stateDir(),
	}
}

func (s *scopeSandbox) executeHarvest(t *testing.T, filesystem ingest.FileSystem, subcommand []string, args ...string) (string, string, error) {
	t.Helper()
	root := s.commandRoot(buildHarvestCommand(filesystem))
	var stdout, stderr strings.Builder
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	argv := append([]string{"harvest"}, subcommand...)
	argv = append(argv, s.dirFlags()...)
	argv = append(argv, args...)
	root.SetArgs(argv)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func (s *scopeSandbox) executeOpen(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	server := newScopeDashboard(t)
	port := server.port
	deps := openDependencies{
		startServer: func(webServerSpawn) (string, error) {
			t.Error("open started a server while the test dashboard answered")
			return "", nil
		},
		openBrowser:   func(string) error { return nil },
		client:        &http.Client{Timeout: defaults.ServerClientTimeout},
		readyWait:     time.Second,
		readyInterval: 10 * time.Millisecond,
	}
	root := s.commandRoot(buildOpenCommand(deps))
	var stdout, stderr strings.Builder
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append(append([]string{"open"}, s.dirFlags()...), append(args, "--port", fmt.Sprint(port))...))
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func (s *scopeSandbox) harvest() {
	s.t.Helper()
	if stdout, stderr, err := s.executeHarvest(s.t, &ingest.OSFileSystem{}, nil); err != nil {
		s.t.Fatalf("record the sandbox sessions: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
}

func (s *scopeSandbox) harvestLogs() {
	s.t.Helper()
	if _, stderr, err := s.executeHarvest(s.t, &ingest.OSFileSystem{}, []string{"logs"}); err != nil {
		s.t.Fatalf("extract the sandbox sessions: %v\n%s", err, stderr)
	}
}

func (s *scopeSandbox) expandArgs(tc scopeGoldenCase) []string {
	replacer := strings.NewReplacer(
		"{session}", scopeTestSessionID,
		"{child}", scopeTestChildID,
		"{unknown}", scopeTestUnknownID,
	)
	args := make([]string, len(tc.Args))
	for i, arg := range tc.Args {
		args[i] = replacer.Replace(arg)
	}
	return args
}

func (s *scopeSandbox) assertCase(t *testing.T, tc scopeGoldenCase) {
	t.Helper()
	args := s.expandArgs(tc)
	var stdout, stderr string
	var err error
	switch tc.Command {
	case "harvest":
		stdout, stderr, err = s.executeHarvest(t, &ingest.OSFileSystem{}, nil, args...)
	case "index":
		recorder := &scopeWalkRecorder{FileSystem: &ingest.OSFileSystem{}}
		stdout, stderr, err = s.executeHarvest(t, recorder, []string{"index"}, args...)
		if walks := recorder.walkedUnder(s.outputDir()); len(walks) != 0 {
			t.Errorf("a session-scoped index run walked the output root: %v", walks)
		}
	case "open":
		stdout, stderr, err = s.executeOpen(t, args...)
	default:
		t.Fatalf("case %q names an unknown command %q", tc.Name, tc.Command)
	}

	switch tc.Kind {
	case "miss_block", "miss_line":
		if err == nil || exitCodeFor(err) == defaults.ExitOK {
			t.Fatalf("a miss must exit non-zero; err = %v", err)
		}
		if stdout != "" {
			t.Errorf("a miss wrote to stdout: %q", stdout)
		}
		s.compareGolden(t, tc.Name, stderr)
	case "warning":
		if err != nil {
			t.Fatalf("an unreadable recorded session keeps the stored copy and exits 0: %v\n%s", err, stderr)
		}
		if !strings.Contains(stdout, "peasant harvest:") {
			t.Errorf("the summary is missing from stdout: %q", stdout)
		}
		if strings.Contains(stdout, "was not read again") {
			t.Errorf("the warning was printed on stdout: %q", stdout)
		}
		s.compareGolden(t, tc.Name, stderr)
	case "index_ok", "index_unstored_pair_ok":
		if err != nil {
			t.Fatalf("harvest index --session failed: %v\n%s", err, stderr)
		}
		if stderr != "" {
			t.Errorf("a readable index run warned: %q", stderr)
		}
		if !strings.Contains(stdout, "peasant harvest:") {
			t.Errorf("the summary is missing from stdout: %q", stdout)
		}
	case "related_updated":
		if err != nil {
			t.Fatalf("a related session must be processed by default: %v\n%s", err, stderr)
		}
		if stderr != "" {
			t.Errorf("a readable scoped run warned: %q", stderr)
		}
		if !strings.Contains(stdout, "   - 1     updated") {
			t.Errorf("the changed related session was not updated: %q", stdout)
		}
	case "related_excluded":
		if err != nil {
			t.Fatalf("--no-related must still record the named session: %v\n%s", err, stderr)
		}
		if stderr != "" {
			t.Errorf("a readable scoped run warned: %q", stderr)
		}
		if !strings.Contains(stdout, "   - 0     updated") {
			t.Errorf("--no-related still processed the related session: %q", stdout)
		}
	case "open_warning":
		if err != nil {
			t.Fatalf("open must open the stored copy and exit 0: %v\n%s", err, stderr)
		}
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "peasant: opened") || !strings.HasPrefix(lines[1], "http://localhost:") {
			t.Errorf("open changed its stdout contract: %q", stdout)
		}
		s.compareGolden(t, tc.Name, stderr)
	case "open_hook_warning":
		if err != nil {
			t.Fatalf("hook mode exits 0 on every outcome: %v\n%s", err, stderr)
		}
		if stderr != "" {
			t.Errorf("hook mode wrote to stderr: %q", stderr)
		}
		var response struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal([]byte(stdout), &response); err != nil {
			t.Fatalf("hook stdout is not the hook response: %v: %q", err, stdout)
		}
		lines := strings.Split(response.StopReason, "\n")
		if len(lines) != 3 {
			t.Fatalf("stopReason has %d lines, want the two success lines and the warning: %q", len(lines), response.StopReason)
		}
		s.compareGolden(t, tc.Name, lines[2]+"\n")
	default:
		t.Fatalf("case %q names an unknown kind %q", tc.Name, tc.Kind)
	}
}

var scopeHostSlugPattern = regexp.MustCompile(`__peasant-untracked__--[0-9a-f]{8}--workspace`)

// compareGolden pins one command's printed bytes. UPDATE_SCOPE_GOLDENS=1
// rewrites the file instead, so a deliberate text change is reviewed as a diff.
func (s *scopeSandbox) compareGolden(t *testing.T, name, got string) {
	t.Helper()
	got = strings.ReplaceAll(got, s.root, "/sandbox")
	// A saved pair's host directory carries an opaque hash of the source path,
	// which includes the sandbox root. Pin it so the golden is stable.
	got = scopeHostSlugPattern.ReplaceAllString(got, "__peasant-untracked__--host--workspace")
	path := filepath.Join("testdata", "session_scope_errors", name+".golden")
	if os.Getenv("UPDATE_SCOPE_GOLDENS") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("%s output =\n%s\nwant\n%s", name, got, want)
	}
}

// scopeDashboard is a fake Peasant web server that answers the two reads `open`
// makes: the health probe and the session-summaries lookup.
type scopeDashboard struct {
	port int
}

func newScopeDashboard(t *testing.T) scopeDashboard {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(defaults.RouteHealth.String(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(defaults.RouteSessionSummaries.String(), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]string{{"id": r.URL.Query().Get("ids")}},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	var port int
	if _, err := fmt.Sscanf(server.URL, "http://127.0.0.1:%d", &port); err != nil {
		t.Fatalf("parse the test dashboard address %q: %v", server.URL, err)
	}
	return scopeDashboard{port: port}
}

// TestSessionScopeNoRelatedMisuse pins that --no-related fails with what, why
// and a fix instead of being silently ignored outside a session-scoped run.
func TestSessionScopeNoRelatedMisuse(t *testing.T) {
	t.Parallel()
	fixture := loadScopeGoldenFixture(t)
	cases := []struct {
		name string
		args []string
	}{
		{name: "without a session", args: []string{"--no-related"}},
		{name: "with --all", args: []string{"--no-related", "--all", "--session", scopeTestSessionID}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sandbox := newScopeSandbox(t, fixture)
			sandbox.writeConfig(true, false, "")
			stdout, stderr, err := sandbox.executeHarvest(t, &ingest.OSFileSystem{}, nil, tc.args...)
			if err == nil || exitCodeFor(err) == defaults.ExitOK {
				t.Fatalf("--no-related outside a session-scoped run must fail; err = %v", err)
			}
			if stdout != "" {
				t.Errorf("a refusal wrote to stdout: %q", stdout)
			}
			message := err.Error()
			for _, part := range []string{"--no-related was given", "changes only a session-scoped run", "fix:", "add --session <id>, or remove --no-related"} {
				if !strings.Contains(message, part) {
					t.Errorf("refusal %q does not contain %q", message, part)
				}
			}
			if strings.Contains(stderr, "peasant: harvest failed") {
				t.Errorf("--no-related misuse printed a scope block: %q", stderr)
			}
		})
	}
}

// TestHarvestCmdHarnessAlias pins that --harness and --source-harness are the
// same flag value, neither deprecated.
func TestHarvestCmdHarnessAlias(t *testing.T) {
	t.Parallel()
	parse := func(flag, value string) string {
		var flags harvestFlags
		parser := &cobra.Command{Use: "harvest"}
		registerHarvestFlags(parser, &flags, harvestAll)
		if err := parser.ParseFlags([]string{flag, value}); err != nil {
			t.Fatalf("parse %s: %v", flag, err)
		}
		return flags.sourceHarness
	}
	codex := string(ingest.HarnessCodex)
	if got, want := parse("--harness", codex), codex; got != want {
		t.Errorf("--harness resolved to %q, want %q", got, want)
	}
	if got, want := parse("--source-harness", "cursor"), "cursor"; got != want {
		t.Errorf("--source-harness resolved to %q, want %q", got, want)
	}
}
