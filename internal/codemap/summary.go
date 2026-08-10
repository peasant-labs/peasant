package codemap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/schema"
)

// ResolveProject resolves a single explicit display identity without applying
// discovery visibility or returning sibling project names. This preserves
// stable deep links while ProjectSummaries remains selection-filtered.
func (s *Service) ResolveProject(ctx context.Context, project string) (*schema.ProjectResolutionPayload, error) {
	if canonical, hashErr := schema.NewProjectHash(project); hashErr == nil {
		cwd, gitRemote, found, err := s.queryProjectCwd(ctx, canonical)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: canonical project hash %q is not stored; open a project returned by kickstart or use an existing saved URL", ErrProjectNotFound, canonical)
		}
		fallback := cwd
		if fallback == "" {
			fallback = canonical.String()
		}
		display := projectlabel.Label(gitRemote, fallback)
		return &schema.ProjectResolutionPayload{Project: display, ProjectHash: canonical}, nil
	}
	if invalidLegacyProjectLabel(project) {
		return nil, fmt.Errorf("%w: legacy project label %q is empty or contains control characters; use the exact saved label or a canonical 64-character lowercase hexadecimal project hash", ErrProjectIdentityInvalid, project)
	}

	projects, err := s.queryProjects(ctx)
	if err != nil {
		return nil, err
	}
	matches := make([]projectRow, 0, 1)
	for _, candidate := range projects {
		fallback := candidate.cwd
		if fallback == "" {
			fallback = candidate.hash.String()
		}
		display := projectlabel.Label(candidate.gitRemote, fallback)
		if display == project {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		// Not-found and hidden-match are already textually identical (same
		// ErrProjectNotFound message, below and in case 1) so a caller can't
		// tell them apart by response body. Also run the SAME
		// querySessions+Visible work case 1 runs, on the zero-value
		// ProjectHash (which matches no rows — a harmless no-op query), so
		// the two failure paths have equivalent DB-query cost and can't be
		// told apart by response latency either: without this, only the
		// hidden-match path would pay the extra querySessions round-trip,
		// leaving a timing side-channel even after the text was equalized.
		if _, err := s.projectHasVisibleSession(ctx, ""); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: legacy project label %q does not identify a stored project; choose a current project or use its canonical hash URL", ErrProjectNotFound, project)
	case 1:
		// Unlike the canonical-hash branch above (a deliberate, reviewed
		// exemption for stable deep links — schema.NewProjectHash's opaque
		// 64-hex value is not name-guessable), a legacy label is a
		// human-readable working-directory path that is frequently
		// guessable/memorable. Resolving it must therefore respect the
		// configured selection, or an unauthenticated caller could recover a
		// hidden project's identity+hash by guessing its label — defeating
		// "selected-empty exposes nothing" for any project whose path is
		// known. Fail with the SAME ErrProjectNotFound class as the
		// zero-matches case above (both text AND DB-query shape — see the
		// comment there) so a hidden project is indistinguishable from a
		// nonexistent one (closing the existence oracle).
		hasVisibleSession, err := s.projectHasVisibleSession(ctx, matches[0].hash)
		if err != nil {
			return nil, err
		}
		if !hasVisibleSession {
			return nil, fmt.Errorf("%w: legacy project label %q does not identify a stored project; choose a current project or use its canonical hash URL", ErrProjectNotFound, project)
		}
		return &schema.ProjectResolutionPayload{Project: project, ProjectHash: matches[0].hash}, nil
	default:
		return nil, fmt.Errorf("%w: legacy project label %q matches %d stored projects; use the canonical project hash URL to choose one", ErrProjectAmbiguous, project, len(matches))
	}
}

