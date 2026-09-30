package autopublish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/schema"
)

// Repository is what the matcher reads about one repository: the root Git
// reports and the remote a repository-scoped push derives its identity from.
type Repository struct {
	Root string
	// Remote is the checkout's upstream remote, else origin, else "".
	Remote string
}

// RepositoryGit answers the two questions Resolve asks Git. The ingest
// package's ExecGitResolver is the production answer, so a rule sees the same
// root and remote the push scope does.
type RepositoryGit interface {
	ResolveRepositoryRoot(ctx context.Context, dir string) (string, error)
	RemoteURL(ctx context.Context, dir string) (string, error)
}

// Resolve reads the repository dir belongs to.
func Resolve(ctx context.Context, git RepositoryGit, dir string) (Repository, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return Repository{}, err
	}
	root, err := git.ResolveRepositoryRoot(ctx, existingAncestor(absolute))
	if err != nil {
		return Repository{}, fmt.Errorf("%s is not inside a git repository: %w", absolute, err)
	}
	// A repository without a remote is still a repository a folder rule can
	// cover, so a failed remote read leaves the remote empty.
	remote, _ := git.RemoteURL(ctx, root)
	return Repository{Root: root, Remote: remote}, nil
}

// RecordedDirectories lists every directory Peasant recorded a session in.
type RecordedDirectories interface {
	RecordedDirectories(ctx context.Context) ([]string, error)
}

// Recorded returns the repositories Peasant has recorded sessions in, each
// once, by root. A recorded directory that is gone, or that is not inside a
// repository, names no repository. These are the only repositories a rule
// installs a hook in.
func Recorded(ctx context.Context, reader RecordedDirectories, git RepositoryGit) ([]Repository, error) {
	dirs, err := reader.RecordedDirectories(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the directories Peasant recorded sessions in: %w", err)
	}
	byRoot := map[string]Repository{}
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		repo, err := Resolve(ctx, git, dir)
		if err != nil {
			continue
		}
		if _, seen := byRoot[repo.Root]; !seen {
			byRoot[repo.Root] = repo
		}
	}
	repositories := make([]Repository, 0, len(byRoot))
	for _, repo := range byRoot {
		repositories = append(repositories, repo)
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Root < repositories[j].Root })
	return repositories, nil
}

// Covered returns the repositories the rule's pattern names.
func Covered(rule Rule, repositories []Repository) []Repository {
	var covered []Repository
	for _, repo := range repositories {
		if rule.Covers(repo) {
			covered = append(covered, repo)
		}
	}
	return covered
}

// Hooks reads and installs the hooks of a rule's events in one repository.
// Binding is bound into every hook it writes, so the hook reads the same
// configuration, rules, and store as the process that installed it.
type Hooks struct {
	Lifecycle *githooks.Lifecycle
	Binding   githooks.Binding
}

// ErrNotCovered is returned for an install in a path that is not a recorded
// repository the rule covers. Nothing is installed.
var ErrNotCovered = errors.New("not a recorded repository the rule covers")

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
	events, err := rule.HookEvents()
	if err != nil {
		return nil, err
	}
	repositories := []schema.AutoPublishRepository{}
	for _, repo := range Covered(rule, recorded) {
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
// nothing and returns ErrNotCovered.
func (h Hooks) Install(ctx context.Context, rule Rule, path string, recorded []Repository) (schema.AutoPublishRepository, error) {
	var target *Repository
	for _, repo := range Covered(rule, recorded) {
		if filepath.Clean(repo.Root) == filepath.Clean(path) {
			target = &repo
			break
		}
	}
	if target == nil {
		return schema.AutoPublishRepository{}, fmt.Errorf("%s is %w %q; Peasant installs a hook only in a repository it has recorded sessions in", path, ErrNotCovered, rule.ID)
	}
	events, err := rule.HookEvents()
	if err != nil {
		return schema.AutoPublishRepository{}, err
	}
	if len(events) == 0 {
		return schema.AutoPublishRepository{}, fmt.Errorf("rule %q names no hook event, so there is nothing to install; add pre-push or post-commit to the rule first", rule.ID)
	}
	report, err := h.Lifecycle.Install(ctx, githooks.Request{Dir: target.Root, Events: events, Binding: h.Binding})
	if err != nil {
		hooks := make([]schema.AutoPublishHook, 0, len(events))
		for _, event := range events {
			hooks = append(hooks, schema.AutoPublishHook{Event: schema.AutoPublishEvent(event), Status: schema.AutoPublishHookFailed, Remedy: &schema.AutoPublishHookRemedy{Message: "Installing the hook failed before any file was written: " + err.Error()}})
		}
		return repositoryView(*target, hooks), nil
	}
	hooks := make([]schema.AutoPublishHook, 0, len(report.Results))
	for _, result := range report.Results {
		hooks = append(hooks, ResultHook(result))
	}
	return repositoryView(*target, hooks), nil
}

// planHook is one event's hook as it is now.
func planHook(plan githooks.Plan) schema.AutoPublishHook {
	hook := schema.AutoPublishHook{Event: schema.AutoPublishEvent(plan.Event)}
	switch {
	case plan.Refusal != githooks.RefusalNone:
		hook.Status = schema.AutoPublishHookBlocked
		hook.Remedy = &schema.AutoPublishHookRemedy{Message: plan.Reason, Snippet: plan.Manual}
	case plan.Ownership == githooks.OwnershipPeasant:
		hook.Status = schema.AutoPublishHookInstalled
	default:
		hook.Status = schema.AutoPublishHookAbsent
	}
	return hook
}

// ResultHook is one event's hook after an install: installed, or blocked or
// failed with the githooks remedy.
func ResultHook(result githooks.Result) schema.AutoPublishHook {
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

// Publishing reports whether a Peasant-managed hook that git runs is installed
// in the repository dir belongs to, for any event, so a commit or push there
// publishes without a click. The hook may have been installed for a rule or
// from the terminal, and it outlives the rule. A hook git skips, because it
// lost its executable bit or names a repository that has moved, does not
// publish. A directory Git cannot read reports false.
func Publishing(ctx context.Context, lifecycle *githooks.Lifecycle, dir string) bool {
	report, err := lifecycle.Status(ctx, githooks.Request{Dir: existingAncestor(dir)})
	if err != nil {
		return false
	}
	for _, plan := range report.Plans {
		if plan.Managed() && !slices.ContainsFunc(plan.Warnings, inert) {
			return true
		}
	}
	return false
}

func inert(warning githooks.Warning) bool {
	return warning.Kind == githooks.WarningHookNotExecutable || warning.Kind == githooks.WarningRepositoryMoved
}

// existingAncestor is dir, or its nearest ancestor that exists: a session
// recorded in a directory since removed still belongs to the repository that
// held it.
func existingAncestor(dir string) string {
	current := filepath.Clean(dir)
	for {
		if info, err := os.Stat(current); err == nil && info.IsDir() {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return dir
		}
		current = parent
	}
}
