package githooks

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultCommandTimeout bounds every git invocation this package makes. Each
// logical question is one call, so a repository that hangs git cannot hang the
// hook lifecycle.
const DefaultCommandTimeout = 5 * time.Second

// GitResolver answers the questions the hook lifecycle needs about a
// repository. It is an interface so callers can supply their own resolver;
// production uses ExecGit.
type GitResolver interface {
	// Root returns the absolute worktree top level containing dir.
	Root(ctx context.Context, dir string) (string, error)
	// GitDir returns the absolute Git directory of dir. A hook written inside
	// the worktree but outside this directory is committable content, which
	// changes who ends up running it.
	GitDir(ctx context.Context, dir string) (string, error)
	// HookPath returns the absolute path Git executes for event in dir. The
	// answer must come from Git so that an already-configured hooks directory
	// is honored without Peasant ever reading or writing that configuration.
	HookPath(ctx context.Context, dir string, event Event) (string, error)
}

// ExecGit answers repository questions by running bounded git commands in
// argument-list form. It never uses a shell and never writes to a repository or
// to Git configuration.
type ExecGit struct {
	// Timeout bounds one git invocation. DefaultCommandTimeout when zero.
	Timeout time.Duration
}

var _ GitResolver = (*ExecGit)(nil)

// NewExecGit returns the production resolver with the default timeout.
func NewExecGit() *ExecGit { return &ExecGit{Timeout: DefaultCommandTimeout} }

func (g *ExecGit) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return DefaultCommandTimeout
}

// Root resolves the worktree top level for dir.
func (g *ExecGit) Root(ctx context.Context, dir string) (string, error) {
	abs, err := absoluteDir(dir, "githooks.ExecGit.Root")
	if err != nil {
		return "", err
	}
	out, err := g.run(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf(
			"githooks: %s is not inside a Git worktree\n"+
				"What went wrong: git rev-parse --show-toplevel failed there.\n"+
				"Why: %v\n"+
				"Where: githooks.ExecGit.Root.\n"+
				"When: while resolving the repository, before any hook file was inspected or written.\n"+
				"Impact: nothing was planned, installed, or removed.\n"+
				"Fix: run the command from inside a Git repository, or pass --dir pointing at one.",
			abs, err,
		)
	}
	return filepath.Clean(out), nil
}

// GitDir resolves the Git directory backing dir. It is asked of Git rather than
// assumed to be "<root>/.git" so a repository whose Git directory lives
// elsewhere is classified correctly.
func (g *ExecGit) GitDir(ctx context.Context, dir string) (string, error) {
	abs, err := absoluteDir(dir, "githooks.ExecGit.GitDir")
	if err != nil {
		return "", err
	}
	out, err := g.run(ctx, abs, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf(
			"githooks: cannot resolve the Git directory of %s\n"+
				"What went wrong: git rev-parse --absolute-git-dir failed there.\n"+
				"Why: %v\n"+
				"Where: githooks.ExecGit.GitDir.\n"+
				"When: while resolving the repository, before any hook file was inspected or written.\n"+
				"Impact: nothing was planned, installed, or removed.\n"+
				"Fix: run the command from inside a Git repository, or pass --dir pointing at one.",
			abs, err,
		)
	}
	return filepath.Clean(out), nil
}

// HookPath resolves the file Git executes for event. git rev-parse --git-path
// already resolves whichever hooks directory Git is configured to use, so
// Peasant learns the effective path without ever reading that setting itself.
// Git answers relative to the directory it was run in, so the result is
// re-anchored to that directory.
func (g *ExecGit) HookPath(ctx context.Context, dir string, event Event) (string, error) {
	if err := event.Validate(); err != nil {
		return "", err
	}
	abs, err := absoluteDir(dir, "githooks.ExecGit.HookPath")
	if err != nil {
		return "", err
	}
	out, err := g.run(ctx, abs, "rev-parse", "--git-path", "hooks/"+event.String())
	if err != nil {
		return "", fmt.Errorf(
			"githooks: cannot resolve the %s hook path for %s\n"+
				"What went wrong: git rev-parse --git-path hooks/%s failed.\n"+
				"Why: %v\n"+
				"Where: githooks.ExecGit.HookPath.\n"+
				"When: while resolving the effective hook path, before any hook file was inspected or written.\n"+
				"Impact: nothing was planned, installed, or removed for this event.\n"+
				"Fix: confirm %s is inside a usable Git worktree and that the git command is on PATH.",
			event, abs, event, err, abs,
		)
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(abs, out)
	}
	return filepath.Clean(out), nil
}

// run executes one git command against dir and returns trimmed stdout.
func (g *ExecGit) run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()

	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
	}
	return strings.TrimSpace(string(out)), nil
}

// absoluteDir normalizes a caller-supplied directory. An empty value means the
// process working directory, matching the CLI's --dir default.
func absoluteDir(dir, where string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf(
			"githooks: cannot resolve directory %q to an absolute path\n"+
				"What went wrong: %v\n"+
				"Why: the process working directory could not be determined, or the path is malformed.\n"+
				"Where: %s.\n"+
				"When: while normalizing the requested repository directory, before any git command ran.\n"+
				"Impact: nothing was planned, installed, or removed.\n"+
				"Fix: pass --dir with an absolute path to the repository.",
			dir, err, where,
		)
	}
	return abs, nil
}
