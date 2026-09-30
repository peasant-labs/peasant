package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
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
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
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
	// openTestReadyWait leaves a loaded machine room to answer a started
	// server's first probe; the never-ready case waits it out once.
	openTestReadyWait     = time.Second
	openTestReadyInterval = 10 * time.Millisecond
	openTestRemote        = "https://github.com/example-org/open-fixture.git"
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
	Name                    string   `yaml:"name"`
	Session                 string   `yaml:"session"`
	Prompt                  string   `yaml:"prompt"`
	Transcript              string   `yaml:"transcript"`
	RecordedBefore          bool     `yaml:"recorded_before"`
	Published               bool     `yaml:"published"`
	ReattributeAfterPublish bool     `yaml:"reattribute_after_publish"`
	Config                  string   `yaml:"config"`
	Server                  string   `yaml:"server"`
	StartError              string   `yaml:"start_error"`
	Modes                   []string `yaml:"modes"`
	Want                    struct {
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
	for _, tc := range fixture.Cases {
		if len(tc.Modes) == 0 {
			t.Fatalf("case %q lists no modes, so it would run nothing", tc.Name)
		}
		seen := map[string]bool{}
		for _, mode := range tc.Modes {
			if (mode != "hook" && mode != "plain") || seen[mode] {
				t.Fatalf("case %q has an unknown or repeated mode %q", tc.Name, mode)
			}
			seen[mode] = true
		}
		if want := 2; !tc.Want.Failed && len(tc.Want.Lines) != want {
			t.Fatalf("success case %q holds %d lines, want %d", tc.Name, len(tc.Want.Lines), want)
		}
		if tc.Want.Failed && len(tc.Want.Lines) != 1 {
			t.Fatalf("failure case %q holds %d lines, want 1", tc.Name, len(tc.Want.Lines))
		}
	}
	return fixture
}

// TestOpenOutput drives `peasant open` through the production harvest, store,
// and output path for every fixture case and mode.
func TestOpenOutput(t *testing.T) {
	t.Parallel()
	fixture := loadOpenOutputFixture(t)
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
	if tc.ReattributeAfterPublish {
		world.git(t, nil, "remote", "add", "origin", openTestRemote)
	}

	stdout, stderr, err := world.open(t, mode)

	replacer := strings.NewReplacer(
		"{session_id}", world.sessionID(t),
		"{port}", strconv.Itoa(world.port),
		"{project_hash}", world.projectHash(t),
		"{peasant}", world.commandPrefix(),
	)
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
	if tc.Want.Started {
		_, statErr := os.Stat(world.pidFile)
		switch {
		case tc.Want.Failed && !errors.Is(statErr, os.ErrNotExist):
			t.Errorf("a failed start left its PID file behind: %v", statErr)
		case !tc.Want.Failed && statErr != nil:
			t.Errorf("a successful start lost its PID file: %v", statErr)
		}
	}

	recorded := world.recordedCommits(t)
	if tc.Want.Commits {
		if !slices.Equal(recorded, world.commits) {
			t.Errorf("session_commits = %v, want the session commits %v", recorded, world.commits)
		}
	} else if len(recorded) != 0 {
		t.Errorf("session_commits = %v, want none", recorded)
	}
}

// TestOpenMutesAndRestoresTheDefaultLoggers checks that the harvest's
// structured logs cannot reach the terminal while open runs, and that the
// process loggers are back afterwards. It runs outside the parallel phase,
// because it owns the process-wide loggers while it runs.
func TestOpenMutesAndRestoresTheDefaultLoggers(t *testing.T) {
	fixture := loadOpenOutputFixture(t)
	index := slices.IndexFunc(fixture.Cases, func(tc openOutputCase) bool { return tc.Name == "plain-mode" })
	world := newOpenWorld(t, fixture.Cases[index])

	var logged bytes.Buffer
	recorder := slog.New(slog.NewTextHandler(&logged, nil))
	previous, previousWriter, previousFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(recorder)
	log.SetOutput(&logged)
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	muted := false
	world.duringBrowserOpen = func() {
		muted = slog.Default() != recorder && log.Writer() != &logged
	}
	if _, stderr, err := world.open(t, "plain"); err != nil {
		t.Fatalf("open failed: %v; stderr=%q", err, stderr)
	}
	if !muted {
		t.Error("the default loggers were live while open ran")
	}
	if slog.Default() != recorder || log.Writer() != &logged {
		t.Error("open did not restore the default loggers")
	}
	if logged.Len() != 0 {
		t.Errorf("open wrote to the default loggers: %q", logged.String())
	}
}

