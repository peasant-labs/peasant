package codemap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/schema"
)

// sessionBinding is the classification of one session against one change
// according to the binding rule:
//
//	bound     — >=1 linked commit is contained in the branch (ahead of the
//	            merge-base) AND the session's recorded edits intersect the
//	            branch's changed files;
//	candidate — exactly one arm matches, or only git_branch equality;
//	(absent)  — no arm matches.
//
// Candidates are never silently dropped.
type sessionBinding struct {
	sessionID string
	binding   schema.ChangeBinding
}

// classifySessions applies the binding rule to every project session.
// Results are in the sessions' (newest-first) order, bound before candidate.
func classifySessions(pd *projectData, branch string, rangeHashes map[string]bool, changedSet map[string]bool) []sessionBinding {
	var bound, candidates []sessionBinding
	for _, sess := range pd.sessions {
		commitArm := false
		for _, c := range pd.commitsByID[sess.id] {
			if rangeHashes[c.hash] {
				commitArm = true
				break
			}
		}
		touchArm := false
		for _, f := range pd.editedByID[sess.id] {
			if changedSet[f] {
				touchArm = true
				break
			}
		}
		branchArm := sess.gitBranch != "" && sess.gitBranch == branch

		switch {
		case commitArm && touchArm:
			bound = append(bound, sessionBinding{sess.id, schema.ChangeBindingBound})
		case commitArm || touchArm || branchArm:
			candidates = append(candidates, sessionBinding{sess.id, schema.ChangeBindingCandidate})
		}
	}
	return append(bound, candidates...)
}

// resolveRepo returns the project's repository and default branch.
// ok=false means the canonical_cwd did not resolve to a usable git repo.
func (s *Service) resolveRepo(ctx context.Context, pd *projectData) (gitops.Repository, string, bool) {
	if pd.cwd == "" {
		return nil, "", false
	}
	repo := s.repoFor(pd.cwd)
	defaultBranch, err := repo.DefaultBranch(ctx)
	if err != nil {
		return nil, "", false
	}
	return repo, defaultBranch, true
}

