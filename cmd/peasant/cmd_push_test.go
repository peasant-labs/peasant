package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestPushCmd_SourceProviderHelpDerived pins the --source-provider flag's help
// text to schema.AllHarnesses on the ACTUAL flag wired into BuildPushCommand().
// sourceProviderHelp() derives the provider list so it can never go stale, but a
// derived helper that is never attached to the flag (or a regression that
// re-hardcodes the usage string) would still pass the rest of make check. This
// test fails if the flag is missing/unwired or if any supported harness is absent
// from its usage, preserving the derived rather than hardcoded contract.
func TestPushCmd_SourceProviderHelpDerived(t *testing.T) {
	t.Parallel()
	flag := BuildPushCommand().Flags().Lookup("source-provider")
	if flag == nil {
		t.Fatal("--source-provider flag is not registered on the push command")
	}
	for _, h := range schema.AllHarnesses {
		if !strings.Contains(flag.Usage, h.String()) {
			t.Errorf("--source-provider usage %q is missing harness %q; help must be derived from schema.AllHarnesses, not hardcoded", flag.Usage, h.String())
		}
	}
}

// executePushCmd is a test helper that runs the push cobra command under a root
// carrying --data-dir=dir, --config-dir=dir AND --state-dir=dir (one temp dir
// isolates data + config + state), captures combined stdout+stderr, and returns
// (output, error). The "push" subcommand name is inserted automatically.
//
// NOTE on credentials: the push RunE reads credentials via
// auth.LoadCredentialsFrom(--config-dir), which honors the injected --config-dir
// override. So credential-gated push tests need no process-env mutation — write
// credentials under ResolveConfigDirPathWith(dir) (via writeTestCredentials) and
// run with t.Parallel(). Timing/error logs likewise resolve from the injected
// --state-dir=dir.
func executePushCmd(t *testing.T, dir string, args []string) (string, error) {
	t.Helper()
	return executeWithDataDir(t, BuildPushCommand(), dir, args)
}

// writeTestCredentials writes a valid credentials.json to the peasant config dir
// resolved from the given dir (ResolveConfigDirPathWith(dir) == dir/peasant),
// which is exactly where the push RunE's auth.LoadCredentialsFrom(--config-dir)
// reads when --config-dir=dir is injected by executeWithDataDir.
func writeTestCredentials(t *testing.T, dir string) {
	t.Helper()
	peasantDir := string(defaults.ResolveConfigDirPathWith(dir))
	if err := os.MkdirAll(peasantDir, 0700); err != nil {
		t.Fatalf("create peasant config dir: %v", err)
	}
	credsJSON := `{
		"api_key":         "test-api-key-abc",
		"key_id":          "test-key-id-001",
		"user_id":         "user-00001",
		"username":        "testuser",
		"village_url": "https://village.example.test",
		"linked_at":       "2025-01-01T00:00:00Z"
	}`
	credPath := filepath.Join(peasantDir, string(defaults.CredentialsFile))
	if err := os.WriteFile(credPath, []byte(credsJSON), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

// makeCmdStoreEntry builds a StoreEntry with a git remote + branch for cmd-level
// push integration tests. Mirrors internal/store's makeStoreEntry (not importable
// from package main) and additionally sets Git.Remote/Branch so the push read
// path surfaces them for branch-aware selection.
func makeCmdStoreEntry(t *testing.T, sessionID, hostSlug, remote, branch string, startMs int64, projectPaths ...string) ingest.StoreEntry {
	t.Helper()
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	ph := testutil.TestProjectHash
	hs, err := ingest.NewHostSlug(hostSlug)
	if err != nil {
		t.Fatalf("NewHostSlug(%q): %v", hostSlug, err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	srcPath, err := ingest.NewResolvedPath("/test/path/" + sessionID + ".jsonl")
	if err != nil {
		t.Fatalf("NewResolvedPath: %v", err)
	}

	ingested := startMs + 120000
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = model
	meta.HostSlug = hs
	meta.Timestamp = ingest.TimestampInfo{Start: startMs, End: startMs + 60000, Ingested: &ingested}
	meta.Source = ingest.SourceInfo{FilePath: string(srcPath), Format: ingest.SourceFormatJSONL}
	projectPath := "/home/test/myapp"
	if len(projectPaths) > 0 {
		projectPath = projectPaths[0]
	}
	meta.Project = ingest.ProjectInfo{Hash: ph, Name: "myapp", FilePath: projectPath}
	meta.Stats = ingest.StatsInfo{TurnCount: 5, ToolCallCount: 3, DurationMs: 60000, TokensIn: 100, TokensOut: 50}
	meta.Git = ingest.GitContext{Remote: &remote, Branch: &branch, Worktree: &projectPath}

	return ingest.StoreEntry{
		Metadata: &meta,
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      defaults.HarnessClaudeCode,
			SourcePath:   srcPath,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
}

// seedCrossBranchSessions inserts two sessions of the same project on different
// branches into the store at the test's XDG_DATA_HOME, for branch-aware
// selection integration tests. Returns the selected ("main") and other
// ("feature") session IDs and the shared git remote.
func seedCrossBranchSessions(t *testing.T, dir string) (selectedID, otherID, remote string) {
	t.Helper()
	remote = "git@github.com:user/repo.git"
	selectedID = "11111111-1111-1111-1111-111111111111"
	otherID = "22222222-2222-2222-2222-222222222222"

	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	projectPath := filepath.Join(dir, "repositories", "repo")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}

	entries := []ingest.StoreEntry{
		makeCmdStoreEntry(t, selectedID, "github.com-user-repo", remote, "main", 1700000000000, projectPath),
		makeCmdStoreEntry(t, otherID, "github.com-user-repo", remote, "feature", 1700000060000, projectPath),
	}
	if err := s.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	return selectedID, otherID, remote
}

func seedPublicationCursorsForTest(t *testing.T, dbPath string, sessionIDs []ingest.SessionID, pushedAt int64) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath)
	if err != nil {
		t.Fatalf("open SQLite to seed publication cursors: %v", err)
	}
	defer conn.Close()
	for _, sessionID := range sessionIDs {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET pushed_at = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{pushedAt, sessionID.String()}}); err != nil {
			t.Fatalf("seed publication cursor for %s: %v", sessionID, err)
		}
		if changes := conn.Changes(); changes != 1 {
			t.Fatalf("seed publication cursor for %s changed %d rows, want 1", sessionID, changes)
		}
	}
}

// executePushCmdSeparate runs the push command with SEPARATE stdout/stderr
// writers (unlike executePushCmd, which merges them). This lets stream-routing
// tests assert that the redaction report lands on STDERR while the push record
// (Summary / EmptyReason) lands on STDOUT.
func executePushCmdSeparate(t *testing.T, dir string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	root := newTestRoot()
	cmd := BuildPushCommand()
	root.AddCommand(cmd)

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "push"}, args...))
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// seedMultiProjectConflict seeds three sessions exercising all three branch-match
// outcomes against a REAL multi-project selection (built by the caller's config):
//   - selected:  remoteSelected / main    → admitted             (Yes)
//   - excluded:  remoteSelected / feature → rejected             (No, dropped)
//   - conflict:  remoteConflict / main    → two rules disagree   (WithheldConflict)
//
// The conflict is genuine: the caller configures two rules on remoteConflict with
// disjoint branch sets ([main] admits, [feature] rejects), so the conflict row
// arrives Locked=true through ApplySelection — not a synthetic flag.
func seedMultiProjectConflict(t *testing.T, dir string) (selectedID, excludedID, conflictID, remoteSelected, remoteConflict string) {
	t.Helper()
	remoteSelected = "git@github.com:user/repo-one.git"
	remoteConflict = "git@github.com:user/repo-two.git"
	selectedID = "11111111-1111-1111-1111-111111111111"
	excludedID = "22222222-2222-2222-2222-222222222222"
	conflictID = "33333333-3333-3333-3333-333333333333"

	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	selectedPath := filepath.Join(dir, "repositories", "repo-one")
	conflictPath := filepath.Join(dir, "repositories", "repo-two")
	for _, projectPath := range []string{selectedPath, conflictPath} {
		if err := os.MkdirAll(projectPath, 0o755); err != nil {
			t.Fatalf("mkdir project path: %v", err)
		}
	}

	entries := []ingest.StoreEntry{
		makeCmdStoreEntry(t, selectedID, "github.com-user-repo-one", remoteSelected, "main", 1700000000000, selectedPath),
		makeCmdStoreEntry(t, excludedID, "github.com-user-repo-one", remoteSelected, "feature", 1700000060000, selectedPath),
		makeCmdStoreEntry(t, conflictID, "github.com-user-repo-two", remoteConflict, "main", 1700000120000, conflictPath),
	}
	if err := s.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	return selectedID, excludedID, conflictID, remoteSelected, remoteConflict
}

