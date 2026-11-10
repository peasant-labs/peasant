package selectionfilter

import (
	"context"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
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
// cannot inherit a selected parent's result.
type Filter struct {
	matcher               ingest.SelectionMatcher
	autoIngestNewBranches bool
	git                   ingest.GitResolver
	pathResolver          ingest.PathIdentityResolver
	projectHarnesses      map[ingest.Harness]bool
	prepared              map[ingest.SessionID]preparedSelection
	recordedConflicts     map[ingest.SessionID]bool
	recorder              *ConflictRecorder
}

// New builds a filter and the recorder that names sessions the filter withholds
// because project rules disagree. git may be nil; it is consulted only when a
// project rule or an exact branch exclusion can change the decision. r may be
// nil; an unresolved path stays empty and fails closed for path-only rules.
func New(cfg *config.Config, git ingest.GitResolver, r ingest.PathIdentityResolver) (*Filter, *ConflictRecorder) {
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
	return filter, recorder
}

// Prepare materializes every discovered session before the first Match call.
// Decisions are cached by session ID. Git lookups are memoized by directory and
// run only when the matcher can change its answer from repository evidence.
// A cancelled context returns before this call's decisions are cached.
func (f *Filter) Prepare(ctx context.Context, sessions []ingest.DiscoveredSession) error {
	if f == nil {
		return nil
	}
	type gitContext struct{ remote, branch string }
	resolvedGit := make(map[string]gitContext)
	inputs := make([]CandidateInput, len(sessions))
	for index, session := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		projectPath := discoveredSessionProjectPath(session)
		branch := session.Branch
		gitRemote := ""
		if (f.projectHarnesses[session.Harness] || f.matcher.DiscoveryNeedsGit(session.Harness, session.SessionID)) && f.git != nil {
			dir := discoveredSessionGitDirectory(session, projectPath)
			if dir != "" {
				gitCtx, ok := resolvedGit[dir]
				if !ok {
					if remote, err := f.git.RemoteURL(ctx, dir); err == nil {
						gitCtx.remote = remote
					}
					if resolvedBranch, err := f.git.Branch(ctx, dir); err == nil {
						gitCtx.branch = resolvedBranch
					}
					resolvedGit[dir] = gitCtx
				}
				gitRemote = gitCtx.remote
				if gitCtx.branch != "" {
					branch = gitCtx.branch
				}
			}
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
	for index, candidate := range candidates {
		f.prepared[candidate.SessionID] = preparedSelection{
			candidate: candidate,
			decision:  f.matcher.MatchDiscoveryCandidateDecision(candidate, f.autoIngestNewBranches),
			excluded:  f.matcher.ExcludesCandidate(candidate),
			session:   sessions[index],
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

func discoveredSessionProjectPath(session ingest.DiscoveredSession) string {
	if filepath.IsAbs(session.CWD) {
		return filepath.Clean(session.CWD)
	}
	if session.Harness == defaults.HarnessClaudeCode {
		slug := filepath.Base(filepath.Dir(string(session.SourcePath)))
		if decoded := decodeClaudeSlugToPath(slug); decoded != "" {
			return decoded
		}
	}
	return ""
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

// decodeClaudeSlugToPath decodes a Claude project slug to the real filesystem
// path, using the same directory check the harvest command uses.
func decodeClaudeSlugToPath(encoded string) string {
	return ingest.DecodeClaudeSlug(encoded, func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.IsDir()
	})
}
