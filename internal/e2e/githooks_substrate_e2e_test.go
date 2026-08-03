//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
)

// This file is the single substrate every repository-scoped hook case builds on:
// one sandbox convention, one git runner, one fixture reseed. There were briefly
// two of each, with different cwd-rewrite semantics, and that is exactly how the
// end-to-end oracle can go vacuous while a substrate test stays green.

// disposableSandbox is one throwaway environment: isolated XDG directories, an
// isolated HOME, isolated global and system Git configuration, and every
// repository a case creates, all under a single root.
//
// The root lives under the harness's own test area beneath the RESOLVED state
// directory (the convention sandbox.go documents), so the start-time prune
// reaches it after a hard crash and the developer-state guard can exclude
// exactly one subtree.
type disposableSandbox struct {
	root       string
	dataHome   string
	configHome string
	stateHome  string
	// environment is the COMPLETE environment every subprocess runs with,
	// built once. Runners pass it through unchanged; nothing re-appends
	// os.Environ(), so what a case is isolated from can be read in one place.
	environment []string
}

func TestGitHooksSubstrate_SeedsRepositoryIdentity(t *testing.T) {
	peasantBinary := buildPeasant(t)
	sandbox := newDisposableSandbox(t, peasantBinary)
	repositoryRoot := sandbox.initRepository(t, "githooks-test")
	fixtureRoot := reseedClaudeFixture(t, claudeReseed{
		Destination:              filepath.Join(sandbox.root, "fixtures", "githooks-test"),
		RecordedWorkingDirectory: repositoryRoot,
	})
	writeClaudeOnlyConfig(t, sandbox, fixtureRoot)

	runPeasantInSandbox(t, peasantBinary, sandbox, "ingest", "--include-active")

	dbPath := filepath.Join(sandbox.dataHome, string(defaults.AppName), "peasant.db")
	db, err := store.Open(dbPath, store.WithPoolSize(1))
	if err != nil {
		t.Fatalf("open the disposable repository's seeded store at %s: %v", dbPath, err)
	}
	t.Cleanup(func() { db.Close() })

	scope := resolveDisposableRepositoryScope(t, db, repositoryRoot)
	if scope.Basis != push.IdentityFromPath {
		t.Fatalf("resolved repository scope basis = %q, want %q", scope.Basis, push.IdentityFromPath)
	}
	sessions, err := db.AllSessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions after reseeding the Claude fixture: %v", err)
	}
	if len(sessions) != ExpectedClaudeTranscripts {
		t.Fatalf("seeded session count = %d, want %d", len(sessions), ExpectedClaudeTranscripts)
	}
	for _, session := range sessions {
		if session.ProjectHash != string(scope.Identity) {
			t.Errorf("session %s project identity = %q, disposable repository identity = %q", session.SessionID, session.ProjectHash, scope.Identity)
		}
		if !scope.Admits(session.ProjectHash) {
			t.Errorf("resolved repository scope does not admit seeded session %s identity %q", session.SessionID, session.ProjectHash)
		}
	}
}

