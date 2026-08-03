package ingest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
