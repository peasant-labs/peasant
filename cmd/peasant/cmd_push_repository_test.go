package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/push_repository_scope.yaml
var pushRepositoryScopeFixtureData []byte

const pushRepositoryScopeFixturePath = "cmd/peasant/testdata/push_repository_scope.yaml"

// scopeSelectionMode is the configured selection the case runs under. The set is
// closed so a case cannot ask for a selection the seeding code does not build.
type scopeSelectionMode string

const (
	// scopeModeAll leaves selection off: only --repository narrows.
	scopeModeAll scopeSelectionMode = "all"
	// scopeModeSelected selects THIS repository's project, by branch.
	scopeModeSelected scopeSelectionMode = "selected"
	// scopeModeSelectedOtherProject selects a project the scope excludes, so the
	// two narrowings intersect to nothing.
	scopeModeSelectedOtherProject scopeSelectionMode = "selected-other-project"
)

var allScopeSelectionModes = [...]scopeSelectionMode{scopeModeAll, scopeModeSelected, scopeModeSelectedOtherProject}

// scopeSession names one seeded session by the place it was recorded, so a case
// reads as a statement about repositories rather than about UUIDs.
type scopeSession string

const (
	// scopeSessionRoot was recorded at the repository root, on main.
	scopeSessionRoot scopeSession = "root"
	// scopeSessionSubdir was recorded in a plain subdirectory, on main.
	scopeSessionSubdir scopeSession = "subdir"
	// scopeSessionNested was recorded in a directory that is its own repository.
	scopeSessionNested scopeSession = "nested"
	// scopeSessionOther was recorded in a different repository entirely.
	scopeSessionOther scopeSession = "other"
	// scopeSessionBranch was recorded at the repository root, on feature.
	scopeSessionBranch scopeSession = "branch"
)

var allScopeSessions = [...]scopeSession{
	scopeSessionRoot, scopeSessionSubdir, scopeSessionNested, scopeSessionOther, scopeSessionBranch,
}

// scopeSessionIDs pins one UUID per seeded session. They live here once so a
// case names behaviour and an assertion failure names a place.
var scopeSessionIDs = map[scopeSession]string{
	scopeSessionRoot:   "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	scopeSessionSubdir: "dddddddd-dddd-dddd-dddd-dddddddddddd",
	scopeSessionNested: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
	scopeSessionOther:  "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
	scopeSessionBranch: "cccccccc-cccc-cccc-cccc-cccccccccccc",
}

type pushRepositoryScopeDocument struct {
	ExpectedCaseCount int                       `yaml:"expectedCaseCount"`
	Cases             []pushRepositoryScopeCase `yaml:"cases"`
}

type pushRepositoryScopeCase struct {
	Name                      string             `yaml:"name"`
	Mode                      scopeSelectionMode `yaml:"mode"`
	SelectedBranches          []string           `yaml:"selectedBranches"`
	ExpectedSessions          []scopeSession     `yaml:"expectedSessions"`
	ExpectNarrowedEmptyReason bool               `yaml:"expectNarrowedEmptyReason"`
}

