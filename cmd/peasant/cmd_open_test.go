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
	"net"
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

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
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
	// openTestMalformedSessionArg is a non-empty --session value that is not
	// a session id.
	openTestMalformedSessionArg = "not/a-session"
	// openTestForkedPID is the PID the replaced server start reports. The
	// replaced stop never signals it.
	openTestForkedPID = 4242
	// openTestStopError is what the replaced stop returns for a server that
	// cannot be stopped.
	openTestStopError = "operation not permitted"
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

// openOutputMode is how one open-output case invokes peasant open.
// It is this fixture's vocabulary, not a store-entry or progress mode.
type openOutputMode string

const (
	openOutputHook  openOutputMode = "hook"
	openOutputPlain openOutputMode = "plain"
)

// openServerState is the synthetic dashboard the open-output fixture starts from.
type openServerState string

const (
	openServerRunning          openServerState = "running"
	openServerOtherData        openServerState = "other-data"
	openServerSummariesError   openServerState = "summaries-error"
	openServerStarts           openServerState = "starts"
	openServerStartError       openServerState = "start-error"
	openServerNeverReady       openServerState = "never-ready"
	openServerNeverReadyNoStop openServerState = "never-ready-unstoppable"
)

// openDashboardKind is which server answers the dashboard port in an
// open-output case.
type openDashboardKind string

const (
	// openDashboardFake is a local test server that answers the health and
	// summaries routes from the case's server state.
	openDashboardFake openDashboardKind = "fake"
	// openDashboardAPI is a real in-process api.NewServer.
	openDashboardAPI openDashboardKind = "api"
)

// openSessionRef is which session argument an open-output case passes.
type openSessionRef string

const (
	openSessionKnown     openSessionRef = "known"
	openSessionUnknown   openSessionRef = "unknown"
	openSessionMissing   openSessionRef = "missing"
	openSessionMalformed openSessionRef = "malformed"
)

