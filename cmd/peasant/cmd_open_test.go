package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

//go:embed testdata/open-output.yaml
var openOutputYAML []byte

// openOutputAcceptanceCases are the case names the issue that introduced
// `peasant open` made binding. The fixture's own manifest must keep them.
var openOutputAcceptanceCases = []string{
	"new-session", "refreshed-session", "published-session", "title-with-quotes",
	"harvest-failure", "server-start-failure", "unknown-session", "hook-mode", "plain-mode",
}

const (
	openTestSessionID        = "5a1f0c3e-7b2d-4e8a-9c61-0d4b3e2f1a70"
	openTestUnknownSessionID = "6b2e1d4f-8c3e-4f9b-8d72-1e5c4f3a2b81"
	openTestReadyAttempts    = 3
	openTestReadyInterval    = 10 * time.Millisecond
)

// The session runs 03:12 to 03:20 on 2024-01-02. The base commit falls weeks
// before it, outside the detection window; the session commits fall inside.
const openTestBaseCommitDate = "2023-12-01T09:00:00Z"

var openTestSessionCommitDates = []string{"2024-01-02T03:14:00Z", "2024-01-02T03:18:00Z"}

type openOutputFixture struct {
	RequiredNames []string               `yaml:"requiredNames"`
	Publication   openFixturePublication `yaml:"publication"`
	Cases         []openOutputCase       `yaml:"cases"`
}

type openFixturePublication struct {
	Origin      string `yaml:"origin"`
	Owner       string `yaml:"owner"`
	ReceiptJSON string `yaml:"receipt_json"`
}

type openOutputCase struct {
	Name           string   `yaml:"name"`
	Session        string   `yaml:"session"`
	Prompt         string   `yaml:"prompt"`
	RecordedBefore bool     `yaml:"recorded_before"`
	Published      bool     `yaml:"published"`
	Config         string   `yaml:"config"`
	Server         string   `yaml:"server"`
	StartError     string   `yaml:"start_error"`
	Modes          []string `yaml:"modes"`
	Want           struct {
		Failed      bool     `yaml:"failed"`
		Lines       []string `yaml:"lines"`
		HookStdout  string   `yaml:"hook_stdout"`
		PlainStdout string   `yaml:"plain_stdout"`
		Commits     bool     `yaml:"commits"`
		Started     bool     `yaml:"started"`
	} `yaml:"want"`
}

func loadOpenOutputFixture(t *testing.T) openOutputFixture {
	t.Helper()
	var fixture openOutputFixture
	if err := testutil.DecodeNamedFixtureYAML(openOutputYAML, &fixture); err != nil {
		t.Fatalf("decode open output fixture: %v", err)
	}
	for _, name := range openOutputAcceptanceCases {
		if !slices.Contains(fixture.RequiredNames, name) {
			t.Fatalf("open output fixture manifest dropped the acceptance case %q", name)
		}
	}
	return fixture
}

// TestOpenOutput drives `peasant open` through the production harvest, store,
// and output path for every fixture case and mode.
//
// Commit detection attributes commits by the global Git user email, so the test
// points Git's global configuration at a file that sets it. That makes the
// test itself sequential; its runs are parallel with each other.
func TestOpenOutput(t *testing.T) {
	fixture := loadOpenOutputFixture(t)
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[user]\n\temail = "+testutil.TestEmail+"\n\tname = Test User\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	for _, tc := range fixture.Cases {
		for _, mode := range tc.Modes {
			t.Run(tc.Name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				runOpenOutputCase(t, fixture, tc, mode)
			})
		}
	}
}

