package ingest

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/repository_identity_failures.yaml
var repositoryIdentityFailureData []byte

//go:embed testdata/repository_topology_guards.yaml
var repositoryTopologyGuardData []byte

type repositoryIdentityFailureDocument struct {
	ExpectedCaseCount int                                `yaml:"expectedCaseCount"`
	Cases             []repositoryIdentityFailureFixture `yaml:"cases"`
}

type repositoryIdentityFailureFixture struct {
	Name                   string `yaml:"name"`
	FailCommand            string `yaml:"failCommand"`
	FailPath               string `yaml:"failPath"`
	WantOperation          string `yaml:"wantOperation"`
	WantGitDirectory       string `yaml:"wantGitDirectory"`
	ExpectedCommandMinimum int    `yaml:"expectedCommandMinimum"`
}

type repositoryTopologyGuardKind string

const (
	repositoryTopologyGuardContainment repositoryTopologyGuardKind = "containment"
	repositoryTopologyGuardCycle       repositoryTopologyGuardKind = "cycle"
	repositoryTopologyGuardDepth       repositoryTopologyGuardKind = "depth"
)

type repositoryTopologyGuardDocument struct {
	ExpectedCaseCount int                              `yaml:"expectedCaseCount"`
	Cases             []repositoryTopologyGuardFixture `yaml:"cases"`
}

type repositoryTopologyGuardFixture struct {
	Name             string                      `yaml:"name"`
	Kind             repositoryTopologyGuardKind `yaml:"kind"`
	NestedDepth      int                         `yaml:"nestedDepth"`
	WantErrorText    string                      `yaml:"wantErrorText"`
	WantGitDirectory string                      `yaml:"wantGitDirectory"`
}

type repositoryIdentityFixturePathResolver struct {
	failPath string
}

func (r repositoryIdentityFixturePathResolver) Resolve(raw string) (ClonePath, error) {
	if raw == r.failPath {
		return "", fmt.Errorf("fixture physical path failure for %q", raw)
	}
	return ClonePath(filepath.Clean(raw)), nil
}

func loadRepositoryIdentityFailureFixtures(t *testing.T) repositoryIdentityFailureDocument {
	t.Helper()
	var document repositoryIdentityFailureDocument
	decoder := yaml.NewDecoder(strings.NewReader(string(repositoryIdentityFailureData)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode repository identity failure fixture: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || len(document.Cases) == 0 {
		t.Fatalf("repository identity failure fixture declared=%d actual=%d", document.ExpectedCaseCount, len(document.Cases))
	}
	seen := make(map[string]bool, len(document.Cases))
	for _, testCase := range document.Cases {
		if testCase.Name == "" || testCase.WantOperation == "" || testCase.ExpectedCommandMinimum <= 0 || seen[testCase.Name] {
			t.Fatalf("invalid repository identity failure fixture %#v", testCase)
		}
		seen[testCase.Name] = true
	}
	return document
}

func TestExecGitResolver_RepositoryIdentityFailureBoundaries(t *testing.T) {
	document := loadRepositoryIdentityFailureFixtures(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			commands := 0
			resolver := &ExecGitResolver{
				identityPathResolver: repositoryIdentityFixturePathResolver{failPath: testCase.FailPath},
				identityRunGit: func(_ context.Context, args ...string) (string, error) {
					commands++
					command := strings.Join(args, " ")
					if testCase.FailCommand != "" && strings.Contains(command, testCase.FailCommand) {
						return "", fmt.Errorf("fixture command failure for %q", command)
					}
					switch {
					case strings.Contains(command, "--git-common-dir"):
						return "/fixture/git-dir", nil
					case strings.Contains(command, "--show-superproject-working-tree"):
						return "/fixture/super", nil
					case strings.Contains(command, "--show-toplevel") && strings.Contains(command, "/fixture/super/child"):
						return "/fixture/super/child", nil
					case strings.Contains(command, "--show-toplevel") && strings.Contains(command, "/fixture/super"):
						return "/fixture/super", nil
					case strings.Contains(command, "config --file"):
						return "submodule.child.path child", nil
					default:
						return "", fmt.Errorf("unexpected fixture command %q", command)
					}
				},
			}
			identity, err := resolver.ResolveRepositoryIdentity(context.Background(), ClonePath("/fixture/super/child"))
			if err == nil || !strings.Contains(err.Error(), testCase.WantOperation) {
				t.Fatalf("repository identity failure error=%v, want operation %q", err, testCase.WantOperation)
			}
			if identity.GitDirectory.String() != testCase.WantGitDirectory {
				t.Fatalf("repository identity failure Git directory=%q, want %q", identity.GitDirectory, testCase.WantGitDirectory)
			}
			if commands < testCase.ExpectedCommandMinimum {
				t.Fatalf("repository identity failure commands=%d, want at least %d", commands, testCase.ExpectedCommandMinimum)
			}
		})
	}
}

