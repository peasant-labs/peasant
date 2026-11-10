package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
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
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

//go:embed testdata/open_commit_refresh.yaml
var openCommitRefreshYAML []byte

type openCommitRefreshFixture struct {
	RequiredNames []string                `yaml:"requiredNames"`
	Cases         []openCommitRefreshCase `yaml:"cases"`
}

type openCommitRefreshCase struct {
	Name        string `yaml:"name"`
	AfterRecord string `yaml:"afterRecord"`
	Command     string `yaml:"command"`
	Want        struct {
		Succeeded    bool     `yaml:"succeeded"`
		Commits      string   `yaml:"commits"`
		Stored       string   `yaml:"stored"`
		StderrAbsent []string `yaml:"stderrAbsent"`
	} `yaml:"want"`
}

func loadOpenCommitRefreshFixture(t *testing.T) openCommitRefreshFixture {
	t.Helper()
	var fixture openCommitRefreshFixture
	if err := testutil.DecodeNamedFixtureYAML(openCommitRefreshYAML, &fixture); err != nil {
		t.Fatalf("decode commit refresh fixture: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if present[testCase.Name] {
			t.Fatalf("commit refresh fixture repeats case %q", testCase.Name)
		}
		present[testCase.Name] = true
		switch testCase.AfterRecord {
		case "none", "add-commit", "hide-git":
		default:
			t.Fatalf("case %q has unknown afterRecord %q", testCase.Name, testCase.AfterRecord)
		}
		switch testCase.Command {
		case "open", "harvest", "harvest-session", "harvest-refresh", "harvest-refresh-all", "harvest-force", "harvest-dry-run":
		default:
			t.Fatalf("case %q has unknown command %q", testCase.Name, testCase.Command)
		}
		switch testCase.Want.Commits {
		case "grew", "same-rows", "unchanged":
		default:
			t.Fatalf("case %q has unknown commits want %q", testCase.Name, testCase.Want.Commits)
		}
		switch testCase.Want.Stored {
		case "identical", "extracted":
		default:
			t.Fatalf("case %q has unknown stored want %q", testCase.Name, testCase.Want.Stored)
		}
	}
	if err := testutil.RequireFixtureNames("commit refresh fixture", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for name := range present {
		if !slices.Contains(fixture.RequiredNames, name) {
			t.Fatalf("commit refresh case %q is not in requiredNames; add it to the deletion guard", name)
		}
	}
	return fixture
}

// TestOpenCommitRefresh drives peasant open and peasant harvest, the commands
// a person runs, and checks that an unchanged named session refreshes commits
// without extraction or index work.
func TestOpenCommitRefresh(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCommitRefreshFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runOpenCommitRefreshCase(t, testCase)
		})
	}
}

func TestOpenHarvestDoesNotForce(t *testing.T) {
	t.Parallel()
	args := openHarvestArgs(openTestSessionID, false)
	if slices.Contains(args, "--force") {
		t.Fatalf("open harvest args = %v, want no --force; open must not re-extract an unchanged session", args)
	}
	if !slices.Contains(args, "--detect-commits") {
		t.Fatalf("open harvest args = %v, want --detect-commits", args)
	}
	if !slices.Contains(args, "--refresh-commits") {
		t.Fatalf("open harvest args = %v, want --refresh-commits", args)
	}
	if !slices.Contains(args, "--session") || !slices.Contains(args, openTestSessionID) {
		t.Fatalf("open harvest args = %v, want the named session", args)
	}
}

