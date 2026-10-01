package autopublish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// Repository is what the matcher reads about one repository: the root Git
// reports and its remotes.
type Repository struct {
	Root string
	// MainRoot is the main worktree's root when Root is a linked worktree of
	// it, and "" otherwise. A folder rule that covers a repository covers its
	// linked worktrees, wherever they are: they are the same repository and
	// run its hooks.
	MainRoot string
	// Gone is the directory a session was recorded in when that directory no
	// longer exists, so Git can name no root. A folder rule covers it when the
	// glob names the directory or a folder above it: which of them was the
	// repository root is no longer known, so a gone repository nested in a
	// covered one is covered too.
	Gone string
	// Remote is the remote a repository-scoped push derives its identity
	// from: the checkout's upstream remote, else origin, else "".
	Remote string
	// Origin is the origin remote, or "". A remote rule covers the repository
	// when it names either remote, so switching to a branch that tracks a fork
	// does not take the repository out of its rule.
	Origin string
}

// RepositoryGit answers the questions Resolve asks Git. The ingest package's
// ExecGitResolver is the production answer, so a rule sees the same root and
// remote the push scope does.
type RepositoryGit interface {
	ResolveRepositoryRoot(ctx context.Context, dir string) (string, error)
	RemoteURL(ctx context.Context, dir string) (string, error)
	OriginRemoteURL(ctx context.Context, dir string) (string, error)
}

var _ RepositoryGit = (*ingest.ExecGitResolver)(nil)

// Resolve reads the repository dir belongs to, and its remotes when remotes
// is set: only a remote rule reads them, and each read is a git process. A
// directory that no longer exists names no repository: a parent it was inside
// is a different repository, which Peasant has not recorded.
func Resolve(ctx context.Context, git RepositoryGit, dir string, remotes bool) (Repository, error) {
	root, err := resolveRoot(ctx, git, dir)
	if err != nil {
		return Repository{}, err
	}
	repo := Repository{Root: root, MainRoot: mainRoot(root)}
	if remotes {
		repo.Remote, repo.Origin = readRemotes(ctx, git, root)
	}
	return repo, nil
}

// SessionRepository is the repository a session was recorded in, as the
// matcher reads it: the repository Git reports for dir, the directory the
// session was recorded in. When dir is gone, or is not in a repository, it
// is that path and the remote the session recorded; when no directory was
// recorded, it is that remote alone, so only a remote rule can cover it.
func SessionRepository(ctx context.Context, git RepositoryGit, dir, recordedRemote string, remotes bool) Repository {
	if strings.TrimSpace(dir) == "" {
		return Repository{Remote: recordedRemote, Origin: recordedRemote}
	}
	repo, err := Resolve(ctx, git, dir, remotes)
	if err != nil {
		return Repository{Gone: dir, Remote: recordedRemote, Origin: recordedRemote}
	}
	return repo
}

// resolveRoot is the root Git reports for the existing directory dir. An
// empty dir names no directory: it is not read as the current one.
func resolveRoot(ctx context.Context, git RepositoryGit, dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("no directory was recorded")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(absolute); statErr != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory on this computer", absolute)
	}
	root, err := git.ResolveRepositoryRoot(ctx, absolute)
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository: %w", absolute, err)
	}
	return root, nil
}

// readRemotes reads the remotes of the repository at root. A repository
// without a remote is still one a folder rule can cover, so a failed read
// leaves the remote empty.
func readRemotes(ctx context.Context, git RepositoryGit, root string) (remote, origin string) {
	remote, _ = git.RemoteURL(ctx, root)
	origin, _ = git.OriginRemoteURL(ctx, root)
	return remote, origin
}

// mainRoot is the main worktree's root when root is a linked worktree, read
// from the ".git" file Git writes there ("gitdir: <main>/.git/worktrees/<name>"),
// and "" otherwise.
func mainRoot(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	main, _, found := strings.Cut(filepath.ToSlash(filepath.Clean(gitdir)), "/.git/worktrees/")
	if !found {
		return ""
	}
	return filepath.FromSlash(main)
}

// RecordedDirectories lists every directory Peasant recorded a session in.
type RecordedDirectories interface {
	RecordedDirectories(ctx context.Context) ([]string, error)
}

var _ RecordedDirectories = (*store.Store)(nil)