// ReviewChanges lists the project's changes: open local branches measured
// against the default branch, then recently merged branches. A project without a
// resolvable Git repository returns RepoFound=false while preserving its session
// and rewrite-ledger rows.
func (s *Service) ReviewChanges(ctx context.Context, projectHash schema.ProjectHash) (*schema.ReviewListPayload, error) {
	pd, err := s.loadProjectData(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	payload := schema.NewReviewListPayload(projectHash)
	payload.Sessions = append(payload.Sessions, timelineSessionRefs(pd)...)

	repo, defaultBranch, ok := s.resolveRepo(ctx, pd)
	if !ok {
		payload.RewrittenCommits = rewriteLedger(pd, nil)
		return validateReviewTimeline(payload, projectHash) // Git is unavailable; unattached sessions remain discoverable.
	}
	payload.RepoFound = true
	payload.DefaultBranch = defaultBranch

	branches, err := repo.Branches(ctx)
	if err != nil {
		return nil, fmt.Errorf("codemap: branches: %w", err)
	}
	sort.Strings(branches)

	// Merged branches (resolved before the open pass: a fully-merged branch
	// keeps its local ref, so it would otherwise ALSO appear as an open row
	// with ahead 0 / behind N — the merged:true row wins; see the dedup in
	// pass 1). Structure and work facts for merged branches need merge-base
	// historical diffing; the rows carry identity +
	// merge time only.
	merged, mergedErr := repo.MergedBranches(ctx, maxMergedBranch)
	mergedSet := make(map[string]bool, len(merged))
	if mergedErr == nil {
		for _, m := range merged {
			mergedSet[m.Name] = true
		}
	}

	// Pass 1: activity facts for every open branch (cheap git + store reads),
	// plus the tip commit time used to pick the structure-delta cohort.
	// Branches in the merged set — and the default branch, should the
	// listing include it — are not open changes.
	type openRow struct {
		summary *schema.ChangeSummary
		state   *gitops.BranchState
		tipMs   int64
	}
	rows := make([]openRow, 0, len(branches))
	for _, branch := range branches {
		if branch == defaultBranch || mergedSet[branch] {
			continue
		}
		summary, st := s.changeSummary(ctx, pd, repo, branch)
		if summary == nil {
			continue // branch state unmeasurable (e.g. deleted ref); skipped
		}
		row := openRow{summary: summary, state: st}
		if commits, cErr := repo.Commits(ctx, branch, 1); cErr == nil && len(commits) > 0 {
			row.tipMs = commits[0].TimeMs
			// Graph anchor: the tip committer time positions the branch row
			// on the Changes graph. Best-effort — a failing or empty log
			// leaves TipCommitMs nil, never fatal.
			if commits[0].TimeMs > 0 {
				ms := commits[0].TimeMs
				summary.TipCommitMs = &ms
			}
		}
		rows = append(rows, row)
	}

	// Pass 2: structure deltas (two codegraph builds per branch — the
	// dominant cost of this endpoint) only for the maxStructureDeltaBranches
	// most recently active branches by tip commit time; the rest keep zero
	// structure columns. Graph builds are memoized by resolved commit SHA in
	// buildGraphAt, so the merge-base graph shared by most branches is built
	// once, not once per branch.
	cohort := make([]int, len(rows))
	for i := range rows {
		cohort[i] = i
	}
	sort.SliceStable(cohort, func(a, b int) bool {
		ra, rb := rows[cohort[a]], rows[cohort[b]]
		if ra.tipMs != rb.tipMs {
			return ra.tipMs > rb.tipMs
		}
		return ra.summary.Branch < rb.summary.Branch
	})
	if len(cohort) > maxStructureDeltaBranches {
		cohort = cohort[:maxStructureDeltaBranches]
	}
	for _, i := range cohort {
		row := rows[i]
		diff, diffErr := s.structureDiff(ctx, repo, pd.cwd, row.state.MergeBase, row.summary.Branch)
		if diffErr != nil {
			return nil, diffErr
		}
		row.summary.NewEdges = len(diff.AddedEdges)
		row.summary.RemovedEdges = len(diff.RemovedEdges)
		row.summary.Violations = len(diff.NewViolations)
	}

	open := make([]schema.ChangeSummary, 0, len(rows))
	for _, row := range rows {
		open = append(open, *row.summary)
	}
	// Open changes: most recent recorded work first (unknown last), then name.
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i], open[j]
		switch {
		case a.LastWorkMs != nil && b.LastWorkMs == nil:
			return true
		case a.LastWorkMs == nil && b.LastWorkMs != nil:
			return false
		case a.LastWorkMs != nil && b.LastWorkMs != nil && *a.LastWorkMs != *b.LastWorkMs:
			return *a.LastWorkMs > *b.LastWorkMs
		}
		return a.Branch < b.Branch
	})
	payload.Changes = append(payload.Changes, open...)

	// Merged rows (most recently merged first, capped) follow the open ones.
	// MergeCommitHash is the join anchor on the Changes graph ("" for
	// fast-forward merges, where no merge commit exists).
	if mergedErr == nil {
		// Commits a `git revert` undid on the default branch (best-effort, one
		// bounded call) — used to flag a merged change that was later reverted.
		reverted, _ := repo.RevertedCommits(ctx, defaultBranch)
		for _, m := range merged {
			summary := schema.ChangeSummary{
				Branch:          m.Name,
				Merged:          true,
				MergeCommitHash: m.MergeCommit,
				Reverted:        m.MergeCommit != "" && commitReverted(m.MergeCommit, reverted),
			}
			if m.MergedAtMs > 0 {
				ms := m.MergedAtMs
				summary.MergedAtMs = &ms
			}
			payload.Changes = append(payload.Changes, summary)
		}
	}

	// Default-branch time strip commits, flagged by recorded-session linkage.
	commits, err := repo.Commits(ctx, defaultBranch, maxReviewCommits)
	if err == nil {
		associationsByCommit := timelineCommitAssociationsByCommit(pd, payload.Sessions)
		liveHashes := make(map[string]bool, len(commits))
		for _, c := range commits {
			liveHashes[c.Hash] = true
			payload.RecentCommits = append(payload.RecentCommits, commitRefWithSessions(c, associationsByCommit[c.Hash]))
		}
		payload.RewrittenCommits = rewriteLedger(pd, liveHashes)
	} else {
		// Without a default-branch history we cannot claim an observed hash is
		// live. Preserve it as unresolved rather than silently retargeting it.
		payload.RewrittenCommits = rewriteLedger(pd, nil)
	}

	return validateReviewTimeline(payload, projectHash)
}

// timelineSessionRefs emits each visible project session once, in the same
// newest-first order used throughout the codemap service. Sessions with no
// commit binding are deliberate: the UI keeps them discoverable below the Git
// timeline instead of inventing a temporal association.
func timelineSessionRefs(pd *projectData) []schema.TimelineSessionRef {
	refs := make([]schema.TimelineSessionRef, 0, len(pd.sessions))
	for _, session := range pd.sessions {
		ref := schema.TimelineSessionRef{
			SessionID:        schema.SessionID(session.id),
			Title:            pd.sessionTitle(session.id),
			Harness:          schema.Harness(session.harness),
			HasCommitBinding: len(pd.commitsByID[session.id]) > 0,
		}
		if session.startMs > 0 {
			startMs := session.startMs
			ref.StartMs = &startMs
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		left, right := refs[i], refs[j]
		if left.StartMs == nil || right.StartMs == nil {
			if left.StartMs == nil && right.StartMs == nil {
				return left.SessionID < right.SessionID
			}
			return left.StartMs != nil
		}
		if *left.StartMs == *right.StartMs {
			return left.SessionID < right.SessionID
		}
		return *left.StartMs > *right.StartMs
	})
	return refs
}