// conflictConfigYAML builds a config with a mode=selected selection whose rules
// admit remoteSelected/main and create a genuine conflict on remoteConflict.
func conflictConfigYAML(remoteSelected, remoteConflict string) string {
	return fmt.Sprintf(`version: 1
push:
  method: all
  visibility: private
selection:
  mode: selected
  harnesses:
    claude-code:
      projects:
        - gitRemote: %s
          branches:
            - main
        - gitRemote: %s
          branches:
            - main
        - gitRemote: %s
          branches:
            - feature
`, remoteSelected, remoteConflict, remoteConflict)
}

// writeCfg writes cfg content to a uniquely named file under dir and returns its
// path. A distinct name per call avoids clobbering when a test needs several
// configs (the --config flag accepts any path).
func writeCfg(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	cfgPath := filepath.Join(dir, name)
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

// dryRunIDSet runs `push --dry-run --json` and returns the pushed session-ID set.
func dryRunIDSet(t *testing.T, dir string, args []string) map[string]bool {
	t.Helper()
	output, err := executePushCmd(t, dir, append([]string{"--dry-run", "--json"}, args...))
	if err != nil {
		t.Fatalf("push --dry-run --json %v: %v\noutput: %s", args, err, output)
	}
	jsonStart := strings.Index(output, "{")
	if jsonStart < 0 {
		t.Fatalf("no JSON object in output: %q", output)
	}
	var result struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(output[jsonStart:]), &result); err != nil {
		t.Fatalf("unmarshal push JSON: %v\njson: %s", err, output[jsonStart:])
	}
	ids := map[string]bool{}
	for _, s := range result.Sessions {
		ids[s.SessionID] = true
	}
	return ids
}

// wizardKeptIDSet builds the wizard's view via the TTY-free seam and returns the
// approved (unlocked) session-ID set, mirroring how RunE constructs the query +
// selection from config.
func wizardKeptIDSet(t *testing.T, dir, cfgPath string, force bool, sourceProvider string) map[string]bool {
	t.Helper()
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	resolved, err := ingest.NewResolvedPath(cfg.Output.BasePath)
	if err != nil {
		t.Fatalf("resolve output path: %v", err)
	}
	cfg.Output.BasePath = string(resolved)

	q := push.PushCandidateQuery{
		Force:          force,
		SourceProvider: sourceProvider,
		Method:         cfg.Push.Method,
		Sources:        cfg.Push.Sources,
	}

	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	var sel *push.SessionSelection
	if cfg.Selection.Mode == config.SelectionModeSelected {
		matcher := cfg.SelectionMatcher()
		sel, err = preparePushSelection(context.Background(), db, matcher, fixturePathIdentityResolver{})
		if err != nil {
			t.Fatalf("preparePushSelection: %v", err)
		}
	}

	wiz, err := buildPushWizardSessions(context.Background(), db, &ingest.OSFileSystem{}, cfg.Output.BasePath, q, sel)
	if err != nil {
		t.Fatalf("buildPushWizardSessions: %v", err)
	}
	// A nil preview read: this case asserts the selected set, and never draws
	// the preview pane.
	model := push.NewPushWizard(theme.New(theme.ModeDark), wiz, nil)
	ids := map[string]bool{}
	for _, id := range model.SelectedSessionIDs() {
		ids[id] = true
	}
	return ids
}

func setsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestPushSelection_WizardEqualsDryRunEqualsPipeline is the core invariant: the
// wizard's selectable set equals the dry-run/pipeline push set across all five
// query modes. Both paths route through the SHARED QueryPushCandidates helper, so
// they cannot diverge (including the previously wizard-blind by-source mode).
func TestPushSelection_WizardEqualsDryRunEqualsPipeline(t *testing.T) {
	// PARALLEL: data/config/state are injected per-invocation via flags
	// (executeWithDataDir), and credentials are read via LoadCredentialsFrom
	// (--config-dir), so no process-global env mutation is needed.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	selectedID, otherID, remote := seedCrossBranchSessions(t, dir)

	cfgAll := writeCfg(t, dir, "cfg-all.yaml", `version: 1
push:
  method: all
  visibility: private
selection:
  mode: all
`)
	cfgSelected := writeCfg(t, dir, "cfg-selected.yaml", fmt.Sprintf(`version: 1
push:
  method: all
  visibility: private
selection:
  mode: selected
  harnesses:
    claude-code:
      projects:
        - gitRemote: %s
          branches:
            - main
`, remote))
	cfgBySource := writeCfg(t, dir, "cfg-by-source.yaml", `version: 1
push:
  method: by-source
  sources:
    - claude-code
  visibility: private
selection:
  mode: all
`)

	claude := string(defaults.HarnessClaudeCode)

	cases := []struct {
		name           string
		cfgPath        string
		force          bool
		sourceProvider string
		// dryRunArgs are the extra CLI args (mirroring force/sourceProvider).
		dryRunArgs []string
	}{
		{"default", cfgAll, false, "", nil},
		{"selected", cfgSelected, false, "", nil},
		{"force", cfgAll, true, "", []string{"--force"}},
		{"source-provider", cfgAll, false, claude, []string{"--source-provider=" + claude}},
		{"by-source", cfgBySource, false, "", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pipelineSet := dryRunIDSet(t, dir, append([]string{"--config=" + tc.cfgPath}, tc.dryRunArgs...))
			wizardSet := wizardKeptIDSet(t, dir, tc.cfgPath, tc.force, tc.sourceProvider)

			if !setsEqual(pipelineSet, wizardSet) {
				t.Fatalf("wizard set != pipeline set\n  pipeline: %v\n  wizard:   %v", pipelineSet, wizardSet)
			}
			if len(pipelineSet) == 0 {
				t.Fatalf("expected a non-empty push set for mode %q", tc.name)
			}
			// Spot-check the selection-aware mode actually filters.
			if tc.name == "selected" {
				if !pipelineSet[selectedID] {
					t.Errorf("selected mode: expected %s in set; got %v", selectedID, pipelineSet)
				}
				if pipelineSet[otherID] {
					t.Errorf("selected mode: expected %s excluded; got %v", otherID, pipelineSet)
				}
			}
		})
	}
}

// TestBuildPushWizardSessions_SelectionAware exercises the TTY-free seam against a
// real seeded store: the wizard VIEW shows the selected session (unlocked), the
// branch-conflict session (Locked), excludes the cross-branch session, and the
// approved set equals the kept set.
func TestBuildPushWizardSessions_SelectionAware(t *testing.T) {
	// PARALLEL: seeding + store open resolve from `dir` directly
	// (ResolveDBFilePathWith), no process env.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	selectedID, excludedID, conflictID, remoteSelected, remoteConflict := seedMultiProjectConflict(t, dir)
	cfgPath := writeCfg(t, dir, "conflict.yaml", conflictConfigYAML(remoteSelected, remoteConflict))

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	resolved, err := ingest.NewResolvedPath(cfg.Output.BasePath)
	if err != nil {
		t.Fatalf("resolve output path: %v", err)
	}
	cfg.Output.BasePath = string(resolved)
	matcher := cfg.SelectionMatcher()

	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	selection, err := preparePushSelection(context.Background(), db, matcher, fixturePathIdentityResolver{})
	if err != nil {
		t.Fatalf("preparePushSelection: %v", err)
	}

	q := push.PushCandidateQuery{Method: cfg.Push.Method, Sources: cfg.Push.Sources}
	wiz, err := buildPushWizardSessions(context.Background(), db, &ingest.OSFileSystem{}, cfg.Output.BasePath, q, selection)
	if err != nil {
		t.Fatalf("buildPushWizardSessions: %v", err)
	}

	byID := map[string]push.PushWizardSession{}
	for _, w := range wiz {
		byID[w.Row.SessionID] = w
	}

	// Selected session present and unlocked.
	if s, ok := byID[selectedID]; !ok || s.Locked {
		t.Errorf("selected session %s should be present and unlocked; got ok=%v locked=%v", selectedID, ok, s.Locked)
	}
	// Conflict session present and Locked (real BranchMatchWithheldConflict).
	if s, ok := byID[conflictID]; !ok || !s.Locked {
		t.Errorf("conflict session %s should be present and Locked; got ok=%v locked=%v", conflictID, ok, s.Locked)
	}
	// Cross-branch session excluded entirely.
	if _, ok := byID[excludedID]; ok {
		t.Errorf("cross-branch session %s should be excluded from the wizard view", excludedID)
	}

	// Approved (unlocked) set == kept set == {selectedID}.
	// A nil preview read: the assertion is the approved set, not the pane.
	approved := push.NewPushWizard(theme.New(theme.ModeDark), wiz, nil).SelectedSessionIDs()
	if len(approved) != 1 || approved[0] != selectedID {
		t.Fatalf("approved set should be exactly [%s]; got %v", selectedID, approved)
	}
}