func runOpenOutputCase(t *testing.T, fixture openOutputFixture, tc openOutputCase, mode string) {
	world := newOpenWorld(t, tc)
	if tc.RecordedBefore {
		world.harvest(t)
	}
	if tc.Published {
		world.publish(t, fixture.Publication)
	}

	stdout, stderr, err := world.open(t, mode)

	sessionID := world.sessionID(t)
	replacer := strings.NewReplacer("{session_id}", sessionID, "{port}", strconv.Itoa(world.port), "{project_hash}", world.projectHash(t))
	want := make([]string, len(tc.Want.Lines))
	for i, line := range tc.Want.Lines {
		want[i] = replacer.Replace(line)
	}

	var got []string
	switch mode {
	case "hook":
		if err != nil {
			t.Fatalf("hook mode must exit 0 on every outcome: %v", err)
		}
		if stderr != "" {
			t.Fatalf("hook mode wrote to stderr: %q", stderr)
		}
		if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
			t.Fatalf("hook stdout must be exactly one JSON line: %q", stdout)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(stdout), &fields); err != nil {
			t.Fatalf("hook stdout is not valid JSON: %v: %q", err, stdout)
		}
		if len(fields) != 2 || string(fields["continue"]) != "false" || fields["stopReason"] == nil {
			t.Fatalf("hook response = %s, want exactly continue:false and a stopReason", stdout)
		}
		var reason string
		if err := json.Unmarshal(fields["stopReason"], &reason); err != nil {
			t.Fatalf("stopReason is not a string: %v", err)
		}
		got = strings.Split(reason, "\n")
		if tc.Want.HookStdout != "" {
			if wantRaw := replacer.Replace(tc.Want.HookStdout) + "\n"; stdout != wantRaw {
				t.Errorf("hook stdout =\n%s\nwant\n%s", stdout, wantRaw)
			}
		}
	case "plain":
		if tc.Want.Failed {
			var failed *openFailedError
			if !errors.As(err, &failed) || exitCodeFor(err) == defaults.ExitOK {
				t.Fatalf("a plain-mode failure must exit non-zero, got %v", err)
			}
			if stdout != "" {
				t.Fatalf("a plain-mode failure wrote to stdout: %q", stdout)
			}
			got = strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
		} else {
			if err != nil {
				t.Fatalf("open failed: %v; stderr=%q", err, stderr)
			}
			if stderr != "" {
				t.Fatalf("a plain-mode success wrote to stderr: %q", stderr)
			}
			got = strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		}
		if tc.Want.PlainStdout != "" {
			if wantRaw := replacer.Replace(tc.Want.PlainStdout); stdout != wantRaw {
				t.Errorf("plain stdout =\n%s\nwant\n%s", stdout, wantRaw)
			}
		}
	default:
		t.Fatalf("unknown mode %q", mode)
	}

	if len(got) > 2 {
		t.Errorf("printed %d lines, want at most two: %q", len(got), got)
	}
	root := dashboardBaseURL(world.port)
	for _, line := range got {
		if strings.TrimSuffix(strings.TrimSpace(line), "/") == root {
			t.Errorf("printed the dashboard root %q", line)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	opened := world.openedURLs()
	switch {
	case tc.Want.Failed && len(opened) != 0:
		t.Errorf("a failed run opened %q in the browser", opened)
	case !tc.Want.Failed && !slices.Equal(opened, want[1:]):
		t.Errorf("browser opened %q, want exactly the transcript %q", opened, want[1:])
	}
	if started := world.started.Load(); started != tc.Want.Started {
		t.Errorf("server started = %v, want %v", started, tc.Want.Started)
	}

	recorded := world.recordedCommits(t)
	if tc.Want.Commits {
		if !slices.Equal(recorded, world.commits) {
			t.Errorf("session_commits = %v, want the repository's commits %v", recorded, world.commits)
		}
	} else if len(recorded) != 0 {
		t.Errorf("session_commits = %v, want none", recorded)
	}
}

// openWorld is one isolated install: config, data, and state directories, a
// Claude Code source, a Git repository, and a local server the health probe
// reaches.
type openWorld struct {
	tc         openOutputCase
	root       string
	configPath string
	port       int
	commits    []string
	healthy    atomic.Bool
	started    atomic.Bool
	mu         sync.Mutex
	opened     []string
}

func newOpenWorld(t *testing.T, tc openOutputCase) *openWorld {
	t.Helper()
	w := &openWorld{tc: tc, root: t.TempDir()}
	repo := filepath.Join(w.root, "repo")
	w.commits = openTestRepository(t, repo)

	sourceRoot := filepath.Join(w.root, "transcripts")
	transcript := filepath.Join(sourceRoot, "project", openTestSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, openTestTranscript(t, repo, tc.Prompt), 0o600); err != nil {
		t.Fatal(err)
	}

	config := "version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths: [" + sourceRoot + "]\n" +
		"  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\n" +
		"output:\n  basePath: " + filepath.Join(w.root, "output") + "\n"
	switch tc.Config {
	case "valid":
	case "refused-redaction-level":
		config += "redaction:\n  level: maximum\n"
	default:
		t.Fatalf("unknown config %q", tc.Config)
	}
	w.configPath = writeCfg(t, filepath.Join(w.root, "config"), "config.yaml", config)

	switch tc.Server {
	case "running":
		w.healthy.Store(true)
	case "starts", "start-error", "never-ready":
	default:
		t.Fatalf("unknown server %q", tc.Server)
	}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == defaults.RouteHealth.String() && w.healthy.Load() {
			rw.WriteHeader(http.StatusOK)
			return
		}
		rw.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if w.port, err = strconv.Atoi(address.Port()); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *openWorld) dependencies(t *testing.T) openDependencies {
	return openDependencies{
		startServer: func(_ *cobra.Command, port int) error {
			if port != w.port {
				t.Errorf("started a server on port %d, want %d", port, w.port)
			}
			switch w.tc.Server {
			case "running":
				t.Error("started a server while one was running")
			case "starts":
				w.healthy.Store(true)
			case "start-error":
				return errors.New(w.tc.StartError)
			}
			w.started.Store(true)
			return nil
		},
		openBrowser: func(address string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.opened = append(w.opened, address)
			return nil
		},
		health:        &http.Client{Timeout: defaults.ServerClientTimeout},
		readyAttempts: openTestReadyAttempts,
		readyInterval: openTestReadyInterval,
	}
}

func (w *openWorld) execute(t *testing.T, sub *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(sub)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{
		"--data-dir", filepath.Join(w.root, "data"),
		"--config-dir", filepath.Join(w.root, "config"),
		"--state-dir", filepath.Join(w.root, "state"),
		"--config", w.configPath,
	}, args...))
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