// projectHasVisibleSession reports whether projectHash has at least one
// session the configured selection makes visible — the same per-session
// Candidate/Visible check projectSummary and loadProjectData already run,
// used here to gate the legacy-label ResolveProject branch (see above).
func (s *Service) projectHasVisibleSession(ctx context.Context, projectHash schema.ProjectHash) (bool, error) {
	if s.onVisibilityQuery != nil {
		s.onVisibilityQuery()
	}
	sessions, err := s.querySessions(ctx, projectHash)
	if err != nil {
		return false, err
	}
	for _, session := range sessions {
		visible, visibilityErr := s.visibility.Visible(sessionvisibility.Candidate{
			SessionID:   ingest.SessionID(session.id),
			Harness:     defaults.Harness(session.harness),
			GitRemote:   session.gitRemote,
			ProjectName: session.projectName,
			GitBranch:   session.gitBranch,
		})
		if visibilityErr != nil {
			return false, visibilityErr
		}
		if visible {
			return true, nil
		}
	}
	return false, nil
}

func invalidLegacyProjectLabel(project string) bool {
	if strings.TrimSpace(project) == "" {
		return true
	}
	return strings.IndexFunc(project, unicode.IsControl) >= 0
}

// ProjectSummaries builds the home-picker rows: one per store project, with
// session count, recorded coverage, last work, and open-change count.
//
// Coverage reuses the existing aggregation (gitops ListFiles at HEAD +
// recorded edits) — deliberately NO codegraph parsing and NO graph build:
// the picker must stay cheap with many projects. A project whose
// canonical_cwd does not resolve to a git repo falls back to the
// recorded-edit-only coverage mode (as elsewhere) and reports OpenChanges 0.
// Rows are ordered by display name (hash tie-break) for determinism.
// Everything is computed live per request — no caching layer.
func (s *Service) ProjectSummaries(ctx context.Context) (*ProjectSummariesResult, error) {
	projects, err := s.queryProjects(ctx)
	if err != nil {
		return nil, err
	}

	result := &ProjectSummariesResult{
		Projects:  []schema.ProjectSummary{},
		Selection: SelectionState{Active: s.visibility.Active()},
	}
	mode, matcher, err := s.visibility.ProjectionInputs()
	if err != nil {
		return nil, fmt.Errorf("codemap: prepare project-summary visibility projection: %w", err)
	}

	// Materialize every stored project/session before the first matcher call.
	// EffectiveProjects needs the complete operation cohort to prove remote and
	// name multiplicity instead of letting the query order change visibility.
	cohorts := make([]projectSummaryCohort, len(projects))
	totalSessions := 0
	for projectIndex, project := range projects {
		sessions, queryErr := s.querySessions(ctx, project.hash)
		if queryErr != nil {
			return nil, queryErr
		}
		cohorts[projectIndex] = projectSummaryCohort{project: project, sessions: sessions}
		totalSessions += len(sessions)
	}

	projectionCandidates := make([]selectionprojection.ProjectCandidate, 0, totalSessions)
	resolvedPaths := make(map[string]ingest.ClonePath)
	resolvePath := func(raw string) ingest.ClonePath {
		if raw == "" {
			return ""
		}
		if resolved, ok := resolvedPaths[raw]; ok {
			return resolved
		}
		resolved := s.resolveClonePath(raw)
		resolvedPaths[raw] = resolved
		return resolved
	}
	for _, cohort := range cohorts {
		resolvedWorktrees := make([]ingest.ClonePath, len(cohort.sessions))
		var projectClonePath ingest.ClonePath
		if mode == config.SelectionModeSelected {
			for sessionIndex, session := range cohort.sessions {
				resolvedWorktrees[sessionIndex] = resolvePath(session.gitWorktree)
			}
			// A resolved session worktree is the preferred descendant identity. The
			// project cwd is resolved separately and remains the helper's fallback
			// only when that descendant identity is unavailable.
			projectClonePath = resolvePath(cohort.project.cwd)
		}
		// Keep one descendant per candidate. EffectiveProjects still receives
		// the complete operation cohort, and its identity sets deduplicate shared
		// clone paths so sessions never count as clones. The one-descendant shape
		// lets this same pass return the exact visible session IDs while
		// ParentProjectID aggregates them back into one summary row.
		for sessionIndex, session := range cohort.sessions {
			candidate, candidateErr := projectSummaryCandidate(cohort.project, session, resolvedWorktrees[sessionIndex], projectClonePath)
			if candidateErr != nil {
				return nil, candidateErr
			}
			projectionCandidates = append(projectionCandidates, candidate)
		}
	}

	effective := selectionprojection.EffectiveProjects(
		matcher,
		config.SelectionConfig{Mode: mode},
		projectionCandidates,
	)
	visibleSessions, err := effectiveProjectSummarySessions(effective)
	if err != nil {
		return nil, err
	}

	for _, cohort := range cohorts {
		projectID := selectionprojection.ParentProjectID(cohort.project.hash.String())
		visible := visibleProjectSessions(cohort.sessions, visibleSessions[projectID])
		row, err := s.projectSummary(ctx, cohort.project, visible)
		if err != nil {
			return nil, err
		}
		result.Selection.HiddenSessions += len(cohort.sessions) - len(visible)
		if row == nil {
			if len(cohort.sessions) > 0 {
				result.Selection.HiddenProjects++
			}
			continue
		}
		result.Projects = append(result.Projects, *row)
	}
	sort.SliceStable(result.Projects, func(i, j int) bool {
		a, b := result.Projects[i], result.Projects[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.ProjectHash.String() < b.ProjectHash.String()
	})
	return result, nil
}

type projectSummaryCohort struct {
	project  projectRow
	sessions []sessionRow
}

func (s *Service) resolveClonePath(raw string) ingest.ClonePath {
	if raw == "" || s.pathIdentityResolver == nil {
		return ""
	}
	resolved, err := s.pathIdentityResolver.Resolve(raw)
	if err != nil {
		// Stored paths can become unavailable after ingest. Missing filesystem
		// evidence must not fail the entire viewer list or be recast as identity.
		return ""
	}
	return resolved
}

func projectSummaryCandidate(
	project projectRow,
	session sessionRow,
	sessionClonePath ingest.ClonePath,
	projectClonePath ingest.ClonePath,
) (selectionprojection.ProjectCandidate, error) {
	sessionID, err := ingest.NewSessionID(session.id)
	if err != nil {
		return selectionprojection.ProjectCandidate{}, fmt.Errorf(
			"codemap: prepare project-summary candidate for project %s: stored session ID %q is malformed, so the complete viewer cohort cannot be matched safely; run `peasant ingest verify`, repair the store, and retry: %w",
			project.hash,
			session.id,
			err,
		)
	}
	var parentID ingest.SessionID
	if session.parentID != "" {
		parentID, err = ingest.NewSessionID(session.parentID)
		if err != nil {
			return selectionprojection.ProjectCandidate{}, fmt.Errorf(
				"codemap: prepare project-summary candidate for session %q in project %s: stored parent session ID %q is malformed, so descendant visibility cannot be determined safely; run `peasant ingest verify`, repair the store, and retry: %w",
				session.id,
				project.hash,
				session.parentID,
				err,
			)
		}
	}

	return selectionprojection.ProjectCandidate{
		ParentProjectID: selectionprojection.ParentProjectID(project.hash.String()),
		Harness:         defaults.Harness(session.harness),
		GitRemote:       session.gitRemote,
		ProjectName:     session.projectName,
		ClonePath:       projectClonePath,
		Descendants: []selectionprojection.SessionCandidate{{
			SessionID:       sessionID,
			Branch:          session.gitBranch,
			ParentSessionID: parentID,
			ClonePath:       sessionClonePath,
		}},
	}, nil
}

func effectiveProjectSummarySessions(effective []selectionprojection.EffectiveProject) (map[selectionprojection.ParentProjectID]map[string]struct{}, error) {
	visible := make(map[selectionprojection.ParentProjectID]map[string]struct{})
	for _, project := range effective {
		if len(project.Candidate.Descendants) != 1 {
			return nil, fmt.Errorf(
				"codemap: collect project-summary visibility for parent %q: EffectiveProjects returned %d descendants for a one-session viewer candidate, so exact hidden-session counts cannot be produced safely; update Peasant and retry",
				project.Candidate.ParentProjectID,
				len(project.Candidate.Descendants),
			)
		}
		projectSessions := visible[project.Candidate.ParentProjectID]
		if projectSessions == nil {
			projectSessions = make(map[string]struct{})
			visible[project.Candidate.ParentProjectID] = projectSessions
		}
		projectSessions[project.Candidate.Descendants[0].SessionID.String()] = struct{}{}
	}
	return visible, nil
}

func visibleProjectSessions(sessions []sessionRow, visible map[string]struct{}) []sessionRow {
	result := make([]sessionRow, 0, len(visible))
	for _, session := range sessions {
		if _, ok := visible[session.id]; ok {
			result = append(result, session)
		}
	}
	return result
}

// projectSummary measures one project's already-projected visible sessions:
// recorded activity from the store, coverage against the tracked files at
// HEAD, and the open-branch count. ProjectSummaries supplies this slice from
// the same EffectiveProjects pass that computes hidden counts.
func (s *Service) projectSummary(ctx context.Context, p projectRow, sessions []sessionRow) (*schema.ProjectSummary, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	tasks, err := s.loadTasks(ctx, p.cwd, sessions)
	if err != nil {
		return nil, err
	}
	stats := computeFileStats(tasks)

	fallback := p.cwd
	if fallback == "" {
		fallback = p.hash.String() // same display fallback as the sessions list
	}
	name := projectlabel.Label(p.gitRemote, fallback)
	row := &schema.ProjectSummary{
		ProjectHash: p.hash,
		Project:     name,
		Sessions:    len(sessions),
		LastWorkMs:  lastWorkMs(sessions),
	}

	// Repo resolution mirrors assemble: a failing ListFiles at HEAD means no
	// usable repo — coverage degrades to recorded-edit-only, OpenChanges 0.
	var repo gitops.Repository
	var trackedFiles []string
	repoFound := false
	if p.cwd != "" {
		repo = s.repoFor(p.cwd)
		if files, listErr := repo.ListFiles(ctx, headRef); listErr == nil {
			repoFound = true
			trackedFiles = files
		}
	}

	cov := computeCoverage(repoFound, trackedFiles, stats)
	row.TotalFiles = len(cov.universe)
	row.RecordedFiles = len(cov.recorded)

	if repoFound {
		row.OpenChanges = countOpenChanges(ctx, repo)
	}
	return row, nil
}

// lastWorkMs is the most recent session activity: max end_ms over the
// project's sessions (start_ms when a session has no end). nil when nothing
// carries a timestamp.
func lastWorkMs(sessions []sessionRow) *int64 {
	var last *int64
	for _, sess := range sessions {
		ms := sess.endMs
		if ms == 0 {
			ms = sess.startMs
		}
		if ms > 0 && (last == nil || ms > *last) {
			v := ms
			last = &v
		}
	}
	return last
}

// countOpenChanges counts the local non-default branches not merged into the
// default branch (Branches already excludes the default branch; a
// fully-merged branch keeps its local ref, so the merged set is subtracted).
// One or two git calls; any failure degrades to 0 — a picker row must stay
// renderable on a broken repo.
func countOpenChanges(ctx context.Context, repo gitops.Repository) int {
	branches, err := repo.Branches(ctx)
	if err != nil {
		return 0
	}
	mergedSet := make(map[string]bool)
	if merged, mergedErr := repo.MergedBranches(ctx, 0); mergedErr == nil {
		for _, m := range merged {
			mergedSet[m.Name] = true
		}
	}
	open := 0
	for _, b := range branches {
		if !mergedSet[b] {
			open++
		}
	}
	return open
}