func loadPushRepositoryScopeFixtures(data []byte) (pushRepositoryScopeDocument, error) {
	var document pushRepositoryScopeDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf(
			"push repository scope fixture rule failed: typed YAML fields must match the document schema; unknown or "+
				"malformed data invalidates the scope evidence; where=%s loader=first-document decode; when=test fixture loading; "+
				"impact=what --repository admits cannot be trusted; fix=match the typed schema: %w",
			pushRepositoryScopeFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return document, fmt.Errorf(
			"push repository scope fixture rule failed: exactly one YAML document is allowed; trailing data is silently "+
				"ignored; where=%s loader=end-of-document check; when=test fixture loading; "+
				"impact=what --repository admits cannot be trusted; fix=remove the second document",
			pushRepositoryScopeFixturePath)
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, fmt.Errorf(
			"push repository scope fixture rule failed: declared and actual case counts must match and be non-zero, got "+
				"expectedCaseCount=%d cases=%d; where=%s loader=case-count validation; when=test fixture loading; "+
				"impact=what --repository admits cannot be trusted; fix=set expectedCaseCount to the number of cases present",
			document.ExpectedCaseCount, len(document.Cases), pushRepositoryScopeFixturePath)
	}
	seen := make(map[string]bool, len(document.Cases))
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, pushRepositoryScopeRuleError(index,
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if !scopeFixtureContains(allScopeSelectionModes[:], testCase.Mode) {
			return document, pushRepositoryScopeRuleError(index,
				fmt.Sprintf("unsupported mode %q", testCase.Mode),
				"fix=use all, selected, or selected-other-project")
		}
		if (testCase.Mode == scopeModeSelected) != (len(testCase.SelectedBranches) > 0) {
			return document, pushRepositoryScopeRuleError(index,
				"selectedBranches is set for exactly the selected mode",
				"fix=name at least one branch in selected mode, and none in any other")
		}
		expected := make(map[scopeSession]bool, len(testCase.ExpectedSessions))
		for _, session := range testCase.ExpectedSessions {
			if !scopeFixtureContains(allScopeSessions[:], session) {
				return document, pushRepositoryScopeRuleError(index,
					fmt.Sprintf("unknown session %q", session),
					"fix=name one of the seeded sessions: root, subdir, nested, other, branch")
			}
			if expected[session] {
				return document, pushRepositoryScopeRuleError(index,
					fmt.Sprintf("session %q is expected twice", session), "fix=name each session once")
			}
			expected[session] = true
		}
		for _, forbidden := range []scopeSession{scopeSessionNested, scopeSessionOther} {
			if expected[forbidden] {
				return document, pushRepositoryScopeRuleError(index,
					fmt.Sprintf("session %q can never be in scope", forbidden),
					"fix=a separate repository's sessions are out of scope by construction; expecting one would encode a consent leak as correct")
			}
		}
		if testCase.ExpectNarrowedEmptyReason != (len(testCase.ExpectedSessions) == 0) {
			return document, pushRepositoryScopeRuleError(index,
				fmt.Sprintf("expectNarrowedEmptyReason=%v contradicts %d expected session(s)",
					testCase.ExpectNarrowedEmptyReason, len(testCase.ExpectedSessions)),
				"fix=expect the narrowed empty reason exactly when nothing is left to push")
		}
	}
	return document, nil
}

func pushRepositoryScopeRuleError(index int, what, fix string) error {
	return fmt.Errorf(
		"push repository scope fixture rule failed: %s; a malformed case invalidates the scope evidence; "+
			"where=%s case index %d; when=test fixture loading; impact=what --repository admits cannot be trusted; %s",
		what, pushRepositoryScopeFixturePath, index, fix)
}

func scopeFixtureContains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// --- loader guards ----------------------------------------------------------

func TestLoadPushRepositoryScopeFixtures_RejectsAnOutOfScopeExpectation(t *testing.T) {
	t.Parallel()
	_, err := loadPushRepositoryScopeFixtures([]byte(`expectedCaseCount: 1
cases:
  - name: nested repository is in scope
    mode: all
    expectedSessions: [root, nested]
`))
	if err == nil || !strings.Contains(err.Error(), "can never be in scope") {
		t.Fatalf("error = %v, want rejection of a case that would encode a consent leak as correct", err)
	}
}