// TestPushCmd_PublicConsent_ReportOnStderr_NonInteractive verifies that the
// public visibility no longer emits the retired downgrade disclosure while the
// push record (EmptyReason here) remains on STDOUT.
func TestPushCmd_PublicConsent_ReportOnStderr_NonInteractive(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	stdout, stderr, err := executePushCmdSeparate(t, dir, []string{"--visibility=public", "--non-interactive"})
	if err != nil {
		t.Fatalf("push --visibility=public --non-interactive: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if strings.Contains(stderr+stdout, "visibility is not yet implemented") || strings.Contains(stderr+stdout, "downgraded safely") {
		t.Errorf("retired visibility downgrade disclosure must not appear; stdout: %s stderr: %s", stdout, stderr)
	}
	// The push record (empty-state message) is the stdout record.
	if !strings.Contains(stdout, "Nothing to push") && !strings.Contains(stdout, "All sessions") {
		t.Errorf("push record (EmptyReason) should be on STDOUT; stdout: %s", stdout)
	}
}

// TestPushCmd_NonInteractiveAlias_SkipsInteractive is the behavioral test for the
// literal user-reported bug ("--yes still opens the TUI"). The wizard branch and
// the interactive wizard are gated by the nonInteractive variable
// (nonInteractiveFlag || yesFlag). Public visibility is real and therefore keeps
// the consent gate unless one of those explicit non-interactive flags is set.
//
// (The wizard itself requires a TTY, which the test harness cannot provide; this
// asserts the shared bypass that the wizard gate also consumes.)
func TestPushCmd_NonInteractiveAlias_SkipsInteractive(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	// Control: no skip flag. A real public publish remains consent-gated.
	outCtl, errCtl, err := executePushCmdSeparate(t, dir, []string{"--visibility=public"})
	if err != nil {
		t.Fatalf("control push: unexpected error: %v", err)
	}
	if !strings.Contains(errCtl, "re-run with --yes") {
		t.Errorf("control (no flag) should retain public consent; stderr: %s", errCtl)
	}
	if outCtl != "" {
		t.Errorf("control must stop before the pipeline while awaiting consent; stdout: %s", outCtl)
	}

	// Both skip flags must bypass consent, proceed to the pipeline, and behave
	// identically to each other.
	var prev string
	for _, flag := range []string{"--non-interactive", "--yes"} {
		out, errs, err := executePushCmdSeparate(t, dir, []string{"--visibility=public", flag})
		if err != nil {
			t.Fatalf("push %s: unexpected error: %v\nstdout: %s\nstderr: %s", flag, err, out, errs)
		}
		if strings.Contains(errs, "re-run with --yes") {
			t.Errorf("%s should bypass the interactive consent (no --yes hint); stderr: %s", flag, errs)
		}
		if strings.Contains(errs, "downgraded safely") || strings.Contains(errs, "not yet implemented") {
			t.Errorf("%s must not print the retired downgrade; stderr: %s", flag, errs)
		}
		if !strings.Contains(out, "Nothing to push") && !strings.Contains(out, "All sessions") {
			t.Errorf("%s should reach the pipeline (empty-state record on stdout); stdout: %s", flag, out)
		}
		combined := out + "\x00" + errs
		if prev != "" && combined != prev {
			t.Errorf("--yes must behave identically to --non-interactive\n  non-interactive: %q\n  yes:             %q", prev, combined)
		}
		prev = combined
	}
}

// TestPushCmd_Quiet_SuppressesReport verifies that --quiet suppresses the
// redaction report on stderr while retaining the final result on stdout.
// Errors are never suppressed by quiet mode.
func TestPushCmd_Quiet_SuppressesReport(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	out, errs, err := executePushCmdSeparate(t, dir, []string{"--visibility=public", "--non-interactive", "--quiet"})
	if err != nil {
		t.Fatalf("push public --non-interactive --quiet: %v\nstdout: %s\nstderr: %s", err, out, errs)
	}
	// The store is empty here, so the record has nothing to describe either way.
	// What --quiet suppresses when there IS something to publish is proven against
	// a seeded session in the push-disclosure corpus, where the assertion cannot
	// pass by absence.
	if !strings.Contains(out, "Nothing to push") && !strings.Contains(out, "All sessions") {
		t.Errorf("--quiet should still print the final result line on STDOUT; stdout: %s", out)
	}
	if strings.Contains(errs, "visibility is not implemented") || strings.Contains(errs, "downgraded") {
		t.Errorf("--quiet must not print the retired visibility downgrade; stderr: %s", errs)
	}
}

// TestPushCmd_NothingIngested_SelectionActive_NoSelectionNote verifies that when
// the base store has NO candidate sessions, an active selection must NOT trigger
// the selection-specific note (the generic empty path applies instead). Exit 0.
func TestPushCmd_NothingIngested_SelectionActive_NoSelectionNote(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	// No sessions seeded — the store is empty.
	cfgPath := writeCfg(t, dir, "nothing-ingested.yaml", `version: 1
push:
  method: all
  visibility: private
selection:
  mode: selected
  harnesses:
    claude-code:
      projects:
        - gitRemote: git@github.com:user/repo.git
          branches:
            - main
`)

	out, errs, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--config=" + cfgPath})
	if err != nil {
		t.Fatalf("expected exit 0, got error: %v\nstdout: %s\nstderr: %s", err, out, errs)
	}
	if strings.Contains(errs, "no sessions match the configured selection") {
		t.Errorf("selection note must NOT fire when nothing is ingested; stderr: %s", errs)
	}
	if !strings.Contains(out, "Nothing to push") {
		t.Errorf("generic empty-state message should appear on STDOUT; stdout: %s", out)
	}
}

// TestPushCmd_AllAlreadyPushed_SelectionActive_NoSelectionNote verifies the third
// empty-state case: when every candidate is already pushed (base unpushed set is
// empty), an active selection must NOT trigger the selection-specific note — the
// generic "all already pushed" path applies. Exit 0.
func TestPushCmd_AllAlreadyPushed_SelectionActive_NoSelectionNote(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir; store opens from `dir`.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	selectedID, otherID, remote := seedCrossBranchSessions(t, dir)

	// Seed legacy cursors so the unpushed base query returns empty. Production
	// cursor updates are available only through receipt-validated SavePublication.
	var sids []ingest.SessionID
	for _, raw := range []string{selectedID, otherID} {
		sid, sErr := ingest.NewSessionID(raw)
		if sErr != nil {
			t.Fatalf("NewSessionID: %v", sErr)
		}
		sids = append(sids, sid)
	}
	seedPublicationCursorsForTest(t, string(defaults.ResolveDBFilePathWith(dir)), sids, 1700000200000)

	cfgPath := writeCfg(t, dir, "all-pushed.yaml", fmt.Sprintf(`version: 1
push:
  method: all
  visibility: private
selection:
  mode: selected
  harnesses:
    claude-code:
      projects:
        - gitRemote: %s
          branches:
            - main
`, remote))

	out, errs, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--config=" + cfgPath})
	if err != nil {
		t.Fatalf("expected exit 0, got error: %v\nstdout: %s\nstderr: %s", err, out, errs)
	}
	if strings.Contains(errs, "no sessions match the configured selection") {
		t.Errorf("selection note must NOT fire when all sessions are already pushed; stderr: %s", errs)
	}
	if !strings.Contains(out, "All sessions already pushed") {
		t.Errorf("generic 'all already pushed' message should appear on STDOUT; stdout: %s", out)
	}
}

// TestPushCmd_Verbosity_QuietAndVerbose verifies verbosity routing via --dry-run (no
// network): --quiet suppresses the summary but prints a terse result line, while
// --verbose prints per-session detail.
func TestPushCmd_Verbosity_QuietAndVerbose(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir; store opens from `dir`.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	selectedID, _, _ := seedCrossBranchSessions(t, dir)

	// --quiet: no "Summary:" / "Dry run summary:" block; terse result line present.
	quietOut, err := executePushCmd(t, dir, []string{"--dry-run", "--quiet"})
	if err != nil {
		t.Fatalf("push --dry-run --quiet: %v\noutput: %s", err, quietOut)
	}
	if strings.Contains(quietOut, "Summary:") || strings.Contains(quietOut, "Dry run summary:") {
		t.Errorf("--quiet should suppress the summary block; got: %s", quietOut)
	}
	if !strings.Contains(quietOut, "would push") {
		t.Errorf("--quiet should still print a terse result line; got: %s", quietOut)
	}

	// --verbose: per-session detail (session ID row) present.
	verboseOut, err := executePushCmd(t, dir, []string{"--dry-run", "--verbose"})
	if err != nil {
		t.Fatalf("push --dry-run --verbose: %v\noutput: %s", err, verboseOut)
	}
	if !strings.Contains(verboseOut, selectedID) {
		t.Errorf("--verbose should print per-session detail including %s; got: %s", selectedID, verboseOut)
	}
	if !strings.Contains(verboseOut, "Dry run summary:") {
		t.Errorf("--verbose should still print the summary line; got: %s", verboseOut)
	}
}

// TestResolveOutputLevel verifies the full mapping of --quiet/--verbose to the
// typed outputLevel enum — all three success returns plus the mutual-exclusion error path.
func TestResolveOutputLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		quiet   bool
		verbose bool
		want    outputLevel
		wantErr bool
	}{
		{"neither → normal", false, false, outputNormal, false},
		{"quiet → quiet", true, false, outputQuiet, false},
		{"verbose → verbose", false, true, outputVerbose, false},
		{"both → error", true, true, outputNormal, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOutputLevel(tc.quiet, tc.verbose)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for quiet=%v verbose=%v", tc.quiet, tc.verbose)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveOutputLevel(%v,%v) = %v (%s); want %v", tc.quiet, tc.verbose, got, got, tc.want)
			}
		})
	}
}