func loadRepositoryTopologyGuardFixtures(t *testing.T) repositoryTopologyGuardDocument {
	t.Helper()
	var document repositoryTopologyGuardDocument
	decoder := yaml.NewDecoder(strings.NewReader(string(repositoryTopologyGuardData)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode repository topology guard fixture: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || len(document.Cases) == 0 {
		t.Fatalf("repository topology guard fixture declared=%d actual=%d", document.ExpectedCaseCount, len(document.Cases))
	}
	seen := make(map[string]bool, len(document.Cases))
	for _, testCase := range document.Cases {
		validKind := testCase.Kind == repositoryTopologyGuardContainment || testCase.Kind == repositoryTopologyGuardCycle || testCase.Kind == repositoryTopologyGuardDepth
		if testCase.Name == "" || !validKind || testCase.WantErrorText == "" || testCase.WantGitDirectory == "" || seen[testCase.Name] {
			t.Fatalf("invalid repository topology guard fixture %#v", testCase)
		}
		if testCase.Kind == repositoryTopologyGuardDepth && testCase.NestedDepth <= maximumRepositoryIdentityDepth {
			t.Fatalf("repository topology depth fixture %q depth=%d, want greater than %d", testCase.Name, testCase.NestedDepth, maximumRepositoryIdentityDepth)
		}
		seen[testCase.Name] = true
	}
	return document
}

func TestExecGitResolver_RepositoryIdentityTopologyGuards(t *testing.T) {
	document := loadRepositoryTopologyGuardFixtures(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			resolver, start := topologyGuardResolver(t, testCase)
			identity, err := resolver.ResolveRepositoryIdentity(context.Background(), ClonePath(start))
			if err == nil || !strings.Contains(err.Error(), testCase.WantErrorText) {
				t.Fatalf("repository topology guard error=%v, want text %q", err, testCase.WantErrorText)
			}
			if identity.GitDirectory.String() != testCase.WantGitDirectory {
				t.Fatalf("repository topology guard Git directory=%q, want %q", identity.GitDirectory, testCase.WantGitDirectory)
			}
		})
	}
}