func TestLoadPushRepositoryScopeFixtures_RejectsContradictoryEmptyExpectation(t *testing.T) {
	t.Parallel()
	_, err := loadPushRepositoryScopeFixtures([]byte(`expectedCaseCount: 1
cases:
  - name: empty without saying so
    mode: all
    expectedSessions: []
    expectNarrowedEmptyReason: false
`))
	if err == nil || !strings.Contains(err.Error(), "contradicts") {
		t.Fatalf("error = %v, want rejection of an empty case that expects no explanation", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestPushCmd_RepositoryScopeUsesCanonicalProjectHash drives the mounted push
// command against a seeded world of remote-less repositories.
//
// Remote-less is the interesting case: identity is the directory each session was
// recorded in, so a scope built only from the worktree root would silently admit
// nothing for anyone who works in a subdirectory — a hook that uploads nothing,
// forever, with no error. Admitting those subdirectories must not admit a nested
// repository, which is a separate repository that happens to live inside this one.
func TestPushCmd_RepositoryScopeUsesCanonicalProjectHash(t *testing.T) {
	document, err := loadPushRepositoryScopeFixtures(pushRepositoryScopeFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			world := seedRepositoryScopeWorld(t)
			cfgPath := writeCfg(t, world.dir, "repository-scope.yaml", scopeConfigBody(testCase, world))

			stdout, stderr, runErr := executePushCmdSeparate(t, world.dir,
				[]string{"--dry-run", "--json", "--config=" + cfgPath, "--repository", world.repo})
			if runErr != nil {
				t.Fatalf("push --dry-run: %v\nstdout: %s\nstderr: %s", runErr, stdout, stderr)
			}

			got := scopeSessionsIn(t, stdout)
			want := make(map[string]bool, len(testCase.ExpectedSessions))
			for _, session := range testCase.ExpectedSessions {
				want[scopeSessionIDs[session]] = true
			}
			if !sameIDSet(got, want) {
				t.Errorf("--repository kept %v, want %v", sortedKeys(got), sortedKeys(want))
			}

			// The resolved scope is always reported, so an empty push explains
			// itself instead of leaving the user to guess.
			if !strings.Contains(stderr, "scope: only sessions recorded for "+world.repo) {
				t.Errorf("push must report the resolved scope on stderr; got:\n%s", stderr)
			}
			if !strings.Contains(stderr, "no origin remote") {
				t.Errorf("push must report how the identity was derived; got:\n%s", stderr)
			}

			emptyReason := scopeEmptyReason(t, stdout)
			if testCase.ExpectNarrowedEmptyReason {
				for _, want := range []string{"No sessions match", "cannot widen"} {
					if !strings.Contains(emptyReason, want) {
						t.Errorf("an empty narrowed push must explain itself with %q; got: %q", want, emptyReason)
					}
				}
				if strings.Contains(emptyReason, "already pushed") {
					t.Errorf("a narrowing that removed everything must not be reported as already pushed; got: %q", emptyReason)
				}
			} else if emptyReason != "" {
				t.Errorf("a push with sessions in scope must not report an empty reason; got: %q", emptyReason)
			}
		})
	}
}

// --- world ------------------------------------------------------------------

// repositoryScopeWorld is the seeded set of repositories and sessions every case
// runs against.
type repositoryScopeWorld struct {
	dir  string
	repo string
	// otherProjectName is the display name of a project the scope excludes, used
	// to build a selection that cannot intersect it.
	otherProjectName string
}

func seedRepositoryScopeWorld(t *testing.T) repositoryScopeWorld {
	t.Helper()
	dir := t.TempDir()
	writeTestCredentials(t, dir)

	// Two repositories sharing a basename, so nothing can pass by name alone.
	repo := filepath.Join(dir, "repositories", "one", "shared-name")
	otherRepo := filepath.Join(dir, "repositories", "two", "shared-name")
	subdir := filepath.Join(repo, "services", "api")
	nested := filepath.Join(repo, "vendor", "lib")
	for _, path := range []string{repo, otherRepo, nested} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		hooksGit(t, path, "", "init", "--quiet", "--initial-branch=main")
	}
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}

	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &ingest.ExecGitResolver{}
	hashFor := func(path string) ingest.ProjectHash {
		t.Helper()
		hash, _, hashErr := ingest.DeriveProjectIdentifiersWithGit(t.Context(), db.InstallationSalt(), resolver, "", path)
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		return hash
	}
	rootHash := hashFor(repo)
	subdirHash := hashFor(subdir)
	nestedHash := hashFor(nested)
	otherHash := hashFor(otherRepo)
	for name, hash := range map[string]ingest.ProjectHash{"subdir": subdirHash, "nested": nestedHash, "other": otherHash} {
		if hash == rootHash {
			t.Fatalf("%s must derive its own identity, not the repository root's", name)
		}
	}

	entries := []ingest.StoreEntry{
		scopeEntry(t, scopeSessionRoot, "local-one-shared-name", "main", 1700000000000, rootHash, repo),
		scopeEntry(t, scopeSessionSubdir, "local-one-shared-name-api", "main", 1700000030000, subdirHash, subdir),
		scopeEntry(t, scopeSessionNested, "local-one-shared-name-vendor", "main", 1700000060000, nestedHash, nested),
		scopeEntry(t, scopeSessionOther, "local-two-shared-name", "main", 1700000090000, otherHash, otherRepo),
		scopeEntry(t, scopeSessionBranch, "local-one-shared-name", "feature", 1700000120000, rootHash, repo),
	}
	if err := db.InsertSessions(t.Context(), entries); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return repositoryScopeWorld{dir: dir, repo: repo, otherProjectName: filepath.Base(otherRepo) + "-elsewhere"}
}