// timelineCommitAssociationsByCommit reverses authoritative current
// session_commits rows into a many-to-many commit index. Iterating visible
// sessions first preserves the timeline's stable newest-first session order;
// each row carries the persisted producer-owned ID instead of deriving one at
// response time.
func timelineCommitAssociationsByCommit(pd *projectData, orderedSessions []schema.TimelineSessionRef) map[string][]commitRow {
	byCommit := make(map[string][]commitRow)
	seen := make(map[string]map[schema.SessionID]bool)
	for _, session := range orderedSessions {
		sessionID := session.SessionID
		for _, commit := range pd.commitsByID[string(sessionID)] {
			if seen[commit.hash] == nil {
				seen[commit.hash] = make(map[schema.SessionID]bool)
			}
			if seen[commit.hash][sessionID] {
				continue
			}
			seen[commit.hash][sessionID] = true
			byCommit[commit.hash] = append(byCommit[commit.hash], commit)
		}
	}
	return byCommit
}

func validateReviewTimeline(payload *schema.ReviewListPayload, projectHash schema.ProjectHash) (*schema.ReviewListPayload, error) {
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("codemap: invalid review timeline for project %q: authoritative session_commits data could not be represented during ReviewChanges: %w; the caller must not render this payload; fix the producer so every commit binding names one normalized visible session", projectHash, err)
	}
	return payload, nil
}

// changeSummary measures the activity facts of one open branch (the
// structure-delta columns are filled by the caller's capped second pass).
// Returns (nil, nil) when the branch state cannot be read (the branch is
// skipped, not fatal).
func (s *Service) changeSummary(ctx context.Context, pd *projectData, repo gitops.Repository, branch string) (*schema.ChangeSummary, *gitops.BranchState) {
	st, err := repo.BranchState(ctx, branch)
	if err != nil {
		return nil, nil // unmeasurable branch: skip row
	}

	changedSet := changedFileSet(st.ChangedFiles)
	rangeHashes := commitsInRangeSet(ctx, repo, st.MergeBase, branch)
	bindings := classifySessions(pd, branch, rangeHashes, changedSet)

	summary := &schema.ChangeSummary{
		Branch:       branch,
		AheadCount:   st.AheadCount,
		BehindCount:  st.BehindCount,
		FilesChanged: len(st.ChangedFiles),
		SessionCount: len(bindings),
		BaseHash:     st.MergeBase, // fork anchor on the Changes graph
	}

	var lastWork *int64
	for _, b := range bindings {
		sess := pd.sessionByID[b.sessionID]
		if sess.endMs > 0 && (lastWork == nil || sess.endMs > *lastWork) {
			ms := sess.endMs
			lastWork = &ms
		}
		for _, t := range pd.tasksByID[b.sessionID] {
			if taskOverlapsChange(t, changedSet) {
				summary.TaskCount++
			}
		}
	}
	summary.LastWorkMs = lastWork

	return summary, st
}