// sessionID is the --session value the case passes.
func (w *openWorld) sessionID(t *testing.T) string {
	t.Helper()
	switch w.tc.Session {
	case "known":
		return openTestSessionID
	case "unknown":
		return openTestUnknownSessionID
	case "missing":
		return ""
	}
	t.Fatalf("unknown session %q", w.tc.Session)
	return ""
}

func (w *openWorld) open(t *testing.T, mode string) (string, string, error) {
	t.Helper()
	args := []string{"open", "--session", w.sessionID(t), "--port", strconv.Itoa(w.port)}
	if mode == "hook" {
		args = append(args, "--hook")
	}
	return w.execute(t, buildOpenCommand(w.dependencies(t)), args...)
}

// harvest records the session before the measured run, as an earlier harvest
// or an earlier open would have.
func (w *openWorld) harvest(t *testing.T) {
	t.Helper()
	if stdout, stderr, err := w.execute(t, BuildHarvestCommand(), "harvest", "--session", openTestSessionID, "--force"); err != nil {
		t.Fatalf("record the session first: %v\n%s%s", err, stdout, stderr)
	}
}

func (w *openWorld) store(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(filepath.Join(w.root, "data"))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (w *openWorld) publish(t *testing.T, publication openFixturePublication) {
	t.Helper()
	receipt, err := schema.DecodePublishResponse([]byte(publication.ReceiptJSON))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := schema.NewProjectHash(w.projectHash(t))
	if err != nil {
		t.Fatal(err)
	}
	db := w.store(t)
	if err := db.SavePublication(context.Background(), store.PublicationRecord{VillageOrigin: publication.Origin, OwnerUserID: publication.Owner, SessionID: openTestSessionID, ProjectHash: hash, Receipt: receipt}); err != nil {
		t.Fatalf("store the publication receipt: %v", err)
	}
}

// projectHash is the project the store recorded the session under, or a
// placeholder no output may contain when the session was never recorded.
func (w *openWorld) projectHash(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(string(defaults.ResolveDBFilePathWith(filepath.Join(w.root, "data")))); err != nil {
		return "<not recorded>"
	}
	row, err := w.store(t).SessionByID(context.Background(), openTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return "<not recorded>"
	}
	return row.ProjectHash
}

// recordedCommits reads the session's current commit associations, which are
// exactly its session_commits rows.
func (w *openWorld) recordedCommits(t *testing.T) []string {
	t.Helper()
	if _, err := os.Stat(string(defaults.ResolveDBFilePathWith(filepath.Join(w.root, "data")))); err != nil {
		return nil
	}
	associations, err := w.store(t).ListCurrentSessionCommitAssociations(context.Background(), openTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make([]string, 0, len(associations))
	for _, association := range associations {
		hashes = append(hashes, association.ObservedCommitHash)
	}
	slices.Sort(hashes)
	return hashes
}

func (w *openWorld) openedURLs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.opened)
}

