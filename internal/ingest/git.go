package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
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

// RepositoryIdentityResolver resolves a physical worktree path to its logical
// repository cohort and physical Git directory. Callers retain the original
// ClonePath for matching, persistence, publishing, and destructive operations.
// Once Git's physical common directory is known, later topology failures return
// it alongside the error so callers can fall back to that physical cohort.
type RepositoryIdentityResolver interface {
	ResolveRepositoryIdentity(ctx context.Context, dir ClonePath) (RepositoryIdentity, error)
}

// ExecGitResolver shells out to git using exec.Command (argument-list form, NEVER sh -c).
// The private identity dependencies keep the recursive topology algorithm
// deterministic under tests without creating a second production resolver.
type ExecGitResolver struct {
	identityRunGit       func(ctx context.Context, args ...string) (string, error)
	identityPathResolver PathIdentityResolver
}

var _ GitResolver = (*ExecGitResolver)(nil)
var _ RepositoryIdentityResolver = (*ExecGitResolver)(nil)

// NewGitRepositoryIdentityResolver returns the production Git topology
// resolver.
func NewGitRepositoryIdentityResolver() RepositoryIdentityResolver {
	return &ExecGitResolver{}
}

const maximumRepositoryIdentityDepth = 32

// ResolveRepositoryIdentity asks Git for the repository topology rooted at dir.
// Ordinary linked worktrees share a cohort through their physical common Git
// directory. A declared submodule derives its cohort recursively from the
// direct superproject cohort and its normalized relative path, so equal
// submodules beneath linked superproject worktrees group without remote-based
// inference. Git owns interpretation of .git directories, gitdir files,
// relative pointers, bare repositories, and separate-git-dir layouts.
func (g *ExecGitResolver) ResolveRepositoryIdentity(ctx context.Context, dir ClonePath) (RepositoryIdentity, error) {
	if dir == "" {
		return RepositoryIdentity{}, newRepositoryIdentityError(
			dir,
			"starting repository topology resolution",
			"the physical worktree path is empty",
			"this worktree cannot be grouped with related worktrees",
			"resolve the project directory to a ClonePath before asking for its repository identity",
			nil,
		)
	}
	return g.resolveRepositoryIdentity(ctx, dir, 0, make(map[ClonePath]struct{}))
}