func scopeEntry(t *testing.T, session scopeSession, hostSlug, branch string, startMs int64, hash ingest.ProjectHash, path string) ingest.StoreEntry {
	t.Helper()
	entry := makeCmdStoreEntry(t, scopeSessionIDs[session], hostSlug, "", branch, startMs)
	entry.Metadata.Project = ingest.ProjectInfo{Hash: hash, Name: filepath.Base(path), FilePath: path}
	return entry
}

// scopeConfigBody renders the config for a case. The selected modes use the
// branch-aware project selection the push path already honours.
func scopeConfigBody(testCase pushRepositoryScopeCase, world repositoryScopeWorld) string {
	body := "version: 1\npush:\n  method: all\n  visibility: private\n"
	switch testCase.Mode {
	case scopeModeSelected:
		body += "selection:\n  mode: selected\n  harnesses:\n    claude-code:\n      projects:\n        - name: shared-name\n          branches:\n"
		for _, branch := range testCase.SelectedBranches {
			body += "            - " + branch + "\n"
		}
	case scopeModeSelectedOtherProject:
		body += "selection:\n  mode: selected\n  harnesses:\n    claude-code:\n      projects:\n        - name: " +
			world.otherProjectName + "\n"
	}
	return body
}

// --- output helpers ---------------------------------------------------------

func scopePushJSON(t *testing.T, stdout string) struct {
	EmptyReason string `json:"empty_reason"`
	Sessions    []struct {
		SessionID string `json:"session_id"`
	} `json:"sessions"`
} {
	t.Helper()
	var result struct {
		EmptyReason string `json:"empty_reason"`
		Sessions    []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	start := strings.Index(stdout, "{")
	if start < 0 {
		t.Fatalf("no JSON object in push output: %q", stdout)
	}
	if err := json.Unmarshal([]byte(stdout[start:]), &result); err != nil {
		t.Fatalf("unmarshal push JSON: %v\njson: %s", err, stdout[start:])
	}
	return result
}

func scopeSessionsIn(t *testing.T, stdout string) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, session := range scopePushJSON(t, stdout).Sessions {
		ids[session.SessionID] = true
	}
	return ids
}

func scopeEmptyReason(t *testing.T, stdout string) string {
	t.Helper()
	return scopePushJSON(t, stdout).EmptyReason
}

func sameIDSet(got, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for id := range want {
		if !got[id] {
			return false
		}
	}
	return true
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