// openTestRepository creates a repository with a base commit outside the
// session window and commits inside it, and returns the session commits'
// hashes, sorted.
func openTestRepository(t *testing.T, dir string) []string {
	t.Helper()
	git := func(env []string, args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		command.Env = append(os.Environ(), env...)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	git(nil, "init", "-q", "-b", "main")
	git(nil, "config", "user.email", testutil.TestEmail)
	git(nil, "config", "user.name", "Test User")
	git(nil, "config", "commit.gpgsign", "false")
	commit := func(message, date string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte(message+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(nil, "add", "work.txt")
		git([]string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "commit", "-q", "-m", message)
		return git(nil, "rev-parse", "HEAD")
	}
	commit("base", openTestBaseCommitDate)
	hashes := make([]string, 0, len(openTestSessionCommitDates))
	for i, date := range openTestSessionCommitDates {
		hashes = append(hashes, commit(fmt.Sprintf("change %d", i+1), date))
	}
	slices.Sort(hashes)
	return hashes
}

// openTestTranscript is a Claude Code session in repo that commits once. Commit
// detection keeps the commits in the session window only when the transcript
// shows Git activity.
func openTestTranscript(t *testing.T, repo, prompt string) []byte {
	t.Helper()
	const model = "claude-sonnet-4-20250514"
	records := []map[string]any{
		{"type": "user", "sessionId": openTestSessionID, "cwd": repo, "timestamp": "2024-01-02T03:12:00Z", "message": map[string]any{"role": "user", "content": prompt}},
		{"type": "assistant", "sessionId": openTestSessionID, "cwd": repo, "timestamp": "2024-01-02T03:17:00Z", "message": map[string]any{"role": "assistant", "model": model, "content": []map[string]any{{"type": "tool_use", "id": "toolu_open_commit", "name": "Bash", "input": map[string]any{"command": "git commit -am 'change 2'"}}}}},
		{"type": "user", "sessionId": openTestSessionID, "cwd": repo, "timestamp": "2024-01-02T03:18:00Z", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_open_commit", "content": "[main] change 2"}}}},
		{"type": "assistant", "sessionId": openTestSessionID, "cwd": repo, "timestamp": "2024-01-02T03:20:00Z", "message": map[string]any{"role": "assistant", "model": model, "content": []map[string]any{{"type": "text", "text": "Committed."}}}},
	}
	var out bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