func (g *ExecGitResolver) resolveRepositoryIdentity(
	ctx context.Context,
	dir ClonePath,
	depth int,
	seen map[ClonePath]struct{},
) (RepositoryIdentity, error) {
	if depth >= maximumRepositoryIdentityDepth {
		return RepositoryIdentity{}, newRepositoryIdentityError(
			dir,
			"walking direct superproject repositories",
			fmt.Sprintf("the repository topology exceeds the supported depth of %d", maximumRepositoryIdentityDepth),
			"Peasant cannot prove a stable repository cohort for this checkout",
			"remove the topology cycle or reduce the nested submodule depth, then retry",
			nil,
		)
	}

	physicalDir, err := g.resolveIdentityPath(dir.String())
	if err != nil {
		return RepositoryIdentity{}, newRepositoryIdentityError(
			dir,
			"normalizing the repository worktree",
			"physical path resolution failed",
			"Peasant cannot inspect the repository topology safely",
			"restore access to the worktree and retry",
			err,
		)
	}
	commonDir, err := g.runIdentityGit(ctx, "git", "-C", physicalDir.String(), "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return RepositoryIdentity{}, newRepositoryIdentityError(
			dir,
			"reading Git's common directory",
			"git rev-parse --git-common-dir failed",
			"this worktree will use an exact-path fail-safe instead of repository grouping",
			"verify that the directory is an accessible Git worktree and retry",
			err,
		)
	}
	physicalCommonDir, err := g.resolveIdentityPath(commonDir)
	if err != nil {
		return RepositoryIdentity{}, newRepositoryIdentityError(
			dir,
			"normalizing Git's common directory",
			fmt.Sprintf("the reported Git directory %q could not be resolved physically", commonDir),
			"this worktree will use an exact-path fail-safe instead of repository grouping",
			"restore access to the Git directory and retry",
			err,
		)
	}
	gitDirectory := RepositoryPath(physicalCommonDir.String())

	superproject, err := g.runIdentityGit(ctx, "git", "-C", physicalDir.String(), "rev-parse", "--show-superproject-working-tree")
	if err != nil {
		return RepositoryIdentity{GitDirectory: gitDirectory}, newRepositoryIdentityError(
			dir,
			"checking for a direct superproject",
			"git rev-parse --show-superproject-working-tree failed",
			"Peasant cannot determine whether this checkout is an ordinary repository or a submodule",
			"repair the Git worktree metadata and retry",
			err,
		)
	}
	if superproject == "" {
		return RepositoryIdentity{
			CohortKey:    ordinaryRepositoryCohortKey(gitDirectory),
			GitDirectory: gitDirectory,
		}, nil
	}

	currentTop, err := g.resolvePhysicalTopLevel(ctx, physicalDir)
	if err != nil {
		return RepositoryIdentity{GitDirectory: gitDirectory}, err
	}
	if _, repeated := seen[currentTop]; repeated {
		return RepositoryIdentity{GitDirectory: gitDirectory}, newRepositoryIdentityError(
			dir,
			"walking direct superproject repositories",
			fmt.Sprintf("the physical repository top level %q repeats in its own superproject chain", currentTop),
			"Peasant detected an invalid or cyclic repository topology",
			"repair the submodule worktree links and retry",
			nil,
		)
	}
	nextSeen := make(map[ClonePath]struct{}, len(seen)+1)
	for repositoryTop := range seen {
		nextSeen[repositoryTop] = struct{}{}
	}
	nextSeen[currentTop] = struct{}{}

	physicalSuperproject, err := g.resolveIdentityPath(superproject)
	if err != nil {
		return RepositoryIdentity{GitDirectory: gitDirectory}, newRepositoryIdentityError(
			dir,
			"normalizing the direct superproject worktree",
			fmt.Sprintf("Git reported an inaccessible superproject worktree %q", superproject),
			"Peasant cannot verify the declared submodule relationship",
			"restore the direct superproject worktree and retry",
			err,
		)
	}
	superprojectTop, err := g.resolvePhysicalTopLevel(ctx, physicalSuperproject)
	if err != nil {
		return RepositoryIdentity{GitDirectory: gitDirectory}, err
	}
	relativePath, err := directSubmodulePath(superprojectTop, currentTop)
	if err != nil {
		return RepositoryIdentity{GitDirectory: gitDirectory}, newRepositoryIdentityError(
			dir,
			"checking submodule containment",
			err.Error(),
			"the reported child repository is not a valid direct descendant of its superproject",
			"repair the submodule checkout location and retry",
			err,
		)
	}
	if err := g.verifyDirectSubmoduleDeclaration(ctx, superprojectTop, relativePath); err != nil {
		return RepositoryIdentity{GitDirectory: gitDirectory}, newRepositoryIdentityError(
			dir,
			"verifying the direct .gitmodules declaration",
			err.Error(),
			"Peasant will not group a nested repository as a submodule without direct declaration evidence",
			"restore the matching submodule path in the direct superproject .gitmodules file and retry",
			err,
		)
	}
	parentIdentity, err := g.resolveRepositoryIdentity(ctx, superprojectTop, depth+1, nextSeen)
	if err != nil {
		return RepositoryIdentity{GitDirectory: gitDirectory}, newRepositoryIdentityError(
			dir,
			"resolving the direct superproject cohort",
			"the parent repository identity could not be resolved",
			"Peasant cannot derive this submodule cohort safely",
			"repair the reported parent topology and retry",
			err,
		)
	}
	if parentIdentity.CohortKey == "" {
		return RepositoryIdentity{GitDirectory: gitDirectory}, newRepositoryIdentityError(
			dir,
			"deriving the submodule cohort key",
			"the direct superproject returned an empty cohort key",
			"Peasant cannot derive this submodule cohort safely",
			"repair the reported parent topology and retry",
			nil,
		)
	}
	return RepositoryIdentity{
		CohortKey:    submoduleRepositoryCohortKey(parentIdentity.CohortKey, relativePath),
		GitDirectory: gitDirectory,
	}, nil
}