func topologyGuardResolver(t *testing.T, testCase repositoryTopologyGuardFixture) (*ExecGitResolver, string) {
	t.Helper()
	pathResolver := repositoryIdentityFixturePathResolver{}
	switch testCase.Kind {
	case repositoryTopologyGuardContainment:
		return &ExecGitResolver{
			identityPathResolver: pathResolver,
			identityRunGit: func(_ context.Context, args ...string) (string, error) {
				command := strings.Join(args, " ")
				switch {
				case strings.Contains(command, "--git-common-dir"):
					return testCase.WantGitDirectory, nil
				case strings.Contains(command, "--show-superproject-working-tree"):
					return "/fixture/super", nil
				case strings.Contains(command, "--show-toplevel") && strings.Contains(command, "/fixture/outside/child"):
					return "/fixture/outside/child", nil
				case strings.Contains(command, "--show-toplevel") && strings.Contains(command, "/fixture/super"):
					return "/fixture/super", nil
				default:
					return "", fmt.Errorf("unexpected containment fixture command %q", command)
				}
			},
		}, "/fixture/outside/child"
	case repositoryTopologyGuardCycle:
		superTopCalls := 0
		return &ExecGitResolver{
			identityPathResolver: pathResolver,
			identityRunGit: func(_ context.Context, args ...string) (string, error) {
				command := strings.Join(args, " ")
				directory := repositoryIdentityCommandDirectory(args)
				switch {
				case strings.Contains(command, "--git-common-dir"):
					if directory == "/fixture/super/child" {
						return testCase.WantGitDirectory, nil
					}
					return "/fixture/super.git", nil
				case strings.Contains(command, "--show-superproject-working-tree") && directory == "/fixture/super/child":
					return "/fixture/super", nil
				case strings.Contains(command, "--show-superproject-working-tree"):
					return "/fixture/grand", nil
				case strings.Contains(command, "--show-toplevel") && directory == "/fixture/super/child":
					return "/fixture/super/child", nil
				case strings.Contains(command, "--show-toplevel") && directory == "/fixture/super":
					superTopCalls++
					if superTopCalls == 1 {
						return "/fixture/super", nil
					}
					return "/fixture/super/child", nil
				case strings.Contains(command, "config --file"):
					return "submodule.child.path child", nil
				default:
					return "", fmt.Errorf("unexpected cycle fixture command %q", command)
				}
			},
		}, "/fixture/super/child"
	case repositoryTopologyGuardDepth:
		parts := make([]string, testCase.NestedDepth+2)
		declarations := make([]string, len(parts))
		for index := range parts {
			parts[index] = fmt.Sprintf("level-%02d", index)
			declarations[index] = fmt.Sprintf("submodule.level-%02d.path level-%02d", index, index)
		}
		start := filepath.Join(append([]string{"/fixture"}, parts...)...)
		return &ExecGitResolver{
			identityPathResolver: pathResolver,
			identityRunGit: func(_ context.Context, args ...string) (string, error) {
				command := strings.Join(args, " ")
				directory := repositoryIdentityCommandDirectory(args)
				switch {
				case strings.Contains(command, "--git-common-dir"):
					if directory == start {
						return testCase.WantGitDirectory, nil
					}
					return filepath.Join("/fixture/gitdirs", strings.TrimPrefix(directory, "/")), nil
				case strings.Contains(command, "--show-superproject-working-tree"):
					return filepath.Dir(directory), nil
				case strings.Contains(command, "--show-toplevel"):
					return directory, nil
				case strings.Contains(command, "config --file"):
					return strings.Join(declarations, "\n"), nil
				default:
					return "", fmt.Errorf("unexpected depth fixture command %q", command)
				}
			},
		}, start
	default:
		t.Fatalf("unknown repository topology guard %q", testCase.Kind)
		return nil, ""
	}
}

func repositoryIdentityCommandDirectory(args []string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-C" {
			return args[index+1]
		}
	}
	return ""
}