// Recorded returns the repositories Peasant has recorded sessions in, each
// once, by root. A linked worktree is listed as its main repository, whose
// hooks it runs. A recorded directory that is gone, or that is not inside a
// repository, names no repository. These are the only repositories a rule
// installs a hook in.
func Recorded(ctx context.Context, reader RecordedDirectories, git RepositoryGit) ([]Repository, error) {
	dirs, err := reader.RecordedDirectories(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the directories Peasant recorded sessions in: %w", err)
	}
	// Many sessions share a repository, so the remotes are read once per root.
	roots := map[string]struct{}{}
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if root, err := resolveRoot(ctx, git, dir); err == nil {
			if main := mainRoot(root); main != "" {
				root = main
			}
			roots[root] = struct{}{}
		}
	}
	repositories := make([]Repository, 0, len(roots))
	for root := range roots {
		repo := Repository{Root: root}
		repo.Remote, repo.Origin = readRemotes(ctx, git, root)
		repositories = append(repositories, repo)
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Root < repositories[j].Root })
	return repositories, nil
}

// covered returns the repositories the rule's pattern names.
func covered(rule Rule, repositories []Repository) []Repository {
	var out []Repository
	for _, repo := range repositories {
		if rule.Covers(repo) {
			out = append(out, repo)
		}
	}
	return out
}

// ErrNotCovered is returned for an install in a path that is not a recorded
// repository the rule covers. Nothing is installed.
var ErrNotCovered = errors.New("not a recorded repository the rule covers")

// coveredTarget returns the recorded repository at path when the rule covers it, and
// ErrNotCovered otherwise. Every install for a rule asks it first, so nothing
// is installed in a repository Peasant has not recorded.
func coveredTarget(rule Rule, path string, recorded []Repository) (Repository, error) {
	for _, repo := range covered(rule, recorded) {
		if filepath.Clean(repo.Root) == filepath.Clean(path) {
			return repo, nil
		}
	}
	return Repository{}, fmt.Errorf("%s is %w %q; Peasant installs a hook only in a repository it has recorded sessions in", path, ErrNotCovered, rule.ID)
}

// Hooks reads and installs the hooks of a rule's events in one repository.
// Binding is bound into every hook it writes, so the hook reads the same
// configuration, rules, and store as the process that installed it.
type Hooks struct {
	Lifecycle *githooks.Lifecycle
	Binding   githooks.Binding
}

// View is the rule with the recorded repositories it covers, each with its
// hook per rule event, as the hooks are now.
func (h Hooks) View(ctx context.Context, rule Rule, recorded []Repository) (schema.AutoPublishRule, error) {
	repositories, err := h.States(ctx, rule, recorded)
	if err != nil {
		return schema.AutoPublishRule{}, err
	}
	request := rule.Request()
	return schema.AutoPublishRule{ID: rule.ID, Kind: request.Kind, Match: request.Match, Events: request.Events, Collectives: request.Collectives, Repositories: repositories}, nil
}

// States lists the recorded repositories the rule covers, each with its hook
// per rule event, as the hooks are now.
func (h Hooks) States(ctx context.Context, rule Rule, recorded []Repository) ([]schema.AutoPublishRepository, error) {
	h.Binding.RequireAutoPublishRule = true
	events, err := rule.hookEvents()
	if err != nil {
		return nil, err
	}
	repositories := []schema.AutoPublishRepository{}
	for _, repo := range covered(rule, recorded) {
		hooks := []schema.AutoPublishHook{}
		if len(events) > 0 {
			report, statusErr := h.Lifecycle.Status(ctx, githooks.Request{Dir: repo.Root, Events: events, Binding: h.Binding})
			if statusErr != nil {
				hooks = unreadable(events, repo.Root, statusErr)
			} else {
				for _, plan := range report.Plans {
					hooks = append(hooks, planHook(plan))
				}
			}
		}
		repositories = append(repositories, repositoryView(repo, hooks))
	}
	return repositories, nil
}