// ChangeDetail builds the Review detail payload for one branch. It requires a
// resolvable Git repository (ErrRepoNotFound otherwise); a branch that is neither
// the default branch nor a local branch returns ErrBranchNotFound.
func (s *Service) ChangeDetail(ctx context.Context, projectHash schema.ProjectHash, branch string) (*schema.ChangeDetailPayload, error) {
	pd, err := s.loadProjectData(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	repo, defaultBranch, ok := s.resolveRepo(ctx, pd)
	if !ok {
		return nil, fmt.Errorf("%w: project %s", ErrRepoNotFound, pd.hash)
	}

	st, err := repo.BranchState(ctx, branch)
	if err != nil {
		if !branchKnown(ctx, repo, branch, defaultBranch) {
			return nil, fmt.Errorf("%w: %q", ErrBranchNotFound, branch)
		}
		return nil, fmt.Errorf("codemap: branch state for %q: %w", branch, err)
	}

	payload := schema.NewChangeDetailPayload(branch)
	payload.BaseRef = st.MergeBase
	payload.DefaultBranch = defaultBranch

	// Line-level stats (git diff --numstat against the merge-base), fetched
	// once up front so the per-file churn can be joined into each FileChange
	// (the change-weight treemap's sizing input) and summed for the
	// payload totals. Best-effort: a failing numstat degrades to 0/0 rather
	// than failing the whole detail payload. Renames report at the new path,
	// matching FileChange.Path.
	perFile := map[string]gitops.FileDiffStat{}
	if stats, statsErr := repo.DiffStats(ctx, st.MergeBase, branch); statsErr == nil {
		payload.LinesAdded = stats.LinesAdded
		payload.LinesRemoved = stats.LinesRemoved
		for _, f := range stats.PerFile {
			perFile[f.Path] = f
		}
	}

	for _, fc := range st.ChangedFiles {
		ds := perFile[fc.Path] // zero value (0/0) when absent
		payload.Files = append(payload.Files, schema.FileChange{
			Path:         fc.Path,
			Status:       schema.FileChangeStatus(fc.Status),
			OldPath:      fc.OldPath,
			LinesAdded:   ds.Added,
			LinesRemoved: ds.Removed,
		})
	}

	// Head assembly at the branch tip (the slice's layer/order source) and
	// base graph at the merge-base for the structure delta.
	headAsm, err := s.assemble(ctx, pd, branch)
	if err != nil {
		return nil, err
	}
	baseFiles, err := repo.ListFiles(ctx, st.MergeBase)
	if err != nil {
		return nil, fmt.Errorf("codemap: list files at merge-base %s: %w", st.MergeBase, err)
	}
	baseGraph, err := s.buildGraphAt(ctx, repo, pd.cwd, st.MergeBase, baseFiles)
	if err != nil {
		return nil, err
	}
	diff := codegraph.Diff(baseGraph, headAsm.graph)

	for _, e := range diff.AddedEdges {
		payload.NewEdges = append(payload.NewEdges, schema.MapEdge{From: e.From, To: e.To, Count: e.Count})
	}
	for _, e := range diff.RemovedEdges {
		payload.RemovedEdges = append(payload.RemovedEdges, schema.MapEdge{From: e.From, To: e.To, Count: e.Count})
	}
	for _, n := range diff.AddedNodes {
		payload.NewNodes = append(payload.NewNodes, n.ID)
	}
	for _, n := range diff.RemovedNodes {
		payload.RemovedNodes = append(payload.RemovedNodes, n.ID)
	}
	for _, v := range diff.NewViolations {
		payload.Violations = append(payload.Violations, schema.EdgeViolation{
			Kind: schema.EdgeViolationKind(v.Kind),
			From: v.From,
			To:   v.To,
		})
	}

	changedPaths := make([]string, 0, len(st.ChangedFiles))
	for _, fc := range st.ChangedFiles {
		changedPaths = append(changedPaths, fc.Path)
	}
	payload.Slice = buildSlice(headAsm, diff.RemovedNodes, changedPaths)

	// The work behind the change.
	changedSet := changedFileSet(st.ChangedFiles)
	rangeHashes := commitsInRangeSet(ctx, repo, st.MergeBase, branch)
	bindings := classifySessions(pd, branch, rangeHashes, changedSet)

	costSum := 0.0
	costKnown := false
	for _, b := range bindings {
		sess := pd.sessionByID[b.sessionID]
		cs := schema.NewChangeSession(b.sessionID, b.binding)
		cs.Title = pd.sessionTitle(b.sessionID)
		cs.Harness = sess.harness
		if sess.startMs > 0 {
			ms := sess.startMs
			cs.StartMs = &ms
		}
		var overlapping []taskData
		for _, t := range pd.tasksByID[b.sessionID] {
			if taskOverlapsChange(t, changedSet) {
				overlapping = append(overlapping, t)
			}
		}
		for _, t := range sortTasksReverseChron(overlapping) {
			cs.Tasks = append(cs.Tasks, pd.taskSummary(t))
		}
		payload.Work = append(payload.Work, cs)

		// Footnote sums are over BOUND sessions only. Output
		// tokens summed across the change's sessions; peak-context numbers
		// are never summed).
		if b.binding != schema.ChangeBindingBound {
			continue
		}
		if m, ok := pd.metrics[b.sessionID]; ok {
			if m.outputTokens != nil {
				payload.OutputTokens += int64(*m.outputTokens)
			}
			if m.costTotalUSD != nil {
				costSum += *m.costTotalUSD
				costKnown = true
			}
		}
	}
	if costKnown {
		payload.CostUsd = &costSum
	}

	// What's unusual (neutral rate-elevation vs the project baseline).
	payload.Unusual = unusualSignals(pd, bindings)

	// Recurring friction keyed by file using neutral counts.
	payload.Frictions = frictionClusters(pd, bindings, changedSet)

	// Commits ahead of the merge-base with no recorded session.
	payload.UnrecordedCommits = unrecordedCommits(ctx, pd, repo, branch, rangeHashes)

	return payload, nil
}

// changeDiff builds the rendered unified diff of one changed file of a branch
// (branch vs its merge-base with the default branch) — the lazy per-file
// companion to changeDetail. Requires a resolvable repo (ErrRepoNotFound) and a
// known branch (ErrBranchNotFound); an unknown file yields an empty-hunks
// payload (200), since the caller only requests files the change actually lists.
func (s *Service) changeDiff(ctx context.Context, pd *projectData, branch, file string) (*schema.ChangeDiffPayload, error) {
	repo, defaultBranch, ok := s.resolveRepo(ctx, pd)
	if !ok {
		return nil, fmt.Errorf("%w: project %s", ErrRepoNotFound, pd.hash)
	}

	st, err := repo.BranchState(ctx, branch)
	if err != nil {
		if !branchKnown(ctx, repo, branch, defaultBranch) {
			return nil, fmt.Errorf("%w: %q", ErrBranchNotFound, branch)
		}
		return nil, fmt.Errorf("codemap: branch state for %q: %w", branch, err)
	}

	payload := schema.NewChangeDiffPayload(branch, file)

	// Status/oldPath from the change's file list — covers pure renames and
	// binary files that DiffHunks returns with no textual hunks.
	for i := range st.ChangedFiles {
		if st.ChangedFiles[i].Path == file {
			payload.Status = schema.FileChangeStatus(st.ChangedFiles[i].Status)
			payload.OldPath = st.ChangedFiles[i].OldPath
			break
		}
	}

	var paths []string
	if file != "" {
		paths = []string{file}
	}
	diffs, err := repo.DiffHunks(ctx, st.MergeBase, branch, paths, 0)
	if err != nil {
		return nil, fmt.Errorf("codemap: diff hunks for %q: %w", branch, err)
	}

	// Attribution (the mission climax): git blame the file at the branch tip so
	// each hunk's NEW lines can be traced to the commit that wrote them, then to
	// the recorded conversation. Best-effort — a failing blame just leaves the
	// hunks unattributed. blame[i] is the commit hash for 1-based line i+1.
	blame, _ := repo.BlameCommits(ctx, branch, file)
	commitToSession := commitSessionMap(pd)

	for _, fd := range diffs {
		if file != "" && fd.Path != file {
			continue
		}
		payload.File = fd.Path
		if fd.OldPath != nil {
			payload.OldPath = fd.OldPath
		}
		if payload.Status == "" {
			payload.Status = schema.FileChangeStatus(fd.Status)
		}
		payload.Binary = fd.Binary
		payload.Truncated = fd.Truncated
		for _, h := range fd.Hunks {
			dh := schema.DiffHunk{
				OldStart: h.OldStart,
				OldLines: h.OldLines,
				NewStart: h.NewStart,
				NewLines: h.NewLines,
				Header:   h.Header,
				Lines:    make([]schema.DiffLine, 0, len(h.Lines)),
			}
			for _, l := range h.Lines {
				dh.Lines = append(dh.Lines, schema.DiffLine{Kind: schema.DiffLineKind(l.Kind), Text: l.Text})
			}
			if sid := attributeHunk(h, blame, commitToSession); sid != "" {
				dh.SessionID = sid
				dh.SessionTitle = pd.sessionTitle(sid)
			}
			payload.Hunks = append(payload.Hunks, dh)
		}
		break // single requested file
	}
	return payload, nil
}

// commitSessionMap reverses pd.commitsByID into commit-hash → sessionID, so a
// blamed line's commit resolves to the recorded conversation that produced it.
func commitSessionMap(pd *projectData) map[string]string {
	m := make(map[string]string)
	for sid, commits := range pd.commitsByID {
		for _, c := range commits {
			if _, seen := m[c.hash]; !seen {
				m[c.hash] = sid
			}
		}
	}
	return m
}

// attributeHunk picks the recorded session that wrote most of a hunk's ADDED
// lines: walk the hunk tracking the new-line counter, blame each added line,
// tally commits that map to a recorded session, and return the winner's
// sessionID ("" when none of the added lines trace to a recorded session).
func attributeHunk(h gitops.Hunk, blame []string, commitToSession map[string]string) string {
	counts := make(map[string]int)
	newLine := h.NewStart
	for _, l := range h.Lines {
		switch l.Kind {
		case gitops.DiffLineAdded:
			if newLine-1 >= 0 && newLine-1 < len(blame) {
				if sid, ok := commitToSession[blame[newLine-1]]; ok {
					counts[sid]++
				}
			}
			newLine++
		case gitops.DiffLineContext:
			newLine++
			// removed lines don't advance the new-line counter
		}
	}
	best, bestN := "", 0
	for sid, n := range counts {
		if n > bestN || (n == bestN && sid < best) {
			best, bestN = sid, n
		}
	}
	return best
}

// structureDiff builds the base and head graphs of a branch and diffs them.

// structureDiff builds the base and head graphs of a branch and diffs them.
func (s *Service) structureDiff(ctx context.Context, repo gitops.Repository, repoPath, base, head string) (codegraph.GraphDiff, error) {
	baseFiles, err := repo.ListFiles(ctx, base)
	if err != nil {
		return codegraph.GraphDiff{}, fmt.Errorf("codemap: list files at %s: %w", base, err)
	}
	baseGraph, err := s.buildGraphAt(ctx, repo, repoPath, base, baseFiles)
	if err != nil {
		return codegraph.GraphDiff{}, err
	}
	headFiles, err := repo.ListFiles(ctx, head)
	if err != nil {
		return codegraph.GraphDiff{}, fmt.Errorf("codemap: list files at %s: %w", head, err)
	}
	headGraph, err := s.buildGraphAt(ctx, repo, repoPath, head, headFiles)
	if err != nil {
		return codegraph.GraphDiff{}, err
	}
	return codegraph.Diff(baseGraph, headGraph), nil
}

// branchKnown reports whether branch is the default branch or one of the
// repo's local branches. It distinguishes "unknown branch" (ErrBranchNotFound
// → 404) from a genuine git failure on a real branch (500) when BranchState
// errors. A failing Branches() conservatively reports known, so the original
// BranchState error surfaces instead of a misleading not-found.
func branchKnown(ctx context.Context, repo gitops.Repository, branch, defaultBranch string) bool {
	if branch == defaultBranch {
		return true
	}
	branches, err := repo.Branches(ctx)
	if err != nil {
		return true
	}
	for _, b := range branches {
		if b == branch {
			return true
		}
	}
	return false
}

// buildSlice scopes the head assembly to the changed files: their nodes,
// their ancestors, the one-hop structure neighborhood, and the edges between
// the included nodes. Layer/Order are preserved from the full map so the
// slice is spatially recognizable. removed carries the base
// graph's nodes for diff.RemovedNodes: deleted packages/files no longer
// exist in the head assembly, so they are merged in with their BASE
// layer/order, making the 'removed' delta state renderable.
func buildSlice(asm *assembly, removed []codegraph.Node, changedPaths []string) schema.MapSlice {
	include := make(map[string]bool)
	addWithAncestors := func(id string) {
		if _, ok := asm.nodes[id]; ok {
			include[id] = true
		}
		for d := parentDir(id); d != ""; d = parentDir(d) {
			if _, ok := asm.nodes[d]; ok {
				include[d] = true
			}
		}
	}

	touchedPkgs := make(map[string]bool)
	for _, p := range changedPaths {
		addWithAncestors(p)
		// The touched package: the file's directory when it has a node,
		// otherwise the nearest existing ancestor (covers deleted files).
		for d := parentDir(p); d != ""; d = parentDir(d) {
			if _, ok := asm.nodes[d]; ok {
				touchedPkgs[d] = true
				break
			}
		}
	}

	// One hop over the package-level structure edges of the head graph.
	for _, e := range asm.edges {
		if touchedPkgs[e.From] {
			addWithAncestors(e.To)
		}
		if touchedPkgs[e.To] {
			addWithAncestors(e.From)
		}
	}

	// Removed nodes join the slice with their base layer/order. Their
	// head-surviving ancestors join via addWithAncestors; ancestors deleted
	// with them are themselves in removed (codegraph.Diff lists every base
	// node missing at head). An ID that still exists in the head assembly
	// (e.g. as an activity-only node) keeps the head node.
	removedNodes := make(map[string]*schema.MapNode, len(removed))
	for i := range removed {
		n := &removed[i]
		if _, ok := asm.nodes[n.ID]; ok {
			continue
		}
		removedNodes[n.ID] = mapNodeFromGraph(n)
		addWithAncestors(n.ID)
	}

	merged := make(map[string]*schema.MapNode, len(include)+len(removedNodes))
	for id := range include {
		merged[id] = asm.nodes[id]
	}
	for id, n := range removedNodes {
		merged[id] = n
	}

	slice := schema.NewMapSlice()
	for _, id := range sortedKeys(merged) {
		slice.Nodes = append(slice.Nodes, *merged[id])
	}
	for _, e := range asm.edges {
		if include[e.From] && include[e.To] {
			slice.StructureEdges = append(slice.StructureEdges, e)
		}
	}
	for _, e := range asm.actEdges {
		if include[e.From] && include[e.To] {
			slice.ActivityEdges = append(slice.ActivityEdges, e)
		}
	}
	return slice
}

// unrecordedCommits lists the branch commits ahead of the merge-base with no
// recorded session, newest first. Commit metadata comes from the branch log
// (capped at the range size plus slack for shared history); range hashes
// whose metadata falls outside the cap still appear, hash-only.
func unrecordedCommits(ctx context.Context, pd *projectData, repo gitops.Repository, branch string, rangeHashes map[string]bool) []schema.CommitRef {
	refs := []schema.CommitRef{}
	unrecorded := make(map[string]bool)
	for h := range rangeHashes {
		if !pd.recorded[h] {
			unrecorded[h] = true
		}
	}
	if len(unrecorded) == 0 {
		return refs
	}

	meta := make(map[string]gitops.Commit)
	if commits, err := repo.Commits(ctx, branch, len(rangeHashes)+50); err == nil {
		for _, c := range commits {
			if unrecorded[c.Hash] {
				meta[c.Hash] = c
			}
		}
	}

	rows := make([]gitops.Commit, 0, len(unrecorded))
	for _, h := range sortedKeys(unrecorded) {
		if c, ok := meta[h]; ok {
			rows = append(rows, c)
		} else {
			rows = append(rows, gitops.Commit{Hash: h})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.TimeMs != b.TimeMs {
			if a.TimeMs == 0 || b.TimeMs == 0 {
				return b.TimeMs == 0 // unknown times last
			}
			return a.TimeMs > b.TimeMs
		}
		return a.Hash < b.Hash
	})
	for _, c := range rows {
		refs = append(refs, commitRef(c, false))
	}
	return refs
}

// commitRef converts gitops commit metadata to the wire form.
func commitRef(c gitops.Commit, hasSession bool) schema.CommitRef {
	ref := schema.NewCommitRef(c.Hash, c.Subject)
	ref.HasSession = hasSession
	if c.TimeMs > 0 {
		ms := c.TimeMs
		ref.TimeMs = &ms
	}
	return ref
}

func commitRefWithSessions(c gitops.Commit, bindings []commitRow) schema.CommitRef {
	ref := commitRef(c, len(bindings) > 0)
	for _, binding := range bindings {
		ref.SessionIDs = append(ref.SessionIDs, schema.SessionID(binding.sessionID))
	}
	ref.Associations = recordedCommitAssociations(bindings)
	return ref
}

// recordedCommitAssociations projects persisted association rows into the
// contract's explicit representation. Every observation retains the hash that
// was actually recorded for the session, even if the rewrite ledger later
// renders an unresolved ghost near a different live commit.
func recordedCommitAssociations(bindings []commitRow) []schema.SessionAssociation {
	associations := make([]schema.SessionAssociation, 0, len(bindings))
	for _, binding := range bindings {
		hash := binding.hash
		associations = append(associations, schema.SessionAssociation{
			ID:         binding.associationID,
			SessionID:  schema.SessionID(binding.sessionID),
			Conclusion: schema.AssociationConclusionConfirmed,
			Confidence: schema.ConfidenceHigh,
			Evidence: []schema.AssociationEvidenceObservation{{
				Kind:               schema.AssociationEvidenceRecordedCommit,
				RecordedCommitHash: &hash,
			}},
		})
	}
	return associations
}

// rewriteLedger projects every observed relationship into the schema's
// append-only resolution ledger. This producer intentionally resolves only an
// exact default-branch hash today: an observation absent from the bounded live
// history is explicitly unresolved, never guessed or retargeted to a similar
// commit. A later resolver can add patch/author/temporal matching without
// changing the durable observation or association ID.
func rewriteLedger(pd *projectData, liveHashes map[string]bool) []schema.RewrittenCommit {
	byHash := make(map[string][]commitRow, len(pd.ledger))
	for _, row := range pd.ledger {
		byHash[row.hash] = append(byHash[row.hash], row)
	}
	hashes := sortedKeys(byHash)
	ledger := make([]schema.RewrittenCommit, 0, len(hashes))
	sessionRank := make(map[string]int, len(pd.sessions))
	for index, session := range pd.sessions {
		sessionRank[session.id] = index
	}
	for _, hash := range hashes {
		rows := append([]commitRow(nil), byHash[hash]...)
		sort.SliceStable(rows, func(i, j int) bool {
			left, right := sessionRank[rows[i].sessionID], sessionRank[rows[j].sessionID]
			if left != right {
				return left < right
			}
			return rows[i].sessionID < rows[j].sessionID
		})
		entry := schema.RewrittenCommit{
			GhostHash:    hash,
			Subject:      rows[0].subject,
			AuthorTimeMs: rows[0].timeMs,
			Associations: recordedCommitAssociations(rows),
			Resolution:   schema.RewriteResolutionUnresolved,
			Method:       schema.RewriteMethodNone,
			Confidence:   schema.ConfidenceLow,
		}
		for _, row := range rows {
			entry.SessionIDs = append(entry.SessionIDs, schema.SessionID(row.sessionID))
		}
		if liveHashes != nil && liveHashes[hash] {
			entry.Resolution = schema.RewriteResolutionLive
			entry.Method = schema.RewriteMethodHash
			entry.Confidence = schema.ConfidenceHigh
		}
		ledger = append(ledger, entry)
	}
	return ledger
}

// Thresholds for a retry-loop "unusual" signal — conservative, to avoid noise:
// enough data on both sides, a multiplicative AND an absolute margin.
const (
	unusualMinChangeSessions  = 2
	unusualMinProjectSessions = 3
	unusualRateMultiple       = 1.5
	unusualRateAbsMargin      = 0.5
)

// unusualSignals compares the change's BOUND-session friction (retry loops per
// conversation) to the project baseline and returns neutral rate-elevation
// observations — facts only, never a verdict. Returns an empty slice when there
// isn't enough data or nothing is elevated.
func unusualSignals(pd *projectData, bindings []sessionBinding) []schema.UnusualSignal {
	out := []schema.UnusualSignal{}

	// Project baseline: retry loops per session over all sessions with metrics.
	projRetries, projN := 0, 0
	for _, m := range pd.metrics {
		if m.retryLoops != nil {
			projRetries += *m.retryLoops
			projN++
		}
	}

	// This change: retry loops per bound session with metrics.
	chRetries, chN := 0, 0
	for _, b := range bindings {
		if b.binding != schema.ChangeBindingBound {
			continue
		}
		if m, ok := pd.metrics[b.sessionID]; ok && m.retryLoops != nil {
			chRetries += *m.retryLoops
			chN++
		}
	}

	if chN >= unusualMinChangeSessions && projN >= unusualMinProjectSessions && chRetries > 0 {
		perChange := float64(chRetries) / float64(chN)
		perProject := float64(projRetries) / float64(projN)
		if perChange >= perProject*unusualRateMultiple && perChange >= perProject+unusualRateAbsMargin {
			out = append(out, schema.UnusualSignal{
				Kind:       "retryLoops",
				Label:      "more retry loops per conversation than usual",
				PerChange:  perChange,
				PerProject: perProject,
			})
		}
	}
	return out
}

// frictionClusters groups recurring file-attributable friction across the
// change's BOUND sessions, keyed by (kind, file). v1 has one kind, "retryLoop":
// for every bound-session task whose retryLoop is set, each edited file that is
// also in the change's changed set contributes one occurrence. Counts are
// neutral facts — "N times across M conversations" — never a verdict. Bound-only
// matches every other change-detail footnote (OutputTokens/CostUsd/Unusual).
// Deterministic order (Count desc, then File asc); empty slice when none.
func frictionClusters(pd *projectData, bindings []sessionBinding, changedSet map[string]bool) []schema.FrictionCluster {
	type acc struct {
		count    int
		sessions map[string]bool
	}
	byFile := map[string]*acc{}
	for _, b := range bindings {
		if b.binding != schema.ChangeBindingBound {
			continue
		}
		for _, t := range pd.tasksByID[b.sessionID] {
			if !t.retryLoop {
				continue
			}
			for _, f := range t.editedFiles {
				if !changedSet[f] {
					continue
				}
				a := byFile[f]
				if a == nil {
					a = &acc{sessions: map[string]bool{}}
					byFile[f] = a
				}
				a.count++
				a.sessions[b.sessionID] = true
			}
		}
	}

	out := []schema.FrictionCluster{}
	for f, a := range byFile {
		out = append(out, schema.FrictionCluster{
			Kind:     "retryLoop",
			Label:    "retry loops",
			File:     f,
			Count:    a.count,
			Sessions: len(a.sessions),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].File < out[j].File
	})
	return out
}

// commitReverted reports whether fullHash (a merge commit) appears in the
// reverted set, tolerating abbreviation on either side (git's revert trailer
// may write a short hash). Empty hashes never match.
func commitReverted(fullHash string, reverted map[string]bool) bool {
	if fullHash == "" || len(reverted) == 0 {
		return false
	}
	if reverted[fullHash] {
		return true
	}
	for h := range reverted {
		if h != "" && (strings.HasPrefix(fullHash, h) || strings.HasPrefix(h, fullHash)) {
			return true
		}
	}
	return false
}

// changedFileSet indexes a branch's changed files (new paths; renames count
// under the new path, matching the recorded-edit grain).
func changedFileSet(changes []gitops.FileChange) map[string]bool {
	set := make(map[string]bool, len(changes))
	for _, fc := range changes {
		set[fc.Path] = true
	}
	return set
}

// commitsInRangeSet indexes the commit hashes ahead of the merge-base.
// Errors degrade to an empty set: the commit arm of the binding rule simply
// cannot match, touch-only sessions still classify as candidates.
func commitsInRangeSet(ctx context.Context, repo gitops.Repository, base, head string) map[string]bool {
	set := make(map[string]bool)
	hashes, err := repo.CommitsInRange(ctx, base, head)
	if err != nil {
		return set
	}
	for _, h := range hashes {
		set[h] = true
	}
	return set
}

// taskOverlapsChange reports whether a task edited any of the changed files.
func taskOverlapsChange(t taskData, changedSet map[string]bool) bool {
	for _, f := range t.editedFiles {
		if changedSet[f] {
			return true
		}
	}
	return false
}
