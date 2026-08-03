package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
)

// TestPushCmd_RuntimeFailureIsActionableFromAHook covers the two things a user
// sees when an installed hook's upload cannot even start.
//
// A hook fires on every commit, so a runtime failure that dumps the whole flag
// listing buries the one line that matters — and the line itself has to name a
// command that exists. `peasant login` is not one.
func TestPushCmd_RuntimeFailureIsActionableFromAHook(t *testing.T) {
	t.Parallel()
	// No credentials written: the auth gate is the first runtime failure a hook
	// meets, and the one an expired login produces in practice.
	dir := t.TempDir()

	stdout, stderr, err := executePushCmdSeparate(t, dir, []string{"--non-interactive", "--quiet"})
	if err == nil {
		t.Fatalf("a push without credentials must fail; stdout=%s stderr=%s", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "peasant village login") {
		t.Errorf("the error must name a command that exists; got: %v", err)
	}
	if strings.Contains(err.Error(), "'peasant login'") {
		t.Errorf("'peasant login' is not a command; got: %v", err)
	}
	if strings.Contains(stderr, "Usage:") || strings.Contains(stdout, "Usage:") {
		t.Errorf("a runtime failure must not print the flag listing into every commit; stdout=%s\nstderr=%s", stdout, stderr)
	}
}

// TestPushCmd_QuietDoesNotAnnounceAnUnconfiguredInstall holds --quiet to what it
// promises: errors, and one final result line.
//
// Not having run kickstart is the DEFAULT state, not an error and not a result.
// The notice printed on every commit and every push for every user in it, and
// the remedy it named — an interactive wizard — needs a terminal a git hook does
// not have, so it could not even be acted on from where it appeared.
func TestPushCmd_QuietDoesNotAnnounceAnUnconfiguredInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // no config.yaml, which is what a fresh install looks like
	writeTestCredentials(t, dir)

	const notice = "no config found at"
	quietOut, quietErr, _ := executePushCmdSeparate(t, dir, []string{"--non-interactive", "--quiet"})
	if strings.Contains(quietErr, notice) || strings.Contains(quietOut, notice) {
		t.Errorf("--quiet printed the unconfigured notice into every commit; stdout=%s\nstderr=%s", quietOut, quietErr)
	}

	// It is still worth saying when the user asked for output, so the fix is a
	// suppression rather than a deletion.
	_, loudErr, _ := executePushCmdSeparate(t, dir, []string{"--non-interactive"})
	if !strings.Contains(loudErr, notice) {
		t.Errorf("a non-quiet run must still say the install is unconfigured; stderr=%s", loudErr)
	}
}