func (g *ExecGitResolver) resolvePhysicalTopLevel(ctx context.Context, dir ClonePath) (ClonePath, error) {
	top, err := g.runIdentityGit(ctx, "git", "-C", dir.String(), "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return "", newRepositoryIdentityError(
			dir,
			"reading the canonical repository top level",
			"git rev-parse --show-toplevel failed",
			"Peasant cannot verify repository containment or direct submodule declarations",
			"verify that the directory is an accessible non-bare worktree and retry",
			err,
		)
	}
	physicalTop, err := g.resolveIdentityPath(top)
	if err != nil {
		return "", newRepositoryIdentityError(
			dir,
			"normalizing the canonical repository top level",
			fmt.Sprintf("Git reported an inaccessible top level %q", top),
			"Peasant cannot verify repository containment or direct submodule declarations",
			"restore access to the repository top level and retry",
			err,
		)
	}
	return physicalTop, nil
}

func directSubmodulePath(superprojectTop, childTop ClonePath) (string, error) {
	relative, err := filepath.Rel(superprojectTop.String(), childTop.String())
	if err != nil {
		return "", fmt.Errorf("calculate child path relative to superproject %q: %w", superprojectTop, err)
	}
	relative = filepath.Clean(relative)
	if relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("child top level %q is not contained beneath direct superproject %q", childTop, superprojectTop)
	}
	return filepath.ToSlash(relative), nil
}

func (g *ExecGitResolver) verifyDirectSubmoduleDeclaration(ctx context.Context, superprojectTop ClonePath, relativePath string) error {
	modulesPath := filepath.Join(superprojectTop.String(), ".gitmodules")
	output, err := g.runIdentityGit(
		ctx,
		"git",
		"-C",
		superprojectTop.String(),
		"config",
		"--file",
		modulesPath,
		"--get-regexp",
		`^submodule\..*\.path$`,
	)
	if err != nil {
		return fmt.Errorf("read direct submodule paths from %q through git config: %w", modulesPath, err)
	}
	for _, line := range strings.Split(output, "\n") {
		separator := strings.IndexAny(line, " \t")
		if separator < 0 {
			continue
		}
		declared := filepath.ToSlash(filepath.Clean(strings.TrimSpace(line[separator+1:])))
		if declared == relativePath {
			return nil
		}
	}
	return fmt.Errorf("relative child path %q is not declared directly in %q", relativePath, modulesPath)
}

func (g *ExecGitResolver) runIdentityGit(ctx context.Context, args ...string) (string, error) {
	if g != nil && g.identityRunGit != nil {
		return g.identityRunGit(ctx, args...)
	}
	if len(args) == 0 {
		return "", fmt.Errorf("run repository identity Git command: no executable was provided")
	}
	return runGit(ctx, args[0], args[1:]...)
}

func (g *ExecGitResolver) resolveIdentityPath(raw string) (ClonePath, error) {
	if g != nil && g.identityPathResolver != nil {
		return g.identityPathResolver.Resolve(raw)
	}
	return NewPhysicalPathResolver().Resolve(raw)
}

func ordinaryRepositoryCohortKey(gitDirectory RepositoryPath) RepositoryCohortKey {
	return RepositoryCohortKey("repo:" + lengthPrefixedRepositoryComponent(gitDirectory.String()))
}

func submoduleRepositoryCohortKey(superproject RepositoryCohortKey, relativePath string) RepositoryCohortKey {
	return RepositoryCohortKey(
		"submodule:" +
			lengthPrefixedRepositoryComponent(superproject.String()) +
			lengthPrefixedRepositoryComponent(relativePath),
	)
}

func lengthPrefixedRepositoryComponent(value string) string {
	return strconv.Itoa(len(value)) + ":" + value
}

func newRepositoryIdentityError(
	dir ClonePath,
	operation string,
	reason string,
	meaning string,
	fix string,
	cause error,
) error {
	message := fmt.Sprintf(
		"resolve repository identity for %q: what: Peasant could not resolve a safe repository identity; why: %s; where: ExecGitResolver.ResolveRepositoryIdentity in internal/ingest/git.go; when: %s; meaning: %s; fix: %s",
		dir,
		reason,
		operation,
		meaning,
		fix,
	)
	if cause == nil {
		return fmt.Errorf("%s", message)
	}
	return fmt.Errorf("%s: %w", message, cause)
}

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