func newDisposableSandbox(t *testing.T, peasantBinary string) disposableSandbox {
	t.Helper()
	root := computeSandbox(string(defaults.ResolveStateDirPath()), time.Now().UnixNano())
	home := filepath.Join(root, "home")
	dataHome, configHome, stateHome := sandboxXDG(root)
	gitGlobal := filepath.Join(root, "gitconfig-global")
	gitSystem := filepath.Join(root, "gitconfig-system")
	for _, path := range []string{home, dataHome, configHome, stateHome} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create disposable sandbox directory %s: %v", path, err)
		}
	}
	for _, path := range []string{gitGlobal, gitSystem} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("create isolated Git configuration %s: %v", path, err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// The peasant binary's directory joins PATH because an installed hook
	// resolves `peasant` by name when Git runs it, exactly as a user's would.
	environment := append(os.Environ(), xdgEnvAssignments(dataHome, configHome, stateHome)...)
	environment = append(environment,
		"HOME="+home,
		"GIT_CONFIG_GLOBAL="+gitGlobal,
		"GIT_CONFIG_SYSTEM="+gitSystem,
		"PATH="+filepath.Dir(peasantBinary)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return disposableSandbox{root: root, dataHome: dataHome, configHome: configHome, stateHome: stateHome, environment: environment}
}

// initRepository creates one committed, remote-less Git repository inside the
// sandbox and returns its root. Remote-less is load-bearing: it is what makes
// the project identity derive from the worktree path, which is the scope rule
// the hook has to honour.
func (sandbox disposableSandbox) initRepository(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(sandbox.root, "repositories", name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create disposable repository %s: %v", root, err)
	}
	runGit(t, sandbox.environment, root, "init", "-q")
	runGit(t, sandbox.environment, root, "config", "user.name", "Peasant E2E")
	runGit(t, sandbox.environment, root, "config", "user.email", "peasant-e2e@invalid.test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("disposable e2e repository\n"), 0o600); err != nil {
		t.Fatalf("write disposable repository commit fixture in %s: %v", root, err)
	}
	runGit(t, sandbox.environment, root, "add", "README.md")
	runGit(t, sandbox.environment, root, "commit", "-m", "initial fixture commit")
	if remotes := strings.TrimSpace(runGit(t, sandbox.environment, root, "remote")); remotes != "" {
		t.Fatalf("disposable repository %s unexpectedly has remotes: %q", root, remotes)
	}
	return root
}

// initBareRemote creates a bare repository inside the sandbox to push at. A real
// remote is needed because Git only consults a pre-push hook when it is actually
// pushing somewhere.
func (sandbox disposableSandbox) initBareRemote(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(sandbox.root, "remotes", name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create disposable bare remote %s: %v", root, err)
	}
	runGit(t, sandbox.environment, root, "init", "--bare", "-q")
	return root
}

// claudeReseed describes one reseeded copy of the committed Claude fixture.
type claudeReseed struct {
	// Destination is the directory the rewritten copy is written to.
	Destination string
	// RecordedWorkingDirectory replaces the fixture's recorded cwd. Ingestion
	// derives the canonical project identity from it, so this one value is what
	// makes the committed fixture read as work done in a disposable repository.
	RecordedWorkingDirectory string
	// RootSessionID and SubagentSessionID rename the copy's sessions so a second
	// copy can coexist with the first in one store. Empty keeps the committed
	// identifiers.
	RootSessionID     string
	SubagentSessionID string
}

// reseedClaudeFixture copies the committed Claude fixture into
// reseed.Destination, rewriting the recorded working directory and, when asked,
// the session identifiers.
//
// The cwd rewrite decodes the first JSONL line and sets its `cwd` field, because
// that is the field and the line the Claude adapter actually reads to resolve git
// metadata. A blind textual substitution would keep working after the fixture
// stopped carrying the value the adapter reads, which is how a reseed can go
// silently inert.
func reseedClaudeFixture(t *testing.T, reseed claudeReseed) string {
	t.Helper()
	if (reseed.RootSessionID == "") != (reseed.SubagentSessionID == "") {
		t.Fatalf("reseed the committed Claude fixture into %s: renaming needs both the root and the subagent identifier, got root=%q subagent=%q\n"+
			"why: the fixture's subagent lives under the root session's directory, so renaming only one leaves two copies sharing one session identity\n"+
			"where: internal/e2e/githooks_substrate_e2e_test.go reseedClaudeFixture\n"+
			"when: preparing a second reseeded copy\n"+
			"means: the two copies would collapse into one session and the exclusion assertion would have nothing to name\n"+
			"fix: set both RootSessionID and SubagentSessionID, or neither",
			reseed.Destination, reseed.RootSessionID, reseed.SubagentSessionID)
	}
	source := FixtureSourcePath()
	rename := func(value string) string {
		if reseed.RootSessionID == "" {
			return value
		}
		value = strings.ReplaceAll(value, FixtureRootSessionID, reseed.RootSessionID)
		return strings.ReplaceAll(value, FixtureClaudeSubagentSessionID, reseed.SubagentSessionID)
	}
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(reseed.Destination, rename(relative))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".jsonl" {
			contents, err = rewriteFirstTranscriptCWD(contents, reseed.RecordedWorkingDirectory)
			if err != nil {
				return fmt.Errorf("rewrite first transcript line in %s: %w", path, err)
			}
		}
		return os.WriteFile(target, []byte(rename(string(contents))), 0o600)
	}); err != nil {
		t.Fatalf("copy and reseed committed Claude fixture into %s: %v", reseed.Destination, err)
	}
	return reseed.Destination
}