// TestPushCmd_ConfigDirReachesConfigLoading proves the override a generated hook
// binds actually changes what that hook reads.
//
// The hook's own header states that an explicit config directory is bound into
// the command it runs, and it is — but config loading read only --config, whose
// DEFAULT value is resolved at process start. So a hook installed with
// --config-dir ran on default configuration: the user's redaction level,
// selection, and visibility were all silently ignored. That is a consent
// problem, not a documentation one.
func TestPushCmd_ConfigDirReachesConfigLoading(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	// A config in the bound directory, at the path --config-dir implies. Its
	// redaction level is one the default config never produces, so reading it is
	// provable from the command's own output.
	configDir := filepath.Join(dir, defaults.AppName.String())
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bound := filepath.Join(configDir, string(defaults.Config.FileName))
	if err := os.WriteFile(bound, []byte("version: 1\nredaction:\n  level: minimal\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, _ := executePushCmdSeparate(t, dir, []string{"--non-interactive"})
	// The observable is the transition notice a configured minimal produces, which
	// the DEFAULT configuration never produces - so seeing it proves the bound
	// directory was read. The expected text is the shared disclosure rather than a
	// phrase retyped here: push used to word this itself, which is how the same
	// configuration came to be announced differently by different surfaces. The
	// wording itself is held to its contract by the policy corpus; this assertion
	// only needs it to be the shared one and to be present at all.
	want := config.ResolveRedactionPolicy(redact.Minimal).Disclosure()
	if want == "" {
		t.Fatal("a configured minimal level no longer produces a transition notice, so this test has no observable left " +
			"to prove the bound config directory was read; pick another setting the default configuration never writes")
	}
	if !strings.Contains(stderr, want) {
		t.Errorf("the bound config directory was not read: a hook bound with --config-dir would run on default configuration.\nwant notice:\n%s\nstderr=%s",
			want, stderr)
	}
}

// TestPushCmd_RepositoryFlagHelpDescribesProjectIdentity keeps the flag help
// honest. The scope is a canonical project identity, so two clones of one origin
// share it; describing it as "this Git repository" would promise a path-level
// boundary the implementation does not have.
func TestPushCmd_RepositoryFlagHelpDescribesProjectIdentity(t *testing.T) {
	t.Parallel()
	usage := BuildPushCommand().Flags().Lookup("repository").Usage
	for _, want := range []string{"canonical project identity", "normalized origin remote", "no origin remote"} {
		if !strings.Contains(usage, want) {
			t.Errorf("--repository help must state %q; got: %s", want, usage)
		}
	}
}

// TestPushCmd_RepositoryResolutionSurfacesGitsOwnDiagnosis proves a bad
// --repository reports what git said rather than a bare exit status. From a hook
// this is the difference between an actionable message and "exit status 128".
func TestPushCmd_RepositoryResolutionSurfacesGitsOwnDiagnosis(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)
	notARepository := filepath.Join(dir, "not-a-repository")
	if err := os.MkdirAll(notARepository, 0o700); err != nil {
		t.Fatal(err)
	}

	_, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--repository", notARepository})
	if err == nil {
		t.Fatal("pushing scoped to a directory that is not a repository must fail")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("the error must carry git's own diagnosis, not a bare exit status; got: %v", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Errorf("the error must not degrade to a bare exit status; got: %v", err)
	}
}

// writeTestCredentialsFor writes valid credentials pointed at villageURL, so a
// test can aim the push at a listener it controls.
func writeTestCredentialsFor(t *testing.T, dir, villageURL string) {
	t.Helper()
	writeTestCredentials(t, dir)
	credPath := filepath.Join(string(defaults.ResolveConfigDirPathWith(dir)), string(defaults.CredentialsFile))
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	var creds map[string]any
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("parse credentials: %v", err)
	}
	creds["village_url"] = villageURL
	rewritten, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	if err := os.WriteFile(credPath, rewritten, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

// seedPushableSession puts one eligible session in the store at dir, so a push
// reaches the network instead of returning early with nothing to send.
func seedPushableSession(t *testing.T, dir string) {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entry := makeCmdStoreEntry(t, "cccc3333-cccc-4ccc-8ccc-cccccccccccc", "github.com-user-repo",
		"git@github.com:user/repo.git", "main", 1700000000000)
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
}

// seedUploadableSession seeds a session that survives the whole pre-flight and
// is actually UPLOADED: the store row plus the on-disk metadata the pipeline
// reads. seedPushableSession stops at the store row, which is enough to reach
// the network but fails at "read metadata" before any transcript is sent.
//
// It returns the session id and a config whose output base path is this test's
// own directory, so parallel tests never share transcripts.
func seedUploadableSession(t *testing.T, dir, sessionID string) string {
	t.Helper()
	const hostSlug = "github.com-user-repo"
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entry := makeCmdStoreEntry(t, sessionID, hostSlug, "git@github.com:user/repo.git", "main", 1700000000000)
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}

	basePath := filepath.Join(dir, "peasant-sync")
	sessionDir := filepath.Join(basePath, hostSlug, sessionID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(entry.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, sessionID+"--metadata.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return writeCfg(t, dir, "uploadable.yaml", "version: 1\noutput:\n  basePath: "+basePath+
		"\npush:\n  method: all\n  visibility: private\n")
}

// TestPushCmd_RejectsAnEmptyRepositoryScope proves the containment flag never
// degrades into no containment.
//
// An empty --repository used to be indistinguishable from not passing the flag
// at all, so a scripted or mis-expanded value silently pushed every configured
// session — the exact opposite of what the flag is for, with no warning.
func TestPushCmd_RejectsAnEmptyRepositoryScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	stdout, stderr, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--repository", ""})
	if err == nil {
		t.Fatalf("an empty --repository must be rejected, not ignored; stdout=%s stderr=%s", stdout, stderr)
	}
	for _, want := range []string{"empty value", "nothing was uploaded", "--repository ."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must state %q; got: %v", want, err)
		}
	}
	if strings.Contains(stdout, "would push") {
		t.Errorf("nothing may be pushed or forecast when the scope was rejected; stdout: %s", stdout)
	}
}

// TestPushCmd_RejectsANegativeTimeout keeps the upload budget meaningful.
func TestPushCmd_RejectsANegativeTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	_, _, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--timeout", "-1s"})
	if err == nil {
		t.Fatal("a negative --timeout must be rejected")
	}
	if !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("the error must explain why; got: %v", err)
	}
}