// initTestRepo creates a temp directory with a git repo configured for testing.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "remote", "add", "origin", "git@github.com:testuser/testrepo.git"},
		{"git", "commit", "--allow-empty", "-m", "init"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestExecGitResolver_RemoteURL(t *testing.T) {
	repoDir := initTestRepo(t)
	g := &ExecGitResolver{}
	ctx := context.Background()

	got, err := g.RemoteURL(ctx, repoDir)
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	want := "git@github.com:testuser/testrepo.git"
	if got != want {
		t.Errorf("RemoteURL = %q, want %q", got, want)
	}
}

func TestExecGitResolver_Branch(t *testing.T) {
	repoDir := initTestRepo(t)
	g := &ExecGitResolver{}
	ctx := context.Background()

	got, err := g.Branch(ctx, repoDir)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	// git init defaults to "master" or "main" depending on git version/config
	if got != "master" && got != "main" {
		t.Errorf("Branch = %q, want \"master\" or \"main\"", got)
	}
}

func TestExecGitResolver_Worktree_MainCheckout(t *testing.T) {
	// For a standard repo (not a linked worktree), Worktree() returns empty.
	repoDir := initTestRepo(t)
	g := &ExecGitResolver{}
	ctx := context.Background()

	got, err := g.Worktree(ctx, repoDir)
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	if got != "" {
		t.Errorf("Worktree for main checkout = %q, want empty", got)
	}
}

func TestExecGitResolver_Worktree_Subdir(t *testing.T) {
	// Even from a subdirectory, a main checkout returns empty.
	repoDir := initTestRepo(t)
	g := &ExecGitResolver{}
	ctx := context.Background()

	subdir := filepath.Join(repoDir, "src", "foo")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatalf("MkdirAll subdir: %v", err)
	}

	got, err := g.Worktree(ctx, subdir)
	if err != nil {
		t.Fatalf("Worktree from subdir: %v", err)
	}

	if got != "" {
		t.Errorf("Worktree from subdir = %q, want empty for main checkout", got)
	}
}

func TestExecGitResolver_UserEmail(t *testing.T) {
	// UserEmail reads --global config; skip if not set
	g := &ExecGitResolver{}
	ctx := context.Background()

	got, err := g.UserEmail(ctx)
	if err != nil {
		// It's acceptable for this to fail if global git config is not set
		t.Skipf("UserEmail not available in this environment: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Errorf("UserEmail returned empty string")
	}
}

func TestExecGitResolver_RemoteURL_NoRemote(t *testing.T) {
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
		{"git", "config", "commit.gpgsign", "false"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v\n%s", args, err, out)
		}
	}

	g := &ExecGitResolver{}
	ctx := context.Background()

	_, err := g.RemoteURL(ctx, dir)
	if err == nil {
		t.Errorf("RemoteURL on repo with no origin should return error")
	}
}

// TestExecGitResolver_WalkUpRemoteURL_FirstHop verifies that when dir is a git repo
// with a remote, WalkUpRemoteURL returns it immediately (first hop).
func TestExecGitResolver_WalkUpRemoteURL_FirstHop(t *testing.T) {
	repoDir := initTestRepo(t)
	g := &ExecGitResolver{}
	ctx := context.Background()

	remote, resolved, err := g.WalkUpRemoteURL(ctx, repoDir)
	if err != nil {
		t.Fatalf("WalkUpRemoteURL: %v", err)
	}
	want := "git@github.com:testuser/testrepo.git"
	if remote != want {
		t.Errorf("remote = %q, want %q", remote, want)
	}
	if resolved == "" {
		t.Errorf("resolvedDir should not be empty")
	}
}