func rewriteFirstTranscriptCWD(transcript []byte, workingDirectory string) ([]byte, error) {
	if strings.TrimSpace(workingDirectory) == "" {
		return nil, fmt.Errorf("recorded working directory is empty; ingestion would derive the project identity from the fixture path instead of the disposable repository")
	}
	newline := strings.IndexByte(string(transcript), '\n')
	if newline < 0 {
		return nil, fmt.Errorf("transcript has no complete first JSONL line")
	}
	var firstLine map[string]any
	if err := json.Unmarshal(transcript[:newline], &firstLine); err != nil {
		return nil, fmt.Errorf("decode first JSONL line: %w", err)
	}
	if _, ok := firstLine["cwd"]; !ok {
		return nil, fmt.Errorf("first JSONL line has no cwd field")
	}
	firstLine["cwd"] = workingDirectory
	rewritten, err := json.Marshal(firstLine)
	if err != nil {
		return nil, fmt.Errorf("encode rewritten first JSONL line: %w", err)
	}
	return append(append(rewritten, '\n'), transcript[newline+1:]...), nil
}

func writeClaudeOnlyConfig(t *testing.T, sandbox disposableSandbox, fixtureRoot string) {
	t.Helper()
	writeDisposableSandboxConfig(t, sandbox, fmt.Sprintf(`version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - %q
  opencode:
    enabled: false
  codex:
    enabled: false
  cursor:
    enabled: false
output:
  basePath: %q
`, fixtureRoot, sandbox.transcriptOutputPath()))
}

// transcriptOutputPath is where ingested transcripts land inside the sandbox.
func (sandbox disposableSandbox) transcriptOutputPath() string {
	return filepath.Join(sandbox.dataHome, string(defaults.AppName), "peasant-sync")
}

func writeDisposableSandboxConfig(t *testing.T, sandbox disposableSandbox, contents string) {
	t.Helper()
	directory := filepath.Join(sandbox.configHome, string(defaults.AppName))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create disposable Peasant config directory %s: %v", directory, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write disposable Peasant config in %s: %v", directory, err)
	}
}

func resolveDisposableRepositoryScope(t *testing.T, db *store.Store, repositoryRoot string) *push.RepositoryScope {
	t.Helper()
	ctx := context.Background()
	resolver := &ingest.ExecGitResolver{}
	canonicalRoot, err := resolver.ResolveRepositoryRoot(ctx, repositoryRoot)
	if err != nil {
		t.Fatalf("resolve disposable Git repository root: %v", err)
	}
	remote, _, err := resolver.WalkUpRemoteURL(ctx, canonicalRoot)
	if err != nil {
		t.Fatalf("resolve disposable repository origin: %v", err)
	}
	if remote != "" {
		t.Fatalf("disposable repository resolved unexpected origin remote %q", remote)
	}
	identity, _, err := ingest.DeriveProjectIdentifiers(db.InstallationSalt(), remote, canonicalRoot)
	if err != nil {
		t.Fatalf("derive disposable repository identity: %v", err)
	}
	return push.NewRepositoryScope(canonicalRoot, identity, push.IdentityFromPath, nil, nil)
}

// runPeasantInSandbox runs the real peasant binary with the sandbox's complete
// environment and returns its combined output.
func runPeasantInSandbox(t *testing.T, binary string, sandbox disposableSandbox, arguments ...string) string {
	t.Helper()
	command := exec.Command(binary, arguments...)
	command.Env = sandbox.environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run peasant %s in the disposable sandbox: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

// gitCommand runs one isolated git invocation and returns its combined output
// with the error, for the cases whose whole point is what git did on failure.
func gitCommand(environment []string, repositoryRoot string, arguments ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", repositoryRoot}, arguments...)...)
	command.Env = environment
	output, err := command.CombinedOutput()
	return string(output), err
}

// runGit runs one isolated git invocation that must succeed.
func runGit(t *testing.T, environment []string, repositoryRoot string, arguments ...string) string {
	t.Helper()
	output, err := gitCommand(environment, repositoryRoot, arguments...)
	if err != nil {
		t.Fatalf("run isolated git %s in %s: %v\n%s", strings.Join(arguments, " "), repositoryRoot, err, output)
	}
	return output
}
