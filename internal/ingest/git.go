package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitResolver resolves git metadata from a directory.
type GitResolver interface {
	RemoteURL(ctx context.Context, dir string) (string, error)
	Branch(ctx context.Context, dir string) (string, error)
	Worktree(ctx context.Context, dir string) (string, error)
	TrackingBranch(ctx context.Context, dir string) (string, error)
	UserEmail(ctx context.Context) (string, error)
	// WalkUpRemoteURL walks parent directories from dir until it finds one
	// with a git remote, without ever leaving the repository dir belongs to.
	// Returns (remoteURL, resolvedDir, nil) on success.
	// Returns ("", "", nil) if no remote is found before the walk reaches that
	// repository's top level, or the filesystem root when dir is in no
	// repository at all.
	WalkUpRemoteURL(ctx context.Context, dir string) (remoteURL string, resolvedDir string, err error)
}

// ExecGitResolver shells out to git using exec.Command (argument-list form, NEVER sh -c).
type ExecGitResolver struct{}

var _ GitResolver = (*ExecGitResolver)(nil)

// ResolveRepositoryRoot returns Git's canonical worktree root for dir.
func (g *ExecGitResolver) ResolveRepositoryRoot(ctx context.Context, dir string) (string, error) {
	root, err := runGit(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.Clean(root), nil
}

func (g *ExecGitResolver) RemoteURL(ctx context.Context, dir string) (string, error) {
	return runGit(ctx, "git", "-C", dir, "remote", "get-url", "origin")
}

func (g *ExecGitResolver) Branch(ctx context.Context, dir string) (string, error) {
	return runGit(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func (g *ExecGitResolver) Worktree(ctx context.Context, dir string) (string, error) {
	out, err := runGit(ctx, "git", "-C", dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}

	// Parse worktree blocks. Each starts with "worktree {path}".
	// The first block is always the main worktree.
	var worktrees []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			worktrees = append(worktrees, strings.TrimPrefix(line, "worktree "))
		}
	}

	if len(worktrees) <= 1 {
		// Only one worktree (main) or none — not a linked worktree.
		return "", nil
	}

	// Resolve dir to absolute path for comparison.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", nil
	}

	// Check if dir is within a non-main worktree.
	for _, wt := range worktrees[1:] { // skip main worktree (index 0)
		cleanWt := filepath.Clean(wt)
		if strings.HasPrefix(absDir, cleanWt+"/") || absDir == cleanWt {
			return cleanWt, nil
		}
	}

	// dir is in the main worktree or not in any — return empty.
	return "", nil
}

func (g *ExecGitResolver) TrackingBranch(ctx context.Context, dir string) (string, error) {
	return runGit(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "@{upstream}")
}

func (g *ExecGitResolver) UserEmail(ctx context.Context) (string, error) {
	return runGit(ctx, "git", "config", "--global", "user.email")
}

// WalkUpRemoteURL walks parent directories from dir until it finds one with a git remote.
// Returns (remoteURL, resolvedDir, nil) on success.
// Returns ("", "", nil) if no remote is found.
// Returns ("", "", err) only if context is cancelled or another unexpected error occurs.
//
// The walk stops at the repository boundary. Once git can name the worktree a
// directory belongs to, the walk never ascends past that worktree's top level.
// A repository nested inside another one — a submodule, a clone inside a clone,
// a plain `git init` in a subdirectory — has its own identity and does not
// inherit the remote of whatever repository happens to contain it. Adopting the
// parent's remote there merges two repositories into one project identity, so
// work recorded in the inner one is stamped as the outer one's and a push scoped
// to the inner repository uploads the outer repository's sessions.
//
// The stop costs one extra git call per level only on the path where no remote
// was found; a directory that has one answers on the first call and never
// reaches the check.
func (g *ExecGitResolver) WalkUpRemoteURL(ctx context.Context, dir string) (string, string, error) {
	current := filepath.Clean(dir)
	for {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		remote, err := g.RemoteURL(ctx, current)
		if err == nil && remote != "" {
			return remote, current, nil
		}
		// git answered which worktree current belongs to, and current IS its top
		// level: this repository has no remote and the next step up would leave
		// it. A directory git cannot place in a worktree (it does not exist, or
		// it is outside every repository) keeps walking, which is how a recorded
		// path that has since been deleted still resolves to its repository.
		if root, rootErr := g.ResolveRepositoryRoot(ctx, current); rootErr == nil && sameDirectory(root, current) {
			return "", "", nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root with no remote found.
			return "", "", nil
		}
		current = parent
	}
}

// sameDirectory reports whether two paths name the same directory, comparing
// through symlinks when both sides resolve. Git reports a worktree top level in
// its own resolved form, which need not be the string the caller walked down
// from, so a lexical comparison alone would miss the boundary.
func sameDirectory(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

// runGit executes a git command and returns trimmed stdout, or an error on
// non-zero exit.
func runGit(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", &gitError{args: strings.Join(args, " "), detail: detail, err: err}
	}
	return strings.TrimSpace(string(out)), nil
}

// gitError reports git's own diagnosis while keeping the underlying error in the
// chain.
//
// Both halves matter, and a formatted string can only have one of them. What a
// user must READ is git's explanation - "fatal: not a git repository" - because
// a bare "exit status 128" tells them nothing about why a path was rejected,
// least of all from inside a commit hook. What callers of this shared helper
// must still be able to INSPECT is the original error, so errors.Is and
// errors.As keep matching a cancelled context or an *exec.ExitError. Wrapping
// with %w would put the exit status back into the message; discarding the error
// breaks the chain. A dedicated type keeps both.
type gitError struct {
	args   string
	detail string
	err    error
}

func (e *gitError) Error() string { return fmt.Sprintf("git %s: %s", e.args, e.detail) }

// Unwrap exposes the original failure to errors.Is and errors.As.
func (e *gitError) Unwrap() error { return e.err }