type openOutputCase struct {
	Name                    string            `yaml:"name"`
	Session                 openSessionRef    `yaml:"session"`
	Dashboard               openDashboardKind `yaml:"dashboard"`
	Prompt                  string            `yaml:"prompt"`
	Transcript              string            `yaml:"transcript"`
	RecordedBefore          bool              `yaml:"recorded_before"`
	Published               bool              `yaml:"published"`
	ReattributeAfterPublish bool              `yaml:"reattribute_after_publish"`
	Config                  string            `yaml:"config"`
	Server                  openServerState   `yaml:"server"`
	StartError              string            `yaml:"start_error"`
	Modes                   []openOutputMode  `yaml:"modes"`
	Want                    struct {
		Failed      bool     `yaml:"failed"`
		Lines       []string `yaml:"lines"`
		HookStdout  string   `yaml:"hook_stdout"`
		PlainStdout string   `yaml:"plain_stdout"`
		Commits     bool     `yaml:"commits"`
		Started     bool     `yaml:"started"`
		Stopped     bool     `yaml:"stopped"`
		PIDFileKept bool     `yaml:"pid_file_kept"`
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
		seen := map[openOutputMode]bool{}
		for _, mode := range tc.Modes {
			if (mode != openOutputHook && mode != openOutputPlain) || seen[mode] {
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
		switch tc.Dashboard {
		case openDashboardFake:
			if tc.Server == openServerSummariesError {
				t.Fatalf("case %q plays %q on a fake dashboard; only a real one fails its own summaries route", tc.Name, tc.Server)
			}
		case openDashboardAPI:
			// A real server is already running and cannot be started or made
			// to fail its start on demand.
			if !slices.Contains([]openServerState{openServerRunning, openServerOtherData, openServerSummariesError}, tc.Server) {
				t.Fatalf("case %q runs a real dashboard in server state %q, which only a fake dashboard can play", tc.Name, tc.Server)
			}
		default:
			t.Fatalf("case %q has an unknown dashboard %q", tc.Name, tc.Dashboard)
		}
		if (tc.Want.Stopped || tc.Want.PIDFileKept) && !tc.Want.Started {
			t.Fatalf("case %q expects a stop or a kept PID file without a start", tc.Name)
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
			t.Run(tc.Name+"/"+string(mode), func(t *testing.T) {
				t.Parallel()
				runOpenOutputCase(t, fixture, tc, mode)
			})
		}
	}
}

func runOpenOutputCase(t *testing.T, fixture openOutputFixture, tc openOutputCase, mode openOutputMode) {
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
	case openOutputHook:
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
		if len(fields) != 2 || string(fields["continue"]) != strconv.FormatBool(false) || fields["stopReason"] == nil {
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
	case openOutputPlain:
		if tc.Want.Failed {
			var failed *openFailedError
			if !errors.As(err, &failed) || exitCodeFor(err) != defaults.ExitFailure {
				t.Fatalf("a plain-mode failure must exit %d, got %v (exit %d)", defaults.ExitFailure, err, exitCodeFor(err))
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
	if stopped := world.stopped.Load(); stopped != tc.Want.Stopped {
		t.Errorf("server stopped = %v, want %v", stopped, tc.Want.Stopped)
	}
	if !world.muteBalanced() {
		t.Error("open did not mute the loggers exactly once and restore them")
	}
	if tc.Want.Started {
		_, statErr := os.Stat(world.pidFile)
		switch {
		case tc.Want.PIDFileKept && statErr != nil:
			t.Errorf("a server that could not be stopped lost its PID file: %v", statErr)
		case tc.Want.Failed && !tc.Want.PIDFileKept && !errors.Is(statErr, os.ErrNotExist):
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

// TestOpenHookFlagErrorExitsFailure pins the documented exception to hook
// mode's exit 0: a flag or argument error stops the command before it runs,
// so it exits 1 with Cobra's usage on stderr and no hook response.
func TestOpenHookFlagErrorExitsFailure(t *testing.T) {
	t.Parallel()
	fixture := loadOpenOutputFixture(t)
	index := slices.IndexFunc(fixture.Cases, func(tc openOutputCase) bool { return tc.Name == "hook-mode" })
	world := newOpenWorld(t, fixture.Cases[index])

	stdout, stderr, err := world.execute(t, buildOpenCommand(world.dependencies(t)),
		"open", "--session", openTestSessionID, "--port", strconv.Itoa(world.port), "--hook", "extra-argument")
	if err == nil || exitCodeFor(err) != defaults.ExitFailure {
		t.Fatalf("an extra argument in hook mode must exit %d, got %v", defaults.ExitFailure, err)
	}
	// Cobra prints the usage to the command's output writer, which is stderr
	// unless set, as here, to a buffer; the error itself goes to stderr.
	if strings.Contains(stdout+stderr, `"continue"`) {
		t.Errorf("a flag error printed a hook response: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "Usage:") || !strings.Contains(stderr, "extra-argument") {
		t.Errorf("a flag error must print the error and the usage: stdout=%q stderr=%q", stdout, stderr)
	}
	if world.mutes.Load() != 0 || len(world.openedURLs()) != 0 || world.started.Load() {
		t.Error("open ran despite the argument error")
	}
}

// TestOpenMutesAndRestoresTheDefaultLoggers checks the production mute: the
// harvest's structured logs and anything else logged while open runs go
// nowhere, and the process loggers, with the log package's flags, are back
// afterwards. It runs outside the parallel phase, because it owns the
// process-wide loggers while it runs; the parallel TestOpenOutput runs inject
// a mute that touches no process state.
func TestOpenMutesAndRestoresTheDefaultLoggers(t *testing.T) {
	fixture := loadOpenOutputFixture(t)
	index := slices.IndexFunc(fixture.Cases, func(tc openOutputCase) bool { return tc.Name == "plain-mode" })
	world := newOpenWorld(t, fixture.Cases[index])
	world.muteLoggers = muteDefaultLoggers

	var logged bytes.Buffer
	recorder := slog.New(slog.NewTextHandler(&logged, nil))
	previous, previousWriter, previousFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(recorder)
	log.SetOutput(&logged)
	// Setting a slog default resets the log package's flags, so distinct
	// flags show whether the mute put them back.
	const loggerFlags = log.Lshortfile | log.Lmicroseconds
	log.SetFlags(loggerFlags)
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	muted := false
	world.duringBrowserOpen = func() {
		muted = slog.Default() != recorder && log.Writer() != &logged
		slog.Info("logged while open runs")
		log.Print("printed while open runs")
	}
	if _, stderr, err := world.open(t, openOutputPlain); err != nil {
		t.Fatalf("open failed: %v; stderr=%q", err, stderr)
	}
	if !muted {
		t.Error("the default loggers were live while open ran")
	}
	if slog.Default() != recorder || log.Writer() != &logged {
		t.Error("open did not restore the default loggers")
	}
	if got := log.Flags(); got != loggerFlags {
		t.Errorf("log flags after open = %d, want %d", got, loggerFlags)
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
	stopped           atomic.Bool
	duringBrowserOpen func()
	// muteLoggers replaces the mute the world injects, which only counts
	// calls and touches no process state.
	muteLoggers func() (restore func())
	mutes       atomic.Int32
	restores    atomic.Int32
	mu          sync.Mutex
	opened      []string
}

// keepOpenLoggers is the logger mute for parallel open runs that do not check
// it: it leaves the process-wide loggers alone, so no run swaps them under
// another.
func keepOpenLoggers() (restore func()) { return func() {} }

// muteBalanced reports whether the injected mute ran once and was restored
// once. A world whose mute was replaced does not count.
func (w *openWorld) muteBalanced() bool {
	return w.muteLoggers != nil || (w.mutes.Load() == 1 && w.restores.Load() == 1)
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
		"output:\n  basePath: " + w.outputDir() + "\n" + redaction
	w.configPath = writeCfg(t, filepath.Join(w.root, "config"), "config.yaml", config)

	// A migrated empty database, so a run does not pay for the migrations.
	dbPath := string(defaults.ResolveDBFilePathWith(w.dataDir()))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	storetest.CopyGoldenTo(t, dbPath)

	switch tc.Server {
	case openServerRunning:
		w.healthy.Store(true)
		w.serves.Store(true)
	case openServerOtherData, openServerSummariesError:
		w.healthy.Store(true)
	case openServerStarts, openServerStartError, openServerNeverReady, openServerNeverReadyNoStop:
	default:
		t.Fatalf("unknown server %q", tc.Server)
	}
	switch tc.Dashboard {
	case openDashboardAPI:
		w.port = w.startAPIDashboard(t, dbPath)
	default:
		w.port = w.startFakeDashboard(t)
	}
	return w
}

// startFakeDashboard serves the health and summaries routes from the world's
// server state and returns its port.
func (w *openWorld) startFakeDashboard(t *testing.T) int {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !w.healthy.Load() {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case defaults.RouteHealth.String():
			rw.WriteHeader(http.StatusOK)
		case defaults.RouteSessionSummaries.String():
			payload := schema.SessionsPayload{Sessions: []schema.SessionSummary{}}
			if id := r.URL.Query().Get("ids"); w.serves.Load() && id == openTestSessionID {
				payload.Sessions = append(payload.Sessions, schema.SessionSummary{ID: id})
			}
			_ = json.NewEncoder(rw).Encode(payload)
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(address.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// startAPIDashboard runs a real in-process api.NewServer and returns its port.
// A running dashboard reads this world's database; an other-data dashboard
// reads a separate empty one; a dashboard in the summaries-error state has no
// data provider, so its summaries route answers with an error.
func (w *openWorld) startAPIDashboard(t *testing.T, dbPath string) int {
	t.Helper()
	serverConfig := api.ServerConfig{Port: 0, Config: config.BaseConfig(), ConfigHome: t.TempDir()}
	switch w.tc.Server {
	case openServerRunning, openServerOtherData:
		if w.tc.Server == openServerOtherData {
			dbPath = filepath.Join(w.root, "other-data", "peasant.db")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatal(err)
			}
			storetest.CopyGoldenTo(t, dbPath)
		}
		// The production server opens the store with the managed-generation
		// options of the configured output tree, which is where the harvest
		// writes the session's transcript.
		outputDir := w.outputDir()
		db, err := openPreparedStore(t, dbPath, mustGenerationStoreOptions(t, outputDir)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		serverConfig.Store = db
		serverConfig.Provider = api.NewStoreDataProvider(db, sessionvisibility.Browsing(), outputDir)
	case openServerSummariesError:
	default:
		t.Fatalf("a real dashboard cannot play server state %q", w.tc.Server)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(serverConfig)
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	// Registered after the store's close, so it runs first: the server stops
	// before its store closes.
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("dashboard shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("dashboard did not stop after cancellation")
		}
	})
	address, ok := srv.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("dashboard listens on %v, not a TCP address", srv.Addr())
	}
	return address.Port
}

func (w *openWorld) dataDir() string   { return filepath.Join(w.root, "data") }
func (w *openWorld) configDir() string { return filepath.Join(w.root, "config") }
func (w *openWorld) stateDir() string  { return filepath.Join(w.root, "state") }
func (w *openWorld) outputDir() string { return filepath.Join(w.root, "output") }

// commandPrefix is how the command renders `peasant` with this run's flags.
func (w *openWorld) commandPrefix() string {
	return githooks.CommandPrefix(githooks.Binding{ConfigPath: w.configPath, ConfigDir: w.configDir(), DataDir: w.dataDir(), StateDir: w.stateDir()})
}

func (w *openWorld) dependencies(t *testing.T) openDependencies {
	muteLoggers := w.muteLoggers
	if muteLoggers == nil {
		muteLoggers = func() func() {
			w.mutes.Add(1)
			return func() { w.restores.Add(1) }
		}
	}
	return openDependencies{
		startServer: func(spawn webServerSpawn) (int, string, error) {
			if spawn.port != w.port || spawn.configPath != w.configPath || spawn.dataDir != w.dataDir() || spawn.configDir != w.configDir() || spawn.stateDir != w.stateDir() {
				t.Errorf("the forked server would not read this run's store: %+v", spawn)
			}
			switch w.tc.Server {
			case openServerRunning, openServerOtherData, openServerSummariesError:
				t.Error("started a server while one was running")
			case openServerStartError:
				return 0, "", errors.New(w.tc.StartError)
			}
			if err := os.MkdirAll(filepath.Dir(w.pidFile), 0o700); err != nil {
				return 0, "", err
			}
			if err := os.WriteFile(w.pidFile, []byte(strconv.Itoa(openTestForkedPID)), 0o600); err != nil {
				return 0, "", err
			}
			w.started.Store(true)
			if w.tc.Server == openServerStarts {
				w.serves.Store(true)
				w.healthy.Store(true)
			}
			return openTestForkedPID, w.pidFile, nil
		},
		stopServer: func(pid int) error {
			if pid != openTestForkedPID {
				t.Errorf("stopped PID %d, want the started server's PID %d", pid, openTestForkedPID)
			}
			// The PID file must still name the server while it is stopped, so a
			// failed stop leaves it for `peasant web stop`.
			if _, err := os.Stat(w.pidFile); err != nil {
				t.Errorf("the PID file was gone before the server was stopped: %v", err)
			}
			w.stopped.Store(true)
			if w.tc.Server == openServerNeverReadyNoStop {
				return errors.New(openTestStopError)
			}
			return nil
		},
		muteLoggers: muteLoggers,
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
	case openSessionKnown:
		return openTestSessionID
	case openSessionUnknown:
		return openTestUnknownSessionID
	case openSessionMissing:
		return ""
	case openSessionMalformed:
		return openTestMalformedSessionArg
	}
	t.Fatalf("unknown session %q", w.tc.Session)
	return ""
}

func (w *openWorld) open(t *testing.T, mode openOutputMode) (string, string, error) {
	t.Helper()
	args := []string{"open", "--session", w.sessionID(t), "--port", strconv.Itoa(w.port)}
	if mode == openOutputHook {
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
