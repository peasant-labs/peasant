package selectionfilter

import (
	"context"
	"path/filepath"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
)

type preparedSelection struct {
	candidate ingest.DiscoveryCandidate
	decision  ingest.DiscoveryDecision
	excluded  bool
	session   ingest.DiscoveredSession
}

// Filter applies a saved selection to one discovered cohort. Prepare must see
// the complete cohort before Match or Excludes. A session outside that cohort
// fails closed: Match refuses it, and Excludes reports it as denied so a child
// cannot inherit a selected parent's result. Filter and its recorder must be
// used serially by one goroutine; they are not safe for concurrent use.
type Filter struct {
	matcher               ingest.SelectionMatcher
	autoIngestNewBranches bool
	git                   ingest.GitResolver
	known                 ingest.KnownSessionIndex
	staleness             time.Duration
	pathResolver          ingest.PathIdentityResolver
	projectHarnesses      map[ingest.Harness]bool
	prepared              map[ingest.SessionID]preparedSelection
	recordedConflicts     map[ingest.SessionID]bool
	recorder              *ConflictRecorder
}

// Option configures a Filter.
type Option func(*Filter)

// WithKnownSessions gives the filter the index the tree's scan reads, so a
// session that is already stored and whose source has not changed is judged by
// the branch the store holds for it, exactly as the tree lists it. staleness is
// the window ingest and the scan share. A nil index changes nothing.
func WithKnownSessions(index ingest.KnownSessionIndex, staleness time.Duration) Option {
	return func(f *Filter) {
		f.known = index
		f.staleness = staleness
	}
}

// New builds a filter and the recorder that names sessions the filter withholds
// because project rules disagree. git may be nil; it is consulted only when a
// project rule or an exact branch exclusion can change the decision. r may be
// nil; an unresolved path stays empty and fails closed for path-only rules.
func New(cfg *config.Config, git ingest.GitResolver, r ingest.PathIdentityResolver, options ...Option) (*Filter, *ConflictRecorder) {
	recorder := &ConflictRecorder{}
	projectHarnesses := make(map[ingest.Harness]bool)
	if cfg != nil {
		for harness, selection := range cfg.Selection.Harnesses {
			if len(selection.Projects) > 0 || len(selection.Exclusions.Branches) > 0 {
				projectHarnesses[ingest.Harness(harness)] = true
			}
		}
	}
	matcher := ingest.SelectionMatcher{}
	autoIngest := false
	if cfg != nil {
		matcher = cfg.SelectionMatcher()
		autoIngest = cfg.Selection.AutoIngestNewBranches
	}
	filter := &Filter{
		matcher:               matcher,
		autoIngestNewBranches: autoIngest,
		git:                   git,
		pathResolver:          r,
		projectHarnesses:      projectHarnesses,
		recorder:              recorder,
	}
	for _, option := range options {
		option(filter)
	}
	return filter, recorder
}

// Prepare materializes every discovered session before the first Match call.
// Decisions are cached by session ID. Git lookups are memoized by directory and
// run only when the matcher can change its answer from repository evidence.
// A session belongs to the branch the tree lists it under, which
// ingest.SessionBranchFor decides for both: the branch its transcript
// recorded, else the branch the store holds for an unchanged stored session,
// else the branch checked out in its project directory.
// A cancelled context returns before this call's decisions are cached.
func (f *Filter) Prepare(ctx context.Context, sessions []ingest.DiscoveredSession) error {
	if f == nil {
		return nil
	}
	f.prepared = nil
	f.recordedConflicts = nil
	f.recorder.reset()
	var git ingest.GitResolver
	if f.git != nil {
		git = newMemoizedGit(f.git)
	}
	inputs := make([]CandidateInput, len(sessions))
	for index, session := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		projectPath := ingest.SessionProjectDirectory(session)
		branch := session.Branch
		gitRemote := ""
		if f.projectHarnesses[session.Harness] || f.matcher.DiscoveryNeedsGit(session.Harness, session.SessionID) {
			if dir := discoveredSessionGitDirectory(session, projectPath); dir != "" && git != nil {
				if remote, err := git.RemoteURL(ctx, dir); err == nil {
					gitRemote = remote
				}
			}
			branch = f.sessionBranch(ctx, git, session)
		}
		inputs[index] = CandidateInput{
			Harness:     session.Harness,
			GitRemote:   gitRemote,
			ProjectName: session.ProjectName,
			ProjectPath: projectPath,
			Branch:      branch,
			SessionID:   session.SessionID,
		}
	}

	candidates, err := PrepareCandidates(ctx, inputs, f.pathResolver)
	if err != nil {
		return err
	}
	f.prepared = make(map[ingest.SessionID]preparedSelection, len(candidates))
	f.recordedConflicts = make(map[ingest.SessionID]bool)
	f.recorder.reset()
	sessionsByID := make(map[ingest.SessionID]ingest.DiscoveredSession, len(sessions))
	for _, session := range sessions {
		sessionsByID[session.SessionID] = session
	}
	for _, candidate := range candidates {
		f.prepared[candidate.SessionID] = preparedSelection{
			candidate: candidate,
			decision:  f.matcher.MatchDiscoveryCandidateDecision(candidate, f.autoIngestNewBranches),
			excluded:  f.matcher.ExcludesCandidate(candidate),
			session:   sessionsByID[candidate.SessionID],
		}
	}
	return nil
}