func runOpenCommitRefreshCase(t *testing.T, testCase openCommitRefreshCase) {
	t.Helper()
	world := newCommitRefreshWorld(t)
	if stdout, stderr, err := world.harvest(t, "--session", openTestSessionID); err != nil {
		t.Fatalf("record the session: %v\n%s%s", err, stdout, stderr)
	}
	before := world.snapshot(t)
	if before.entryCount == 0 {
		t.Fatal("the recorded session has no entries, so an unchanged-entries assertion would be vacuous")
	}
	if before.indexCount == 0 {
		t.Fatal("the recorded session has no index log row, so an unchanged-index assertion would be vacuous")
	}
	if before.artifact == "" {
		t.Fatal("the recorded session has no saved pair identity, so an unchanged-identity assertion would be vacuous")
	}
	if len(before.hashes) == 0 {
		t.Fatal("the recorded session has no commits, so a refresh assertion would be vacuous")
	}

	var added string
	switch testCase.AfterRecord {
	case "none":
	case "add-commit":
		added = world.addWindowCommit(t)
	case "hide-git":
		world.hideGit(t)
	}

	stdout, stderr, err := world.run(t, testCase.Command)
	if testCase.Want.Succeeded && err != nil {
		t.Fatalf("%s failed: %v\n%s%s", testCase.Command, err, stdout, stderr)
	}
	if !testCase.Want.Succeeded && err == nil {
		t.Fatalf("%s succeeded, want a failure\n%s%s", testCase.Command, stdout, stderr)
	}
	after := world.snapshot(t)
	for _, absent := range testCase.Want.StderrAbsent {
		if strings.Contains(stderr, absent) {
			t.Fatalf("%s wrote %q, which a run without commit refresh must not emit:\n%s", testCase.Command, absent, stderr)
		}
	}

	switch testCase.Want.Commits {
	case "grew":
		if added == "" {
			t.Fatal("grew requires a commit added after the recording harvest")
		}
		if !slices.Contains(after.hashes, added) {
			t.Fatalf("commits = %v, want the new commit %s among them", after.hashes, added)
		}
		for _, hash := range before.hashes {
			if !slices.Contains(after.hashes, hash) {
				t.Fatalf("commits = %v, lost the previously stored commit %s", after.hashes, hash)
			}
		}
	case "same-rows":
		if after.commitRows != before.commitRows {
			t.Fatalf("commit projection rows = %q, want %q unchanged; an identical detection must not rewrite the projection", after.commitRows, before.commitRows)
		}
		if !slices.Equal(after.hashes, before.hashes) {
			t.Fatalf("commits = %v, want %v", after.hashes, before.hashes)
		}
	case "unchanged":
		if !slices.Equal(after.hashes, before.hashes) {
			t.Fatalf("commits = %v, want %v", after.hashes, before.hashes)
		}
	}

	switch testCase.Want.Stored {
	case "identical":
		if after.entries != before.entries {
			t.Fatalf("entries changed:\nbefore %s\nafter  %s", before.entries, after.entries)
		}
		if after.indexLog != before.indexLog {
			t.Fatalf("index log changed:\nbefore %s\nafter  %s", before.indexLog, after.indexLog)
		}
		if after.identity != before.identity {
			t.Fatalf("session identity changed:\nbefore %s\nafter  %s", before.identity, after.identity)
		}
		if after.metadata != before.metadata {
			t.Fatal("the saved metadata file changed; an unchanged session must not be re-extracted")
		}
	case "extracted":
		if after.indexCount <= before.indexCount && after.identity == before.identity && after.metadata == before.metadata {
			t.Fatal("forced harvest left the index, the saved pair, and the metadata file untouched; --force must still re-extract and re-index")
		}
	}
}

type commitRefreshSnapshot struct {
	entries    string
	indexLog   string
	identity   string
	artifact   string
	metadata   string
	commitRows string
	hashes     []string
	entryCount int
	indexCount int
}

type commitRefreshWorld struct {
	root        string
	repo        string
	configPath  string
	port        int
	authorEmail string
}

func newCommitRefreshWorld(t *testing.T) *commitRefreshWorld {
	t.Helper()
	w := &commitRefreshWorld{root: t.TempDir(), authorEmail: detectionAuthorEmail(t)}
	w.repo = filepath.Join(w.root, "repo")
	w.createRepository(t)

	sourceRoot := filepath.Join(w.root, "transcripts")
	transcript := filepath.Join(sourceRoot, "project", openTestSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, openTestTranscript(t, w.repo, "Fix the login redirect", "valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths: [" + sourceRoot + "]\n" +
		"  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\n" +
		"output:\n  basePath: " + filepath.Join(w.root, "output") + "\n"
	w.configPath = writeCfg(t, filepath.Join(w.root, "config"), "config.yaml", config)

	dbPath := string(defaults.ResolveDBFilePathWith(w.dataDir()))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	storetest.CopyGoldenTo(t, dbPath)
	w.serveDashboard(t)
	return w
}

func detectionAuthorEmail(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "config", "--global", "user.email").Output()
	if err != nil {
		return testutil.TestEmail
	}
	if email := strings.TrimSpace(string(out)); email != "" {
		return email
	}
	return testutil.TestEmail
}

func (w *commitRefreshWorld) dataDir() string   { return filepath.Join(w.root, "data") }
func (w *commitRefreshWorld) configDir() string { return filepath.Join(w.root, "config") }
func (w *commitRefreshWorld) stateDir() string  { return filepath.Join(w.root, "state") }

func (w *commitRefreshWorld) createRepository(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(w.repo, 0o700); err != nil {
		t.Fatal(err)
	}
	w.git(t, nil, "init", "-q", "-b", "main")
	w.git(t, nil, "config", "user.email", w.authorEmail)
	w.git(t, nil, "config", "user.name", "Test User")
	w.git(t, nil, "config", "commit.gpgsign", "false")
	w.commitAt(t, "base", openTestBaseCommitDate)
	for i, date := range openTestSessionCommitDates {
		w.commitAt(t, fmt.Sprintf("change %d", i+1), date)
	}
}

func (w *commitRefreshWorld) commitAt(t *testing.T, message, date string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.repo, "work.txt"), []byte(message+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.git(t, nil, "add", "work.txt")
	w.git(t, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "commit", "-q", "-m", message)
	return w.git(t, nil, "rev-parse", "HEAD")
}

func (w *commitRefreshWorld) addWindowCommit(t *testing.T) string {
	t.Helper()
	return w.commitAt(t, "change 3", "2024-01-02T03:19:00Z")
}

func (w *commitRefreshWorld) hideGit(t *testing.T) {
	t.Helper()
	gitDir := filepath.Join(w.repo, ".git")
	if err := os.Chmod(gitDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o700) })
}