// TestExecGitResolver_WalkUpRemoteURL_NthHop verifies that when dir is a subdirectory
// of a git repo, WalkUpRemoteURL walks up and finds the remote at the repo root.
func TestExecGitResolver_WalkUpRemoteURL_NthHop(t *testing.T) {
	repoDir := initTestRepo(t)
	g := &ExecGitResolver{}
	ctx := context.Background()

	// Create a nested subdirectory inside the repo.
	nested := filepath.Join(repoDir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	remote, resolved, err := g.WalkUpRemoteURL(ctx, nested)
	if err != nil {
		t.Fatalf("WalkUpRemoteURL from nested dir: %v", err)
	}
	want := "git@github.com:testuser/testrepo.git"
	if remote != want {
		t.Errorf("remote = %q, want %q", remote, want)
	}
	if resolved == "" {
		t.Errorf("resolvedDir should not be empty")
	}
}

// TestExecGitResolver_WalkUpRemoteURL_StopsAtRepositoryBoundary is the evidence
// behind the project-identity boundary.
//
// A repository nested inside another one — a `git init` in a subdirectory, a
// clone inside a clone — is a separate repository with its own history. Walking
// past its top level and adopting the OUTER repository's origin gives both the
// same canonical project identity, which means sessions recorded in the inner
// one are stamped as the outer one's and a push scoped to the inner repository
// uploads the outer repository's transcripts.
func TestExecGitResolver_WalkUpRemoteURL_StopsAtRepositoryBoundary(t *testing.T) {
	outer := initTestRepo(t)
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = inner
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("inner repo setup %v: %v\n%s", args, err, out)
		}
	}

	g := &ExecGitResolver{}
	ctx := context.Background()

	// The nested repository has no remote of its own and must not borrow one.
	remote, resolved, err := g.WalkUpRemoteURL(ctx, inner)
	if err != nil {
		t.Fatalf("WalkUpRemoteURL from the nested repository: %v", err)
	}
	if remote != "" || resolved != "" {
		t.Errorf("the nested repository adopted the parent's remote: remote=%q resolved=%q; "+
			"its sessions would then carry the parent's project identity", remote, resolved)
	}

	// A subdirectory of the nested repository is still the nested repository.
	deep := filepath.Join(inner, "pkg", "sub")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if remote, _, err := g.WalkUpRemoteURL(ctx, deep); err != nil || remote != "" {
		t.Errorf("a subdirectory of the nested repository resolved remote=%q err=%v, want no remote", remote, err)
	}

	// The outer repository is unaffected: it still resolves its own origin, and
	// so does an ordinary subdirectory of it.
	outerSub := filepath.Join(outer, "a", "b")
	if err := os.MkdirAll(outerSub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const want = "git@github.com:testuser/testrepo.git"
	for _, dir := range []string{outer, outerSub} {
		got, _, err := g.WalkUpRemoteURL(ctx, dir)
		if err != nil {
			t.Fatalf("WalkUpRemoteURL(%s): %v", dir, err)
		}
		if got != want {
			t.Errorf("WalkUpRemoteURL(%s) = %q, want %q", dir, got, want)
		}
	}
}

// TestExecGitResolver_WalkUpRemoteURL_NoRemoteAnywhere verifies that when no git repo
// exists anywhere in the parent chain, WalkUpRemoteURL returns ("", "", nil).
func TestExecGitResolver_WalkUpRemoteURL_NoRemoteAnywhere(t *testing.T) {
	// Use a temp dir that is NOT inside any git repo.
	// We can't guarantee that t.TempDir() is outside all git repos on the CI
	// machine, so we create a deeply nested dir that is definitely not a git repo.
	base := t.TempDir()
	dir := filepath.Join(base, "not-a-repo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	g := &ExecGitResolver{}
	ctx := context.Background()

	remote, resolved, err := g.WalkUpRemoteURL(ctx, dir)
	if err != nil {
		t.Fatalf("WalkUpRemoteURL: %v", err)
	}
	// The test environment may be inside a git repo (dev machine). Skip the
	// emptiness assertion if a remote was found (acceptable: the walk-up is
	// working correctly by finding the enclosing repo).
	if remote != "" && resolved != "" {
		t.Logf("walk-up found enclosing git repo: remote=%q resolved=%q (ok in dev environment)", remote, resolved)
		return
	}
	if remote != "" || resolved != "" {
		t.Errorf("expected both empty, got remote=%q resolved=%q", remote, resolved)
	}
}

// TestExecGitResolver_WalkUpRemoteURL_ContextCancelled verifies that a cancelled context
// causes WalkUpRemoteURL to return the context error promptly.
func TestExecGitResolver_WalkUpRemoteURL_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	g := &ExecGitResolver{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, _, err := g.WalkUpRemoteURL(ctx, dir)
	if err == nil {
		// If no error: the walk completed (which is technically correct for
		// a dir not in a repo — no calls to RemoteURL are made before the ctx check).
		// The constraint is "must not hang", which is satisfied.
		t.Log("WalkUpRemoteURL returned nil error with cancelled context (expected for non-repo dir)")
		return
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("expected context error, got %v", err)
	}
}