// TestPushCmd_TimeoutBoundsTheWholeUpload proves the overall budget exists and
// is enforced end to end.
//
// The village client's own timeout is PER REQUEST and a push issues several in
// sequence, so a village that accepts a connection and never answers stalls for
// minutes with no bound — once per commit, three times through a three-commit
// rebase. The budget caps the whole command instead, and says so.
func TestPushCmd_TimeoutBoundsTheWholeUpload(t *testing.T) {
	t.Parallel()
	// A listener that accepts and never responds: the realistic VPN or
	// captive-portal failure. A refused connection fails fast and is fine.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	dir := t.TempDir()
	writeTestCredentialsFor(t, dir, "http://"+listener.Addr().String())
	seedPushableSession(t, dir)

	const budget = 300 * time.Millisecond
	started := time.Now()
	_, _, err = executePushCmdSeparate(t, dir, []string{"--quiet", "--timeout", budget.String()})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a push that ran out of its budget must report it, not exit clean")
	}
	if !strings.Contains(err.Error(), "ran out of its "+budget.String()+" budget") {
		t.Errorf("the error must name the budget rather than surface a bare deadline; got: %v", err)
	}
	if !strings.Contains(err.Error(), "--timeout") {
		t.Errorf("the error must say how to finish a large upload; got: %v", err)
	}
	// Generous ceiling: the point is that it is bounded at all. Without a budget
	// this same run takes minutes (60s per request, several requests).
	if elapsed > 30*time.Second {
		t.Errorf("the budget did not bound the run: took %s for a %s budget", elapsed, budget)
	}
}

// TestPushCmd_AnUploadWithoutATerminalReceiptRemainsTargetedForRetry proves a
// transport-level acceptance never advances local applied state.
//
// A village that answers headers immediately and then stalls its response body —
// an ordinary proxy, load balancer, or VPN — accepts the transcript, and the
// upload budget then expires while peasant is still reading the answer. The
// "mark pushed" write used to run on that same expiring context, so the work
// that DID happen was never recorded and every subsequent commit re-sent the
// identical full transcript, forever.
func TestPushCmd_AnUploadWithoutATerminalReceiptRemainsTargetedForRetry(t *testing.T) {
	t.Parallel()
	var publishes atomic.Int64
	// Headers out at once, body withheld: the transcript is on the village and
	// the client is left waiting.
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/transcripts/publish") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		publishes.Add(1)
		w.WriteHeader(http.StatusCreated)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(village.Close)

	dir := t.TempDir()
	writeTestCredentialsFor(t, dir, village.URL)
	cfgPath := seedUploadableSession(t, dir, "cccc3333-cccc-4ccc-8ccc-cccccccccccc")

	// The stall is indefinite, so the budget always expires; it only has to be
	// long enough for the headers to arrive first, which is the state under
	// test. Generous rather than tight so a loaded machine cannot turn "the
	// village accepted it" into "the request never got there".
	const budget = 3 * time.Second
	for commit := 1; commit <= 3; commit++ {
		stdout, stderr, err := executePushCmdSeparate(t, dir,
			[]string{"--non-interactive", "--quiet", "--config=" + cfgPath, "--timeout", budget.String()})
		if err == nil {
			t.Fatalf("attempt %d must report the missing terminal receipt; stdout=%s stderr=%s", commit, stdout, stderr)
		}
	}
	if got := publishes.Load(); got != 3 {
		t.Errorf("the village received %d attempts, want one targeted retry per explicit invocation until a terminal receipt exists", got)
	}
}