// Install installs the rule's hooks in one recorded repository the rule
// covers, one per rule event. Events are independent: each reports its own
// hook, and a hook Peasant does not manage is left as it is and carries the
// remedy. A path that is not a recorded repository the rule covers installs
// nothing and returns ErrNotCovered. The githooks report is returned too, for
// a caller that renders its warnings.
func (h Hooks) Install(ctx context.Context, rule Rule, path string, recorded []Repository) (schema.AutoPublishRepository, githooks.ChangeReport, error) {
	h.Binding.RequireAutoPublishRule = true
	target, err := coveredTarget(rule, path, recorded)
	if err != nil {
		return schema.AutoPublishRepository{}, githooks.ChangeReport{}, err
	}
	events, err := rule.hookEvents()
	if err != nil {
		return schema.AutoPublishRepository{}, githooks.ChangeReport{}, err
	}
	if len(events) == 0 {
		return schema.AutoPublishRepository{}, githooks.ChangeReport{}, fmt.Errorf("rule %q names no hook event, so there is nothing to install; add pre-push or post-commit to the rule first", rule.ID)
	}
	report, err := h.Lifecycle.Install(ctx, githooks.Request{Dir: target.Root, Events: events, Binding: h.Binding})
	if err != nil {
		hooks := make([]schema.AutoPublishHook, 0, len(events))
		for _, event := range events {
			hooks = append(hooks, schema.AutoPublishHook{Event: schema.AutoPublishEvent(event), Status: schema.AutoPublishHookFailed, Remedy: &schema.AutoPublishHookRemedy{Message: "Installing the hook failed before any file was written: " + err.Error()}})
		}
		return repositoryView(target, hooks), report, err
	}
	hooks := make([]schema.AutoPublishHook, 0, len(report.Results))
	for _, result := range report.Results {
		hooks = append(hooks, resultHook(result))
	}
	return repositoryView(target, hooks), report, nil
}

// planHook is one event's hook as it is now. A Peasant hook git would not run,
// because it lost its executable bit or names a repository that moved, is
// reported absent: installing is what repairs it, and installing is offered
// for an absent hook. A slot Peasant will not manage is blocked with the
// githooks remedy, which also says when a by-hand section already uploads.
func planHook(plan githooks.Plan) schema.AutoPublishHook {
	hook := schema.AutoPublishHook{Event: schema.AutoPublishEvent(plan.Event)}
	switch {
	case plan.Refusal != githooks.RefusalNone:
		hook.Status = schema.AutoPublishHookBlocked
		hook.Remedy = &schema.AutoPublishHookRemedy{Message: plan.Reason, Snippet: plan.Manual}
	case plan.Uploads():
		hook.Status = schema.AutoPublishHookInstalled
	default:
		hook.Status = schema.AutoPublishHookAbsent
	}
	return hook
}

// resultHook is one event's hook after an install: installed, or blocked or
// failed with the githooks remedy.
func resultHook(result githooks.Result) schema.AutoPublishHook {
	hook := schema.AutoPublishHook{Event: schema.AutoPublishEvent(result.Event)}
	switch result.Outcome {
	case githooks.OutcomeCreated, githooks.OutcomeReplaced:
		hook.Status = schema.AutoPublishHookInstalled
	case githooks.OutcomeRefused:
		hook.Status = schema.AutoPublishHookBlocked
		hook.Remedy = &schema.AutoPublishHookRemedy{Message: result.Reason, Snippet: result.Manual}
	default:
		hook.Status = schema.AutoPublishHookFailed
		hook.Remedy = &schema.AutoPublishHookRemedy{Message: result.Reason}
	}
	return hook
}

// unreadable reports every event blocked when the repository's hooks could not
// be read, with the reason and the command that shows more.
func unreadable(events []githooks.Event, root string, err error) []schema.AutoPublishHook {
	hooks := make([]schema.AutoPublishHook, 0, len(events))
	for _, event := range events {
		hooks = append(hooks, schema.AutoPublishHook{Event: schema.AutoPublishEvent(event), Status: schema.AutoPublishHookBlocked, Remedy: &schema.AutoPublishHookRemedy{
			Message: "Peasant could not read this repository's " + event.String() + " hook: " + err.Error() + ". Run '" + githooks.StatusCommand(root) + "' to see why.",
		}})
	}
	return hooks
}

func repositoryView(repo Repository, hooks []schema.AutoPublishHook) schema.AutoPublishRepository {
	label, _ := schema.RemoteLabel(repo.Remote)
	return schema.AutoPublishRepository{Path: repo.Root, Label: label, Hooks: hooks}
}
