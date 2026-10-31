package codemap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/schema"
)

// ResolveProject resolves a single explicit display identity without returning
// sibling project names. This preserves stable deep links, and it resolves any
// stored project: the saved kickstart selection does not hide an ingested
// project from the viewer.
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
		return nil, fmt.Errorf("%w: legacy project label %q does not identify a stored project; choose a current project or use its canonical hash URL", ErrProjectNotFound, project)
	case 1:
		return &schema.ProjectResolutionPayload{Project: project, ProjectHash: matches[0].hash}, nil
	default:
		return nil, fmt.Errorf("%w: legacy project label %q matches %d stored projects; use the canonical project hash URL to choose one", ErrProjectAmbiguous, project, len(matches))
	}
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
// Every stored project and every stored session in it is listed. The saved
// kickstart selection is not applied: it decides what Peasant ingests next, not
// which already-ingested projects the viewer shows.
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
		Selection: SelectionState{},
	}

	for _, project := range projects {
		sessions, queryErr := s.querySessions(ctx, project.hash)
		if queryErr != nil {
			return nil, queryErr
		}
		row, summaryErr := s.projectSummary(ctx, project, sessions)
		if summaryErr != nil {
			return nil, summaryErr
		}
		if row == nil {
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

// projectSummary measures one project's recorded activity from the store,
// coverage against the tracked files at HEAD, and the open-branch count.
func (s *Service) projectSummary(ctx context.Context, p projectRow, sessions []sessionRow) (*schema.ProjectSummary, error) {
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