// openWorld is one isolated install: config, data, and state directories, a
// Claude Code source, a Git repository, and a local server the health probe
// reaches.
type openWorld struct {
	tc                openOutputCase
	root              string
	repo              string
	configPath        string
	pidFile           string
	port              int
	commits           []string
	healthy           atomic.Bool
	serves            atomic.Bool
	started           atomic.Bool
	duringBrowserOpen func()
	mu                sync.Mutex
	opened            []string
}

func newOpenWorld(t *testing.T, tc openOutputCase) *openWorld {
	t.Helper()
	w := &openWorld{tc: tc, root: t.TempDir()}
	w.repo = filepath.Join(w.root, "repo")
	w.pidFile = filepath.Join(w.root, "state", "web.pid")
	w.commits = w.createRepository(t)

	sourceRoot := filepath.Join(w.root, "transcripts")
	transcript := filepath.Join(sourceRoot, "project", openTestSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, openTestTranscript(t, w.repo, tc.Prompt, tc.Transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	paths := "[" + sourceRoot + "]"
	redaction := ""
	switch tc.Config {
	case "valid":
	case "invalid-source-path":
		// A relative path is invalid; the harvest warns about it and goes on.
		paths = "[" + sourceRoot + ", relative/transcripts]"
	case "refused-redaction-level":
		redaction = "redaction:\n  level: maximum\n"
	default:
		t.Fatalf("unknown config %q", tc.Config)
	}
	config := "version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths: " + paths + "\n" +
		"  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\n" +
		"output:\n  basePath: " + filepath.Join(w.root, "output") + "\n" + redaction
	w.configPath = writeCfg(t, filepath.Join(w.root, "config"), "config.yaml", config)

	// A migrated empty database, so a run does not pay for the migrations.
	dbPath := string(defaults.ResolveDBFilePathWith(w.dataDir()))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	storetest.CopyGoldenTo(t, dbPath)

	switch tc.Server {
	case "running":
		w.healthy.Store(true)
		w.serves.Store(true)
	case "other-data":
		w.healthy.Store(true)
	case "starts", "start-error", "never-ready":
	default:
		t.Fatalf("unknown server %q", tc.Server)
	}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !w.healthy.Load() {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case defaults.RouteHealth.String():
			rw.WriteHeader(http.StatusOK)
		case defaults.RouteSessionSummaries.String():
			summaries := []schema.SessionSummary{}
			if id := r.URL.Query().Get("ids"); w.serves.Load() && id == openTestSessionID {
				summaries = append(summaries, schema.SessionSummary{ID: id})
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{"sessions": summaries})
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
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

func (w *openWorld) dataDir() string   { return filepath.Join(w.root, "data") }
func (w *openWorld) configDir() string { return filepath.Join(w.root, "config") }
func (w *openWorld) stateDir() string  { return filepath.Join(w.root, "state") }

// commandPrefix is how the command renders `peasant` with this run's flags.
func (w *openWorld) commandPrefix() string {
	return githooks.CommandPrefix(githooks.Binding{ConfigPath: w.configPath, ConfigDir: w.configDir(), DataDir: w.dataDir(), StateDir: w.stateDir()})
}

func (w *openWorld) dependencies(t *testing.T) openDependencies {
	return openDependencies{
		startServer: func(spawn webServerSpawn) (string, error) {
			if spawn.port != w.port || spawn.configPath != w.configPath || spawn.dataDir != w.dataDir() || spawn.configDir != w.configDir() || spawn.stateDir != w.stateDir() {
				t.Errorf("the forked server would not read this run's store: %+v", spawn)
			}
			switch w.tc.Server {
			case "running", "other-data":
				t.Error("started a server while one was running")
			case "start-error":
				return "", errors.New(w.tc.StartError)
			}
			if err := os.MkdirAll(filepath.Dir(w.pidFile), 0o700); err != nil {
				return "", err
			}
			if err := os.WriteFile(w.pidFile, []byte("1"), 0o600); err != nil {
				return "", err
			}
			w.started.Store(true)
			if w.tc.Server == "starts" {
				w.serves.Store(true)
				w.healthy.Store(true)
			}
			return w.pidFile, nil
		},
		openBrowser: func(address string) error {
			if !w.healthy.Load() || !w.serves.Load() {
				t.Error("opened the browser before the dashboard served the session")
			}
			if w.duringBrowserOpen != nil {
				w.duringBrowserOpen()
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			w.opened = append(w.opened, address)
			return nil
		},
		client:        &http.Client{Timeout: defaults.ServerClientTimeout},
		readyWait:     openTestReadyWait,
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
		"--data-dir", w.dataDir(),
		"--config-dir", w.configDir(),
		"--state-dir", w.stateDir(),
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
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(w.dataDir())))
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

func (w *openWorld) git(t *testing.T, env []string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", w.repo}, args...)...)
	command.Env = append(os.Environ(), env...)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// createRepository creates a repository with a base commit outside the
// session window and commits inside it, and returns the session commits'
// hashes, sorted.
func (w *openWorld) createRepository(t *testing.T) []string {
	t.Helper()
	if err := os.MkdirAll(w.repo, 0o700); err != nil {
		t.Fatal(err)
	}
	w.git(t, nil, "init", "-q", "-b", "main")
	w.git(t, nil, "config", "user.email", testutil.TestEmail)
	w.git(t, nil, "config", "user.name", "Test User")
	w.git(t, nil, "config", "commit.gpgsign", "false")
	commit := func(message, date string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(w.repo, "work.txt"), []byte(message+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w.git(t, nil, "add", "work.txt")
		w.git(t, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "commit", "-q", "-m", message)
		return w.git(t, nil, "rev-parse", "HEAD")
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
// shows Git activity. A malformed-record transcript breaks its second record.
func openTestTranscript(t *testing.T, repo, prompt, shape string) []byte {
	t.Helper()
	const model = "claude-sonnet-4-20250514"
	records := []map[string]any{
		{"type": "user", "sessionId": openTestSessionID, "cwd": repo, "timestamp": "2024-01-02T03:12:00Z", "message": map[string]any{"role": "user", "content": prompt}},
		{"type": "assistant", "sessionId": openTestSessionID, "cwd": repo, "timestamp": "2024-01-02T03:17:00Z", "message": map[string]any{"role": "assistant", "model": model, "content": []map[string]any{{"type": "tool_use", "id": "toolu_open_commit", "name": "Bash", "input": map[string]any{"command": "git commit -am 'change 2'"}}}}},
		{"type": "user", "sessionId": openTestSessionID, "cwd": repo, "timestamp": "2024-01-02T03:18:00Z", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_open_commit", "content": "[main] change 2"}}}},
		{"type": "assistant", "sessionId": openTestSessionID, "cwd": repo, "timestamp": "2024-01-02T03:20:00Z", "message": map[string]any{"role": "assistant", "model": model, "content": []map[string]any{{"type": "text", "text": "Committed."}}}},
	}
	var out bytes.Buffer
	for i, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case shape == "malformed-record" && i == 1:
			out.WriteString(`{"type":"assistant","sessionId":"` + openTestSessionID + `",`)
		case shape == "valid" || shape == "malformed-record":
			out.Write(line)
		default:
			t.Fatalf("unknown transcript %q", shape)
		}
		out.WriteByte('\n')
	}
	return out.Bytes()
}