func (w *commitRefreshWorld) git(t *testing.T, env []string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", w.repo}, args...)...)
	command.Env = append(os.Environ(), env...)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *commitRefreshWorld) serveDashboard(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case defaults.RouteHealth.String():
			rw.WriteHeader(http.StatusOK)
		case defaults.RouteSessionSummaries.String():
			summaries := []schema.SessionSummary{}
			if r.URL.Query().Get("ids") == openTestSessionID {
				summaries = append(summaries, schema.SessionSummary{ID: openTestSessionID})
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
	w.port, err = strconv.Atoi(address.Port())
	if err != nil {
		t.Fatal(err)
	}
}

func (w *commitRefreshWorld) execute(t *testing.T, sub *cobra.Command, args ...string) (string, string, error) {
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

func (w *commitRefreshWorld) harvest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return w.execute(t, BuildHarvestCommand(), append([]string{"harvest"}, args...)...)
}

func (w *commitRefreshWorld) run(t *testing.T, command string) (string, string, error) {
	t.Helper()
	switch command {
	case "open":
		return w.execute(t, buildOpenCommand(openDependencies{
			startServer: func(webServerSpawn) (string, error) {
				t.Fatal("open started a dashboard while the test server was already answering")
				return "", nil
			},
			openBrowser:   func(string) error { return nil },
			client:        &http.Client{Timeout: defaults.ServerClientTimeout},
			readyWait:     openTestReadyWait,
			readyInterval: openTestReadyInterval,
		}), "open", "--session", openTestSessionID, "--port", strconv.Itoa(w.port))
	case "harvest":
		return w.harvest(t)
	case "harvest-session":
		return w.harvest(t, "--session", openTestSessionID)
	case "harvest-refresh":
		return w.harvest(t, "--session", openTestSessionID, "--refresh-commits")
	case "harvest-refresh-all":
		return w.harvest(t, "--refresh-commits")
	case "harvest-force":
		return w.harvest(t, "--session", openTestSessionID, "--force")
	case "harvest-dry-run":
		return w.harvest(t, "--session", openTestSessionID, "--dry-run", "--refresh-commits")
	default:
		t.Fatalf("unknown command %q", command)
		return "", "", nil
	}
}

func (w *commitRefreshWorld) store(t *testing.T) *store.Store {
	t.Helper()
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(w.dataDir())), mustGenerationStoreOptions(t, filepath.Join(w.root, "output"))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (w *commitRefreshWorld) snapshot(t *testing.T) commitRefreshSnapshot {
	t.Helper()
	db := w.store(t)
	entries, err := db.ListEntries(context.Background(), ingest.SessionID(openTestSessionID))
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)

	snap := commitRefreshSnapshot{
		entries:    string(encoded),
		entryCount: len(entries),
		indexLog:   queryText(t, conn, `SELECT group_concat(id || ':' || outcome || ':' || index_version || ':' || entries_count || ':' || started_at || ':' || COALESCE(finished_at, 0) || ':' || COALESCE(reason, '') , char(10)) FROM (SELECT id, outcome, index_version, entries_count, started_at, finished_at, reason FROM index_log WHERE session_id = ? ORDER BY id)`, openTestSessionID),
		identity:   queryText(t, conn, `SELECT COALESCE(artifact_hash, '') || '|' || COALESCE(ingested_ms, 0) || '|' || COALESCE(indexed_at, 0) || '|' || COALESCE(index_version, 0) || '|' || COALESCE(active_generation_id, '') || '|' || COALESCE(indexed_input_hash, '') || '|' || COALESCE(session_entries_hash, '') FROM sessions WHERE session_id = ?`, openTestSessionID),
		commitRows: queryText(t, conn, `SELECT group_concat(rowid || ':' || commit_hash, char(10)) FROM (SELECT rowid, commit_hash FROM session_commits WHERE session_id = ? ORDER BY commit_hash)`, openTestSessionID),
		metadata:   metadataDigest(t, filepath.Join(w.root, "output"), openTestSessionID),
	}
	snap.artifact = strings.Split(snap.identity, "|")[0]
	snap.indexCount = queryInt(t, conn, `SELECT COUNT(*) FROM index_log WHERE session_id = ?`, openTestSessionID)
	hashes := queryText(t, conn, `SELECT group_concat(commit_hash, char(10)) FROM (SELECT commit_hash FROM session_commits WHERE session_id = ? ORDER BY commit_hash)`, openTestSessionID)
	if hashes != "" {
		snap.hashes = strings.Split(hashes, "\n")
	}
	return snap
}

func metadataDigest(t *testing.T, outputDir, sessionID string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(outputDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, sessionID+"--metadata.json") {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("no metadata file for session %s under %s", sessionID, outputDir)
	}
	data, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func queryText(t *testing.T, conn *sqlite.Conn, query string, args ...any) string {
	t.Helper()
	var text string
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			text = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	return text
}

func queryInt(t *testing.T, conn *sqlite.Conn, query string, args ...any) int {
	t.Helper()
	var value int
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	return value
}