// TestPushCmd_ABudgetThatExpiresEarlyIsStillReportedAsTheBudget covers the
// failures that happen BEFORE any upload.
//
// The budget wrapper used to be applied only at the tail of the command, so an
// early return under an expired cap escaped raw: the store answered "sqlite:
// prepare: interrupted", and repository resolution reported that a perfectly
// valid git repository was not one. Inside a hook that message is the entire
// diagnosis the user gets.
func TestPushCmd_ABudgetThatExpiresEarlyIsStillReportedAsTheBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)
	seedPushableSession(t, dir)
	repo := filepath.Join(dir, "repository")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksGit(t, repo, "", "init", "--quiet", "--initial-branch=main")

	// A cap small enough that it expires during the store query or the git call,
	// long before anything is uploaded.
	_, _, err := executePushCmdSeparate(t, dir,
		[]string{"--non-interactive", "--quiet", "--timeout", "1ms", "--repository", repo})
	if err == nil {
		t.Fatal("a push whose budget expired must fail")
	}
	for _, want := range []string{"ran out of its 1ms budget", "--timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must be reported as the budget, stating %q; got: %v", want, err)
		}
	}
	// The two things the raw errors got wrong: a valid repository reported as
	// not one, and a local database error offered as the cause.
	if strings.Contains(err.Error(), "pass a path inside an existing Git repository") {
		t.Errorf("%s IS a git repository; a budget expiry must not be reported as a bad path; got: %v", repo, err)
	}
	if strings.Contains(err.Error(), "sqlite") {
		t.Errorf("a local database error must not be offered as the cause of a network budget expiring; got: %v", err)
	}
}

// TestPushCmd_RepositoryScopeSuppressesAnotherRepositorysWithheldNotice proves
// the withheld notice is narrowed by the repository scope.
//
// A branch-selection conflict belongs to the repository whose sessions it is
// about. Reporting it from a hook installed somewhere else means an unrelated
// commit prints a warning about a repository the user was not working in, on
// every single commit, forever.
func TestPushCmd_RepositoryScopeSuppressesAnotherRepositorysWithheldNotice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	scoped := filepath.Join(dir, "work", "alpha")
	if err := os.MkdirAll(scoped, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksGit(t, scoped, "", "init", "--quiet", "--initial-branch=main")

	const conflictRemote = "git@github.com:user/conflict.git"
	const scopedID = "aaaa1111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const conflictedID = "bbbb2222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &ingest.ExecGitResolver{}
	scopedHash, _, err := ingest.DeriveProjectIdentifiersWithGit(t.Context(), db.InstallationSalt(), resolver, "", scoped)
	if err != nil {
		t.Fatal(err)
	}
	conflictHash, _, err := ingest.DeriveProjectIdentifiers(db.InstallationSalt(), conflictRemote, "/elsewhere/conflict")
	if err != nil {
		t.Fatal(err)
	}

	scopedEntry := makeCmdStoreEntry(t, scopedID, "local-work-alpha", "", "main", 1700000000000)
	scopedEntry.Metadata.Project = ingest.ProjectInfo{Hash: scopedHash, Name: "alpha", FilePath: scoped}
	conflicted := makeCmdStoreEntry(t, conflictedID, "github.com-user-conflict", conflictRemote, "main", 1700000060000)
	conflicted.Metadata.Project = ingest.ProjectInfo{Hash: conflictHash, Name: "conflict", FilePath: "/elsewhere/conflict"}
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{scopedEntry, conflicted}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Two rules on the SAME remote with disjoint branch sets: one admits main,
	// the other rejects it, so the session is withheld rather than dropped.
	cfgPath := writeCfg(t, dir, "withheld-scope.yaml", `version: 1
push:
  method: all
  visibility: private
selection:
  mode: selected
  harnesses:
    claude-code:
      projects:
        - gitRemote: `+conflictRemote+`
          branches:
            - main
        - gitRemote: `+conflictRemote+`
          branches:
            - feature
        - name: alpha
`)

	const notice = "withheld from push"
	_, unscopedStderr, err := executePushCmdSeparate(t, dir, []string{"--dry-run", "--json", "--config=" + cfgPath})
	if err != nil {
		t.Fatalf("unscoped push: %v (stderr=%s)", err, unscopedStderr)
	}
	if !strings.Contains(unscopedStderr, notice) {
		t.Fatalf("the conflict must be surfaced when nothing narrows it away; stderr:\n%s", unscopedStderr)
	}

	scopedStdout, scopedStderr, err := executePushCmdSeparate(t, dir,
		[]string{"--dry-run", "--json", "--config=" + cfgPath, "--repository", scoped})
	if err != nil {
		t.Fatalf("scoped push: %v (stderr=%s)", err, scopedStderr)
	}
	if strings.Contains(scopedStderr, notice) {
		t.Errorf("a push scoped to %s must not report another repository's branch conflict; stderr:\n%s", scoped, scopedStderr)
	}
	if ids := scopeSessionsIn(t, scopedStdout); !ids[scopedID] || ids[conflictedID] {
		t.Errorf("the scoped push kept %v, want only %s", sortedKeys(ids), scopedID)
	}
}