// Excludes reports the exact-denial result cached during complete-cohort
// preparation. An unknown session, including any session seen before Prepare,
// fails closed so a child outside that cohort cannot inherit a selected parent.
func (f *Filter) Excludes(session ingest.DiscoveredSession) bool {
	if f == nil || f.prepared == nil {
		return true
	}
	prepared, ok := f.prepared[session.SessionID]
	return !ok || prepared.excluded
}

// Match reports whether the prepared cohort admitted the session. A session
// that was not in that cohort is refused. A withheld conflict is recorded once
// and is not admitted.
func (f *Filter) Match(session ingest.DiscoveredSession) bool {
	if f == nil || f.prepared == nil {
		return false
	}
	prepared, ok := f.prepared[session.SessionID]
	if !ok {
		return false
	}
	switch prepared.decision.Match {
	case ingest.BranchMatchYes:
		return true
	case ingest.BranchMatchWithheldConflict:
		if !f.recordedConflicts[session.SessionID] {
			f.recorder.record(selectionConflict{
				session: prepared.session,
				branch:  prepared.candidate.Branch,
				entries: prepared.decision.Conflicting(),
			}, f.recordedConflicts)
		}
	}
	return false
}

// sessionBranch is the branch the tree lists this session under: the answer of
// the function the tree asks, over the same index.
func (f *Filter) sessionBranch(ctx context.Context, git ingest.GitResolver, session ingest.DiscoveredSession) string {
	var record ingest.KnownSession
	stored := false
	if f.known != nil {
		record, stored = f.known.Reusable(session, f.staleness)
	}
	return ingest.SessionBranchFor(ctx, git, session, record, stored)
}

type gitAnswer struct {
	value string
	err   error
}

// memoizedGit answers each directory's remote and checked-out branch once for a
// Prepare call. Only the two questions selection asks are remembered; the rest
// of the resolver is the embedded one.
type memoizedGit struct {
	ingest.GitResolver
	remotes  map[string]gitAnswer
	branches map[string]gitAnswer
}

var _ ingest.GitResolver = (*memoizedGit)(nil)

func newMemoizedGit(git ingest.GitResolver) *memoizedGit {
	return &memoizedGit{
		GitResolver: git,
		remotes:     make(map[string]gitAnswer),
		branches:    make(map[string]gitAnswer),
	}
}

func (m *memoizedGit) RemoteURL(ctx context.Context, dir string) (string, error) {
	if answer, ok := m.remotes[dir]; ok {
		return answer.value, answer.err
	}
	value, err := m.GitResolver.RemoteURL(ctx, dir)
	m.remotes[dir] = gitAnswer{value: value, err: err}
	return value, err
}

func (m *memoizedGit) Branch(ctx context.Context, dir string) (string, error) {
	if answer, ok := m.branches[dir]; ok {
		return answer.value, answer.err
	}
	value, err := m.GitResolver.Branch(ctx, dir)
	m.branches[dir] = gitAnswer{value: value, err: err}
	return value, err
}

func discoveredSessionGitDirectory(session ingest.DiscoveredSession, projectPath string) string {
	if projectPath != "" {
		return projectPath
	}
	dir := string(session.OriginalRoot)
	if dir == "" && session.SourcePath != "" {
		dir = filepath.Dir(string(session.SourcePath))
	}
	return dir
}