// TestPushSummary_VerboseMappingNonDryRun asserts the cmd_push.go:348 mapping for
// a NON-dry-run push: level==outputVerbose drives printPushSummary's verbose flag
// so per-session detail is rendered (mirrors the RunE call with dryRun=false).
func TestPushSummary_VerboseMappingNonDryRun(t *testing.T) {
	t.Parallel()
	level, err := resolveOutputLevel(false, true)
	if err != nil || level != outputVerbose {
		t.Fatalf("resolveOutputLevel(false,true) = %v, %v; want outputVerbose, nil", level, err)
	}
	result := &push.PushResult{
		New: 1,
		Sessions: []push.SessionPushResult{
			{SessionID: "sess-verbose-1", HostSlug: "github.com-user-repo", Title: "T", Status: push.PushStatusNew},
		},
	}
	var buf bytes.Buffer
	// Mirror cmd_push.go:348 exactly, with dryRun=false (real push).
	printPushSummary(&buf, result, false /* dryRun */, level == outputVerbose)
	out := buf.String()
	if !strings.Contains(out, "sess-verbose-1") {
		t.Errorf("non-dry-run verbose should render per-session detail; got: %s", out)
	}
	if !strings.Contains(out, "Summary:") {
		t.Errorf("non-dry-run verbose should still print the Summary line; got: %s", out)
	}
}

// TestPushCmd_QuietVerboseMutualExclusion verifies --quiet + --verbose is an
// actionable error naming both flags (resolved before any work, so no creds need).
func TestPushCmd_QuietVerboseMutualExclusion(t *testing.T) {
	// PARALLEL: --quiet+--verbose is rejected by resolveOutputLevel BEFORE the
	// credential gate or any config read, so no env isolation is needed.
	t.Parallel()
	dir := t.TempDir()

	_, err := executePushCmd(t, dir, []string{"--quiet", "--verbose"})
	if err == nil {
		t.Fatal("expected an error for --quiet + --verbose, got nil")
	}
	if !strings.Contains(err.Error(), "--quiet") || !strings.Contains(err.Error(), "--verbose") {
		t.Errorf("mutual-exclusion error should name both flags; got: %v", err)
	}
}

// TestPushCmd_ErrorSummaryTable verifies R5 routing: the "Errors by type:" table
// renders in the default/verbose non-JSON branch, is suppressed under --quiet,
// and never pollutes --json stdout. It exercises ≥2 typed categories (no-model +
// metadata-missing) so the deterministic ordering renders through the REAL CLI:
// one session has on-disk metadata with an empty model (→ no-model), the other
// has no metadata file at all (→ metadata-missing). Driven via --dry-run (no
// network), which now reads metadata for parity with the real push path.
func TestPushCmd_ErrorSummaryTable(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir; store opens from `dir`;
	// output.basePath is an explicit config path (syncBase).
	t.Parallel()
	dir := t.TempDir()
	syncBase := t.TempDir()
	writeTestCredentials(t, dir)

	// Two unpushed sessions in the DB (same host slug).
	emptyModelID, missingMetaID, _ := seedCrossBranchSessions(t, dir)

	// Point output.basePath at an isolated tempdir and write on-disk metadata
	// with an EMPTY model for ONE session; the other has no metadata file.
	cfgPath := writeCfg(t, dir, "errtable.yaml", fmt.Sprintf(`version: 1
push:
  method: all
  visibility: private
output:
  basePath: %s
`, syncBase))
	writeEmptyModelMetadata(t, syncBase, "github.com-user-repo", emptyModelID)
	_ = missingMetaID // its metadata is intentionally absent → metadata-missing

	cfgArg := "--config=" + cfgPath

	// Default: table present with BOTH categories in deterministic order. Counts
	// tie at 1 each, so the declaration-order tie-break puts no-model before
	// metadata-missing.
	defaultOut, err := executePushCmd(t, dir, []string{"--dry-run", cfgArg})
	if err != nil {
		t.Fatalf("push --dry-run: %v\noutput: %s", err, defaultOut)
	}
	if !strings.Contains(defaultOut, "Errors by type:") {
		t.Fatalf("default output should include the error-summary table; got: %s", defaultOut)
	}
	noModelIdx := strings.Index(defaultOut, string(push.CategoryNoModel))
	missingIdx := strings.Index(defaultOut, string(push.CategoryMetadataMissing))
	if noModelIdx < 0 || missingIdx < 0 {
		t.Fatalf("table should name both no-model and metadata-missing; got: %s", defaultOut)
	}
	if noModelIdx > missingIdx {
		t.Errorf("deterministic order broken: no-model must render before metadata-missing; got: %s", defaultOut)
	}

	// --quiet: table suppressed.
	quietOut, err := executePushCmd(t, dir, []string{"--dry-run", "--quiet", cfgArg})
	if err != nil {
		t.Fatalf("push --dry-run --quiet: %v\noutput: %s", err, quietOut)
	}
	if strings.Contains(quietOut, "Errors by type:") {
		t.Errorf("--quiet must suppress the error-summary table; got: %s", quietOut)
	}

	// --json: stdout stays clean (no table) and parses as JSON.
	jsonOut, err := executePushCmd(t, dir, []string{"--dry-run", "--json", cfgArg})
	if err != nil {
		t.Fatalf("push --dry-run --json: %v\noutput: %s", err, jsonOut)
	}
	if strings.Contains(jsonOut, "Errors by type:") {
		t.Errorf("--json stdout must not contain the table; got: %s", jsonOut)
	}
	jsonStart := strings.Index(jsonOut, "{")
	if jsonStart < 0 {
		t.Fatalf("no JSON object in --json output: %q", jsonOut)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonOut[jsonStart:]), &parsed); err != nil {
		t.Errorf("--json stdout should be valid JSON: %v\njson: %s", err, jsonOut[jsonStart:])
	}
}

// writeEmptyModelMetadata writes a {sessionID}--metadata.json with an empty model
// field to the root-session path under base, so the push/dry-run path classifies
// the session as the no-model error category.
func writeEmptyModelMetadata(t *testing.T, base, hostSlug, sessionID string) {
	t.Helper()
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = ingest.SessionID(sessionID)
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = "" // the defect under test → no-model category
	meta.HostSlug = ingest.HostSlug(hostSlug)
	ingested := int64(1700000120000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested}
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "myapp"}
	meta.Source = ingest.SourceInfo{Format: ingest.SourceFormatJSONL, FilePath: "/source/file.jsonl"}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal empty-model metadata: %v", err)
	}
	path := ingest.SessionMetadataPath(base, hostSlug, sessionID, "")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for metadata: %v", err)
	}
	if err := os.WriteFile(path, metaJSON, 0o644); err != nil {
		t.Fatalf("write empty-model metadata: %v", err)
	}
}

