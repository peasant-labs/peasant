package codemap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/projectlabel"
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
	for _, p := range projects {
		row, total, visible, err := s.projectSummary(ctx, p)
		if err != nil {
			return nil, err
		}
		result.Selection.HiddenSessions += total - visible
		if row == nil {
			if total > 0 {
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

// projectSummary measures one project: recorded activity from the store,
// coverage against the tracked files at HEAD, and the open-branch count.
// The two int returns are (totalSessions, visibleSessions) BEFORE and AFTER
// selection filtering — the same pass ProjectSummaries uses to compute
// SelectionState, so those counts can never drift from what the row (or its
// absence) already shows.
func (s *Service) projectSummary(ctx context.Context, p projectRow) (*schema.ProjectSummary, int, int, error) {
	sessions, err := s.querySessions(ctx, p.hash)
	if err != nil {
		return nil, 0, 0, err
	}
	totalCount := len(sessions)
	visibleSessions := make([]sessionRow, 0, len(sessions))
	for _, session := range sessions {
		visible, visibilityErr := s.visibility.Visible(sessionvisibility.Candidate{
			SessionID:   ingest.SessionID(session.id),
			Harness:     defaults.Harness(session.harness),
			GitRemote:   session.gitRemote,
			ProjectName: session.projectName,
			GitBranch:   session.gitBranch,
		})
		if visibilityErr != nil {
			return nil, 0, 0, visibilityErr
		}
		if visible {
			visibleSessions = append(visibleSessions, session)
		}
	}
	sessions = visibleSessions
	visibleCount := len(sessions)
	if len(sessions) == 0 {
		return nil, totalCount, visibleCount, nil
	}
	tasks, err := s.loadTasks(ctx, p.cwd, sessions)
	if err != nil {
		return nil, 0, 0, err
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
	return row, totalCount, visibleCount, nil
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