// TestPushCmd_DeprecatedProjectHashKey_Warns verifies the R1 legacy-key
// deprecation note: a config carrying push.fields.projectHash prints a one-line
// note on STDERR (parse still succeeds, exit 0); a config without it prints no
// such note.
func TestPushCmd_DeprecatedProjectHashKey_Warns(t *testing.T) {
	t.Parallel()
	const note = "'push.fields.projectHash'"

	t.Run("present → warns on stderr", func(t *testing.T) {
		// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
		t.Parallel()
		dir := t.TempDir()
		writeTestCredentials(t, dir)

		cfgPath := writeCfg(t, dir, "legacy.yaml", `version: 1
push:
  method: all
  visibility: private
  fields:
    projectHash: true
`)
		_, errs, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--config=" + cfgPath})
		if err != nil {
			t.Fatalf("expected exit 0, got: %v\nstderr: %s", err, errs)
		}
		if !strings.Contains(errs, note) {
			t.Errorf("expected deprecation note on stderr; got: %s", errs)
		}
	})

	t.Run("absent → no warning", func(t *testing.T) {
		// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
		t.Parallel()
		dir := t.TempDir()
		writeTestCredentials(t, dir)

		cfgPath := writeCfg(t, dir, "clean.yaml", `version: 1
push:
  method: all
  visibility: private
`)
		_, errs, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--config=" + cfgPath})
		if err != nil {
			t.Fatalf("expected exit 0, got: %v\nstderr: %s", err, errs)
		}
		if strings.Contains(errs, note) {
			t.Errorf("did not expect deprecation note when key absent; got: %s", errs)
		}
	})
}

// TestPushCmd_EmptySelection_NonInteractive_Note verifies that when a mode=selected
// selection matches zero candidates, the result line names the selection as the
// cause and the command exits 0.
//
// The explanation is the result line itself, printed once. It previously arrived
// twice — a stderr note saying the selection excluded everything, immediately
// followed by the result line saying the same thing at greater length.
func TestPushCmd_EmptySelection_NonInteractive_Note(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir; store opens from `dir`.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	// Seed sessions, but configure a selection that matches NONE of them.
	seedCrossBranchSessions(t, dir)
	cfgPath := writeCfg(t, dir, "empty-sel.yaml", `version: 1
push:
  method: all
  visibility: private
selection:
  mode: selected
  harnesses:
    claude-code:
      projects:
        - gitRemote: git@github.com:nobody/no-such-repo.git
          branches:
            - main
`)

	stdout, stderr, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--non-interactive", "--config=" + cfgPath})
	if err != nil {
		t.Fatalf("expected exit 0 for empty selection, got error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "No sessions match the configured selection") {
		t.Errorf("expected the result line to name the selection as the cause; stdout: %s", stdout)
	}
	if !strings.Contains(stdout, "it cannot widen a selection") {
		t.Errorf("expected the result line to say --force cannot widen a selection; stdout: %s", stdout)
	}
	if strings.Contains(strings.ToLower(stderr), "excluded by the configured selection") {
		t.Errorf("the same fact must not be reported twice; stderr: %s", stderr)
	}
}

// TestPushCmd_SelectionFiltersByBranch_Integration is the end-to-end wiring test:
// a mode=selected config is honored through BuildPushCommand, so only sessions on
// the selected branch appear in the dry-run push set. Closes the cmd-wiring gap
// flagged by an earlier test-quality review.
func TestPushCmd_SelectionFiltersByBranch_Integration(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir; store opens from `dir`;
	// config passed explicitly via --config.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	selectedID, otherID, remote := seedCrossBranchSessions(t, dir)

	peasantDir := filepath.Join(dir, string(defaults.AppName))
	if err := os.MkdirAll(peasantDir, 0700); err != nil {
		t.Fatalf("create peasant config dir: %v", err)
	}
	cfgContent := fmt.Sprintf(`version: 1
push:
  method: all
  visibility: private
selection:
  mode: selected
  harnesses:
    claude-code:
      projects:
        - gitRemote: %s
          branches:
            - main
`, remote)
	cfgPath := filepath.Join(peasantDir, string(defaults.Config.FileName))
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	output, err := executePushCmd(t, dir, []string{"--dry-run", "--json", "--config=" + cfgPath})
	if err != nil {
		t.Fatalf("push --dry-run --json: unexpected error: %v\noutput: %s", err, output)
	}

	jsonStart := strings.Index(output, "{")
	if jsonStart < 0 {
		t.Fatalf("no JSON object in output: %q", output)
	}
	var result struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(output[jsonStart:]), &result); err != nil {
		t.Fatalf("unmarshal push JSON: %v\njson: %s", err, output[jsonStart:])
	}

	ids := map[string]bool{}
	for _, s := range result.Sessions {
		ids[s.SessionID] = true
	}
	if !ids[selectedID] {
		t.Errorf("expected selected (main) session %s in push set; got %v", selectedID, ids)
	}
	if ids[otherID] {
		t.Errorf("expected other (feature) session %s excluded by selection; got %v", otherID, ids)
	}
}

// TestPushCmd_Flags verifies all expected flags are registered with correct
// names and defaults on the push command.
func TestPushCmd_Flags(t *testing.T) {
	t.Parallel()
	cmd := BuildPushCommand()

	type flagCheck struct {
		name         string
		defaultValue string
	}

	stringFlags := []flagCheck{
		{"source-provider", ""},
		{"visibility", ""},
		{"repository", ""},
	}
	boolFlags := []flagCheck{
		{"dry-run", "false"},
		{"force", "false"},
		{defaults.JSONFlagName, "false"},
		{"verbose", "false"},
		{"quiet", "false"},
		{"non-interactive", "false"},
		{"yes", "false"},
		{"timing", "false"},
	}
	intFlags := []flagCheck{
		{"concurrency", "0"},
	}

	for _, fc := range stringFlags {
		f := cmd.Flags().Lookup(fc.name)
		if f == nil {
			t.Errorf("flag --%s not registered", fc.name)
			continue
		}
		if f.DefValue != fc.defaultValue {
			t.Errorf("flag --%s: want default %q, got %q", fc.name, fc.defaultValue, f.DefValue)
		}
	}
	for _, fc := range boolFlags {
		f := cmd.Flags().Lookup(fc.name)
		if f == nil {
			t.Errorf("flag --%s not registered", fc.name)
			continue
		}
		if f.DefValue != fc.defaultValue {
			t.Errorf("flag --%s: want default %q, got %q", fc.name, fc.defaultValue, f.DefValue)
		}
	}
	for _, fc := range intFlags {
		f := cmd.Flags().Lookup(fc.name)
		if f == nil {
			t.Errorf("flag --%s not registered", fc.name)
			continue
		}
		if f.DefValue != fc.defaultValue {
			t.Errorf("flag --%s: want default %q, got %q", fc.name, fc.defaultValue, f.DefValue)
		}
	}
}

// TestPrintAnnotationSummary_ShowsRetracted verifies the human-readable summary
// surfaces the retraction count (retraction propagation must be observable).
func TestPrintAnnotationSummary_ShowsRetracted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printAnnotationSummary(&buf, &push.AnnotationPushSummary{
		Total: 3, Created: 1, Updated: 0, Skipped: 1, Retracted: 2, Errors: 0,
	}, nil, false)
	out := buf.String()
	if !strings.Contains(out, "2 retracted") {
		t.Errorf("annotation summary should show '2 retracted'; got:\n%s", out)
	}
}

func TestPrintAnnotationSummary_ReportsPermanentRowsBesideTransientFailure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	unpublishable := (push.UnpublishableAnnotation{
		ID:            "annotation-without-target",
		AnnotatorName: "outcome-classifier",
		TypeID:        "quality.turn_outcome",
		TargetKind:    schema.TargetEntry,
		Reason:        "entry target is required",
	}).WithCommandPrefix("peasant --data-dir '/tmp/bound data'")
	printAnnotationSummary(&buf, &push.AnnotationPushSummary{
		Unpublishable: []push.UnpublishableAnnotation{unpublishable},
	}, errors.New("temporary village failure"), false)
	out := buf.String()
	for _, want := range []string{"Your transcript push was not affected", "annotation-without-target", "peasant --data-dir '/tmp/bound data' annotate prune", "--dry-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("annotation failure output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "peasant ingest --force --session") {
		t.Errorf("annotation failure repeats the harmful re-ingest recovery:\n%s", out)
	}
}

// TestPrintPushJSON_IncludesRetracted verifies the --json annotation result
// carries the retracted count under the "retracted" key.
func TestPrintPushJSON_IncludesRetracted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := printPushJSON(&buf, &push.PushResult{}, &push.AnnotationPushSummary{
		Total: 1, Retracted: 4,
	}, nil)
	if err != nil {
		t.Fatalf("printPushJSON: %v", err)
	}
	var parsed struct {
		Annotations struct {
			Retracted int `json:"retracted"`
		} `json:"annotations"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal push JSON: %v\n%s", err, buf.String())
	}
	if parsed.Annotations.Retracted != 4 {
		t.Errorf("json annotations.retracted = %d, want 4; got:\n%s", parsed.Annotations.Retracted, buf.String())
	}
}

// TestPushCmd_Concurrency_RejectsNonPositive verifies that --concurrency 0 (and
// negative) is rejected at the CLI with an actionable error, before any push work.
func TestPushCmd_Concurrency_RejectsNonPositive(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	for _, bad := range []string{"0", "-3"} {
		_, err := executePushCmd(t, dir, []string{"--concurrency", bad})
		if err == nil {
			t.Errorf("--concurrency %s: expected an error, got nil", bad)
			continue
		}
		if !strings.Contains(err.Error(), ">= 1") {
			t.Errorf("--concurrency %s: error not actionable: %v", bad, err)
		}
	}
}

// TestPushCmd_InvalidLicense_FailsFast verifies the --license flag is validated
// up front (before any per-session upload) and that the error names the canonical
// license menu derived from schema.AllLicenses (guards against a hardcoded list
// drifting from the source of truth).
func TestPushCmd_InvalidLicense_FailsFast(t *testing.T) {
	// PARALLEL: credentials written so we clear the auth gate and reach the
	// up-front --license validation in RunE.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	for _, bad := range []string{"MIT", "cc0-1.0", "GPL-3.0"} {
		_, err := executePushCmd(t, dir, []string{"--license", bad})
		if err == nil {
			t.Errorf("--license %q: expected an error, got nil", bad)
			continue
		}
		if !strings.Contains(err.Error(), "invalid --license") {
			t.Errorf("--license %q: error not actionable: %v", bad, err)
		}
		if !strings.Contains(err.Error(), schema.LicenseMenu()) {
			t.Errorf("--license %q: error should list the valid menu %q, got: %v", bad, schema.LicenseMenu(), err)
		}
	}
}

// TestPushCmd_MissingCredentials verifies that running peasant push without stored
// credentials returns a clear "not logged in" error.
func TestPushCmd_MissingCredentials(t *testing.T) {
	// PARALLEL: exercises the ABSENT-credentials path. --config-dir=dir isolates
	// the config dir (LoadCredentialsFrom honors it), so no credentials file
	// exists under the fresh temp dir and the local ~/.config/peasant is never
	// consulted. No t.Setenv needed.
	t.Parallel()
	dir := t.TempDir()

	output, err := executePushCmd(t, dir, []string{})
	if err == nil {
		t.Fatalf("expected error when not logged in, got nil; output: %s", output)
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error should mention 'not logged in', got: %v", err)
	}
}

// TestPushCmd_DryRun verifies the --dry-run flag runs without error on an empty
// store and prints the dry-run summary line.
func TestPushCmd_DryRun(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	output, err := executePushCmd(t, dir, []string{"--dry-run"})
	if err != nil {
		t.Fatalf("push --dry-run on empty store: unexpected error: %v\noutput: %s", err, output)
	}

	// With an empty store the pipeline returns EmptyReason which is printed.
	// Either "Nothing to push" or the dry-run banner must appear.
	if !strings.Contains(output, "Dry run") && !strings.Contains(output, "Nothing to push") && !strings.Contains(output, "All sessions") {
		t.Errorf("push --dry-run output should mention dry run or empty store; got: %s", output)
	}
}

// TestPushCmd_Timing_RollupAndLog verifies that `peasant push --timing` emits the
// per-phase rollup to stderr and writes a per-upload JSONL log under the XDG state
// directory. An empty store does no network, so the rollup reports zero uploads —
// but the harness (rollup + log file) must still be produced, proving the
// --timing wiring runs end-to-end.
func TestPushCmd_Timing_RollupAndLog(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir, and the timing log path
	// resolves from --state-dir (both injected as `dir` by executeWithDataDir via
	// executePushCmd). writeTimingLog(c, stateDirOv) now honors that override, so
	// the log lands under ResolveStateDirPathWith(dir)/logs.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	output, err := executePushCmd(t, dir, []string{"--timing"})
	if err != nil {
		t.Fatalf("push --timing on empty store: unexpected error: %v\noutput: %s", err, output)
	}

	// Rollup header + the uploads/reused line must appear (on the captured stderr).
	if !strings.Contains(output, "per-phase rollup") {
		t.Errorf("--timing output missing rollup header; got:\n%s", output)
	}
	if !strings.Contains(output, "uploads=") {
		t.Errorf("--timing output missing uploads line; got:\n%s", output)
	}

	// A timestamped JSONL log must be written under <state>/peasant/logs/, where
	// <state> resolves from --state-dir=dir (injected by executePushCmd).
	logDir := filepath.Join(string(defaults.ResolveStateDirPathWith(dir)), "logs")
	entries, readErr := os.ReadDir(logDir)
	if readErr != nil {
		t.Fatalf("read timing log dir %s: %v", logDir, readErr)
	}
	var found bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "push--timing--") && strings.HasSuffix(e.Name(), ".jsonl") {
			found = true
		}
	}
	if !found {
		t.Errorf("no push--timing--*.jsonl log written under %s; entries: %v", logDir, entries)
	}
	// And the announce line points the user at the file.
	if !strings.Contains(output, "timing log written to") {
		t.Errorf("--timing output missing 'timing log written to' announcement; got:\n%s", output)
	}
}

// TestPushCmd_NoTiming_NoLog verifies that without --timing, no timing rollup is
// printed and no timing log is written (off by default).
func TestPushCmd_NoTiming_NoLog(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir; state dir resolves from
	// --state-dir=dir (both injected by executePushCmd).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	output, err := executePushCmd(t, dir, []string{})
	if err != nil {
		t.Fatalf("push on empty store: unexpected error: %v\noutput: %s", err, output)
	}
	if strings.Contains(output, "per-phase rollup") {
		t.Errorf("rollup printed without --timing; got:\n%s", output)
	}
	logDir := filepath.Join(string(defaults.ResolveStateDirPathWith(dir)), "logs")
	if entries, readErr := os.ReadDir(logDir); readErr == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "push--timing--") {
				t.Errorf("timing log %s written without --timing", e.Name())
			}
		}
	}
}

// TestPushCmd_JSONOutput verifies --json produces valid, parseable JSON with
// expected top-level keys.
func TestPushCmd_JSONOutput(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	output, err := executePushCmd(t, dir, []string{"--dry-run", "--json"})
	if err != nil {
		t.Fatalf("push --dry-run --json: unexpected error: %v\noutput: %s", err, output)
	}

	// The dry-run header is printed before the JSON body even with --json.
	// Extract only the JSON portion (starts at the first '{').
	jsonStart := strings.Index(output, "{")
	if jsonStart < 0 {
		t.Fatalf("--json output contains no JSON object; got: %q", output)
	}
	jsonPart := output[jsonStart:]

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(jsonPart), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\njson portion: %q", err, jsonPart)
	}

	// The empty-store case returns {"empty_reason": "...", "sessions": null, ...}.
	// Verify the required top-level keys are present.
	for _, key := range []string{"new", "updated", "skipped", "errors", "held"} {
		if _, ok := result[key]; !ok {
			t.Errorf("JSON output missing key %q; got keys: %v", key, keysOf(result))
		}
	}
}

// TestPushCmd_VerboseShowsPerSessionRows verifies that --verbose output
// contains per-session detail rows when sessions exist, and that without
// --verbose only the summary line appears.
func TestPushCmd_VerboseShowsPerSessionRows(t *testing.T) {
	t.Parallel()
	// We test printPushSummary directly because the CLI path requires real
	// credentials + network, and this behaviour is purely output formatting.
	result := &push.PushResult{
		New:     1,
		Skipped: 0,
		Sessions: []push.SessionPushResult{
			{
				SessionID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
				HostSlug:  "github.com-user-repo",
				Title:     "My session",
				Status:    push.PushStatusNew,
			},
		},
	}

	// verbose=false: only the summary line, no per-session rows.
	var bufQuiet bytes.Buffer
	printPushSummary(&bufQuiet, result, false, false)
	quietOut := bufQuiet.String()
	if strings.Contains(quietOut, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa") {
		t.Errorf("non-verbose output should NOT contain session ID row; got: %s", quietOut)
	}
	if !strings.Contains(quietOut, "Summary:") {
		t.Errorf("non-verbose output should contain 'Summary:' line; got: %s", quietOut)
	}

	// verbose=true: per-session rows appear before the summary line.
	var bufVerbose bytes.Buffer
	printPushSummary(&bufVerbose, result, false, true)
	verboseOut := bufVerbose.String()
	if !strings.Contains(verboseOut, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa") {
		t.Errorf("verbose output should contain session ID row; got: %s", verboseOut)
	}
	if !strings.Contains(verboseOut, "Summary:") {
		t.Errorf("verbose output should also contain 'Summary:' line; got: %s", verboseOut)
	}
}

// TestPushCmd_VerboseCLIFlag verifies that running push --dry-run --verbose
// succeeds without error (integration smoke test for the flag wiring).
func TestPushCmd_VerboseCLIFlag(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	output, err := executePushCmd(t, dir, []string{"--dry-run", "--verbose"})
	if err != nil {
		t.Fatalf("push --dry-run --verbose: unexpected error: %v\noutput: %s", err, output)
	}
	// With an empty store the EmptyReason is printed; no crash from --verbose.
	if output == "" {
		t.Error("push --dry-run --verbose: expected non-empty output")
	}
}

// TestPushCmd_IndividualMethodError verifies that push.method=individual in
// config (without --source-provider) returns a clear error message.
func TestPushCmd_IndividualMethodError(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir; config passed via --config.
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	// Write a config file with push.method=individual.
	peasantDir := filepath.Join(dir, string(defaults.AppName))
	if err := os.MkdirAll(peasantDir, 0700); err != nil {
		t.Fatalf("create peasant config dir: %v", err)
	}
	cfgContent := `version: 1
push:
  method: individual
  visibility: private
`
	cfgPath := filepath.Join(peasantDir, string(defaults.Config.FileName))
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// The individual method guard fires before the empty-store check,
	// so no sessions need to be seeded.

	output, err := executePushCmd(t, dir, []string{"--config=" + cfgPath})
	if err == nil {
		t.Fatalf("expected error for push.method=individual, got nil; output: %s", output)
	}
	if !strings.Contains(err.Error(), "individual") {
		t.Errorf("error should mention 'individual', got: %v", err)
	}
}

// TestPushCmd_Help verifies that peasant push --help shows usage information.
func TestPushCmd_Help(t *testing.T) {
	t.Parallel()
	output, err := executePushCmd(t, t.TempDir(), []string{"--help"})
	if err != nil {
		t.Fatalf("push --help: unexpected error: %v\noutput: %s", err, output)
	}

	// Verify key sections are present.
	if !strings.Contains(output, "push") {
		t.Error("help output should mention 'push'")
	}
	if !strings.Contains(output, "--force") {
		t.Error("help output should mention '--force' flag")
	}
}

// --- Push stage output tests ---
//
// These tests verify the output formatting functions used to report both staged
// results, mirroring the existing TestPushCmd_VerboseShowsPerSessionRows pattern.

// TestPushStages_BothSucceed verifies that when both transcript push and
// annotation push succeed, printAnnotationSummary outputs the combined totals.
func TestPushStages_BothSucceed(t *testing.T) {
	t.Parallel()
	result := &push.PushResult{
		New:     2,
		Updated: 1,
		Skipped: 0,
		Errors:  0,
		Sessions: []push.SessionPushResult{
			{SessionID: "sess-1", HostSlug: "github.com-user-repo", Status: push.PushStatusNew},
			{SessionID: "sess-2", HostSlug: "github.com-user-repo", Status: push.PushStatusNew},
			{SessionID: "sess-3", HostSlug: "github.com-user-repo", Status: push.PushStatusUpdated},
		},
	}
	annSummary := &push.AnnotationPushSummary{
		Total:   5,
		Created: 4,
		Updated: 1,
		Skipped: 0,
		Errors:  0,
	}

	var buf bytes.Buffer
	printPushSummary(&buf, result, false, false)
	printAnnotationSummary(&buf, annSummary, nil, false)
	out := buf.String()

	// Transcript summary must appear.
	if !strings.Contains(out, "2 new") {
		t.Errorf("output should contain transcript new count; got: %s", out)
	}
	if !strings.Contains(out, "1 updated") {
		t.Errorf("output should contain transcript updated count; got: %s", out)
	}

	// Annotation summary must appear.
	if !strings.Contains(out, "Annotations:") {
		t.Errorf("output should contain 'Annotations:' line; got: %s", out)
	}
	if !strings.Contains(out, "4 created") {
		t.Errorf("output should contain annotation created count; got: %s", out)
	}
	if !strings.Contains(out, "1 updated") {
		t.Errorf("output should contain annotation updated count; got: %s", out)
	}
}

// TestPushStages_AnnotationFails_TranscriptSucceeds verifies that when the
// annotation push fails but transcripts succeed, the annotation error is reported
// clearly alongside the transcript summary.
func TestPushStages_AnnotationFails_TranscriptSucceeds(t *testing.T) {
	t.Parallel()
	result := &push.PushResult{
		New:     1,
		Skipped: 0,
		Errors:  0,
	}
	// annSummary is non-nil because PushAnnotations returns a partial summary even on error.
	annSummary := &push.AnnotationPushSummary{Total: 3}
	annErr := fmt.Errorf("annotation server: connection refused")

	var buf bytes.Buffer
	printPushSummary(&buf, result, false, false)
	printAnnotationSummary(&buf, annSummary, annErr, false)
	out := buf.String()

	// Transcript summary should succeed.
	if !strings.Contains(out, "1 new") {
		t.Errorf("output should contain transcript new count; got: %s", out)
	}

	// Annotation error must be surfaced with user-facing context.
	if !strings.Contains(out, "Annotations: push failed") {
		t.Errorf("output should contain 'Annotations: push failed' line; got: %s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("output should contain the annotation error message; got: %s", out)
	}
	if !strings.Contains(out, "not affected") {
		t.Errorf("output should reassure user that transcript push was not affected; got: %s", out)
	}
}

// TestPushStages_TranscriptFails_AnnotationSucceeds verifies that when
// transcript push fails but annotations succeed, the annotation summary is
// still reported (neither push rolls back the other).
func TestPushStages_TranscriptFails_AnnotationSucceeds(t *testing.T) {
	t.Parallel()
	// Transcript push returned a result with errors.
	result := &push.PushResult{
		New:    0,
		Errors: 1,
		Sessions: []push.SessionPushResult{
			{
				SessionID: "sess-fail",
				HostSlug:  "github.com-user-repo",
				Status:    push.PushStatusError,
				Error:     fmt.Errorf("connection refused"),
			},
		},
	}
	annSummary := &push.AnnotationPushSummary{
		Total:   4,
		Created: 4,
		Errors:  0,
	}

	var buf bytes.Buffer
	printPushSummary(&buf, result, false, false)
	printAnnotationSummary(&buf, annSummary, nil, false)
	out := buf.String()

	// Transcript error is reflected in the summary counts.
	if !strings.Contains(out, "1 error") {
		t.Errorf("output should contain '1 error' in transcript summary; got: %s", out)
	}

	// Annotation summary still appears — annotation push succeeded independently.
	if !strings.Contains(out, "Annotations:") {
		t.Errorf("output should contain 'Annotations:' line even when transcript push had errors; got: %s", out)
	}
	if !strings.Contains(out, "4 created") {
		t.Errorf("output should contain annotation created count; got: %s", out)
	}
}

// TestPushStages_DryRun_AnnotationSummary verifies that dry-run mode reports
// annotation counts correctly via printAnnotationSummary.
func TestPushStages_DryRun_AnnotationSummary(t *testing.T) {
	t.Parallel()
	annSummary := &push.AnnotationPushSummary{Total: 7}

	var buf bytes.Buffer
	printAnnotationSummary(&buf, annSummary, nil, true /* dryRun */)
	out := buf.String()

	if !strings.Contains(out, "dry run") {
		t.Errorf("dry-run annotation output should mention 'dry run'; got: %s", out)
	}
	if !strings.Contains(out, "7") {
		t.Errorf("dry-run annotation output should contain count; got: %s", out)
	}
}

// TestPushStages_JSONOutput_IncludesAnnotations verifies that --json output
// includes an "annotations" key when an annotation summary is available.
func TestPushStages_JSONOutput_IncludesAnnotations(t *testing.T) {
	t.Parallel()
	result := &push.PushResult{
		New:     1,
		Updated: 0,
		Skipped: 0,
		Errors:  0,
	}
	annSummary := &push.AnnotationPushSummary{
		Total:   2,
		Created: 2,
	}

	var buf bytes.Buffer
	if err := printPushJSON(&buf, result, annSummary, nil); err != nil {
		t.Fatalf("printPushJSON: %v", err)
	}

	var out map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal JSON output: %v", err)
	}

	if _, ok := out["annotations"]; !ok {
		t.Errorf("JSON output should include 'annotations' key; got keys: %v", keysOf(out))
	}
	ann, ok := out["annotations"].(map[string]interface{})
	if !ok {
		t.Fatalf("'annotations' should be an object; got: %T", out["annotations"])
	}
	if ann["total"] != float64(2) {
		t.Errorf("annotations.total: want 2, got %v", ann["total"])
	}
	if ann["created"] != float64(2) {
		t.Errorf("annotations.created: want 2, got %v", ann["created"])
	}
}

// --- Public consent prompt tests ---

// TestPromptPublicConsent_AutoConfirm verifies that --yes skips the prompt and returns true.
func TestPromptPublicConsent_AutoConfirm(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	// Reader is empty — autoConfirm should not read from it at all.
	consented, err := promptPublicConsent(bytes.NewBufferString(""), &w, true /* autoConfirm */, false /* isTTY */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !consented {
		t.Error("expected consented=true with autoConfirm, got false")
	}
}

// TestPromptPublicConsent_NonTTYWithoutYes verifies that non-TTY without --yes aborts (exit 0, no error).
func TestPromptPublicConsent_NonTTYWithoutYes(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	consented, err := promptPublicConsent(bytes.NewBufferString(""), &w, false /* autoConfirm */, false /* isTTY */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consented {
		t.Error("expected consented=false for non-TTY without --yes, got true")
	}
	if !strings.Contains(w.String(), "--yes") {
		t.Errorf("non-TTY abort message should mention '--yes'; got: %s", w.String())
	}
}

// TestPromptPublicConsent_TTY_AcceptsY verifies that "y" input returns true.
func TestPromptPublicConsent_TTY_AcceptsY(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	consented, err := promptPublicConsent(bytes.NewBufferString("y\n"), &w, false /* autoConfirm */, true /* isTTY */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !consented {
		t.Error("expected consented=true for 'y' input, got false")
	}
}

// TestPromptPublicConsent_TTY_AcceptsUpperY verifies that "Y" input returns true.
func TestPromptPublicConsent_TTY_AcceptsUpperY(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	consented, err := promptPublicConsent(bytes.NewBufferString("Y\n"), &w, false /* autoConfirm */, true /* isTTY */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !consented {
		t.Error("expected consented=true for 'Y' input, got false")
	}
}

// TestPromptPublicConsent_TTY_DeclineN verifies that "N" input returns false (exit 0).
func TestPromptPublicConsent_TTY_DeclineN(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	consented, err := promptPublicConsent(bytes.NewBufferString("N\n"), &w, false /* autoConfirm */, true /* isTTY */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consented {
		t.Error("expected consented=false for 'N' input, got true")
	}
}

// TestPromptPublicConsent_TTY_DeclineEmpty verifies that empty/Enter input returns false.
func TestPromptPublicConsent_TTY_DeclineEmpty(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	consented, err := promptPublicConsent(bytes.NewBufferString("\n"), &w, false /* autoConfirm */, true /* isTTY */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consented {
		t.Error("expected consented=false for empty input, got true")
	}
}

// TestPromptPublicConsent_TTY_EOFEOF verifies that EOF without input returns false (exit 0, no error).
func TestPromptPublicConsent_TTY_EOF(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	consented, err := promptPublicConsent(bytes.NewBufferString(""), &w, false /* autoConfirm */, true /* isTTY */)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consented {
		t.Error("expected consented=false for EOF, got true")
	}
}

// TestPushCmd_VisibilityPrecedence verifies the flag → config → default chain the
// push command resolves through, and that what it resolves is what this version
// can apply. The per-visibility resolution itself, over the whole contract set,
// is driven from a fixture in internal/config.
func TestPushCmd_VisibilityPrecedence(t *testing.T) {
	t.Parallel()

	baseCfg := &config.Config{}

	// The flag takes precedence over the configuration, and what the user asked
	// for survives resolution and is applied through the owner update.
	fromFlag := config.EffectiveVisibility(config.VisibilityPublic, baseCfg)
	if fromFlag.Configured != config.VisibilityPublic {
		t.Errorf("flag override: configured want %q, got %q", config.VisibilityPublic, fromFlag.Configured)
	}
	if fromFlag.Effective != config.VisibilityPublic {
		t.Errorf("flag override: this version publishes %q, got %q", config.VisibilityPublic, fromFlag.Effective)
	}

	// The configuration is used when there is no flag.
	baseCfg.Push.Visibility = config.VisibilityPublic
	fromConfig := config.EffectiveVisibility("", baseCfg)
	if fromConfig != fromFlag {
		t.Errorf("the same value asked for by config resolved to %+v but by flag to %+v", fromConfig, fromFlag)
	}

	// Neither set: the default, and nothing to disclose.
	baseCfg.Push.Visibility = ""
	defaulted := config.EffectiveVisibility("", baseCfg)
	if defaulted.Configured != config.VisibilityPrivate || defaulted.Downgraded() {
		t.Errorf("default: want private with nothing disclosed, got %+v", defaulted)
	}
}

// TestPushCmd_RejectsAnUnknownVisibility proves the flag is validated against the
// contract's closed set, the way --license already was. It used to accept any
// string: a typo was taken as a visibility, quietly resolved to the default, and
// then reported as applied.
func TestPushCmd_RejectsAnUnknownVisibility(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	output, err := executePushCmd(t, dir, []string{"--visibility=bogus", "--dry-run"})
	if err == nil {
		t.Fatalf("an unknown visibility must be refused, not silently resolved; output: %s", output)
	}
	for _, want := range []string{"bogus", string(config.VisibilityPrivate), string(config.VisibilityGroup), string(config.VisibilityPublic)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q so the user can see what is accepted; got: %v", want, err)
		}
	}
}

// TestPushCmd_PublicConsent_ConsentGateExecutes verifies end-to-end that an
// unavailable public mode is disclosed and proceeds privately without false consent.
//
// Setup: empty store + valid credentials. With 0 sessions the push pipeline returns
// EmptyReason without making any HTTP calls, so no real network access is needed.
// --yes bypasses the interactive prompt (non-TTY autoConfirm path).
func TestPushCmd_PublicConsent_ConsentGateExecutes(t *testing.T) {
	// PARALLEL: credential gate reads via --config-dir (per-invocation flag).
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	// Run push with public visibility and --yes to auto-confirm the consent gate.
	// No --dry-run so the consent gate is not skipped.
	output, err := executePushCmd(t, dir, []string{"--visibility=public", "--yes"})
	if err != nil {
		t.Fatalf("push --visibility=public --yes on empty store: unexpected error: %v\noutput: %s", err, output)
	}

	for _, forbidden := range []string{"visibility is not yet implemented", "downgraded safely to private"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("output must not claim a public publish, stating %q; got: %s", forbidden, output)
		}
	}

	// After consent the pipeline runs; with an empty store it returns EmptyReason.
	if !strings.Contains(output, "Nothing to push") && !strings.Contains(output, "All sessions") {
		t.Errorf("pipeline empty-state message should appear; got: %s", output)
	}
}

// TestWritePushErrorLog verifies that upload failures are dumped to a timestamped
// log under the XDG state dir, containing only the failed sessions' error detail.
func TestWritePushErrorLog(t *testing.T) {
	t.Parallel()
	stateHome := t.TempDir()

	result := &push.PushResult{
		Errors: 1,
		Sessions: []push.SessionPushResult{
			{
				SessionID: "sess-fail", HostSlug: "host-a",
				Status: push.PushStatusError,
				Error:  fmt.Errorf("upload: village returned 400: bad content version"),
			},
			{SessionID: "sess-ok", HostSlug: "host-a", Status: push.PushStatusNew},
		},
	}
	annErr := fmt.Errorf("annotations endpoint unreachable")

	path, err := writePushErrorLog(result, nil, annErr, "https://api.village.example", stateHome)
	if err != nil {
		t.Fatalf("writePushErrorLog: %v", err)
	}

	wantDir := filepath.Join(stateHome, "peasant", "logs")
	if !strings.HasPrefix(path, wantDir+string(filepath.Separator)) {
		t.Errorf("log path %q not under %q", path, wantDir)
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "push--error--") || !strings.HasSuffix(base, ".log") {
		t.Errorf("log filename %q does not match push--error--<datetime>.log", base)
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read log %s: %v", path, readErr)
	}
	content := string(data)
	if !strings.Contains(content, "host-a/sess-fail") ||
		!strings.Contains(content, "village returned 400: bad content version") {
		t.Errorf("log missing failed-session detail; got:\n%s", content)
	}
	if strings.Contains(content, "sess-ok") {
		t.Errorf("log must contain only failed sessions, found sess-ok; got:\n%s", content)
	}
	if !strings.Contains(content, "annotations error: annotations endpoint unreachable") {
		t.Errorf("log missing annotation error; got:\n%s", content)
	}
	if !strings.Contains(content, "https://api.village.example") {
		t.Errorf("log missing village URL; got:\n%s", content)
	}
}
