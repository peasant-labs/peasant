package codemap_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// Binding scenarios layered on the activity fixture by the tests below:
//
//	session 1 — commit fxHashA in range + edits internal/a/a.go  => bound
//	session 2 — no commits, edits internal/a/a.go (touch arm)    => candidate
//	session 3 — commit fxHashB in range, no recorded edits       => candidate
//	session 4 — git_branch == feat/x only                        => candidate
//	fxHashC   — in range, linked to no session                   => unrecorded

// TestReviewChanges_BindingAndFacts covers the list payload: the binding
// rule per session class, task counts over changed files, structure deltas,
// merged rows, and the default-branch commit strip.
func TestReviewChanges_BindingAndFacts(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	base := fxBase()
	seedCommit(t, s, fxSession1, fxHashA, base+3500)
	seedSession(t, s, fxSession3, "", base+2400, base+2600)
	seedCommit(t, s, fxSession3, fxHashB, base+2500)
	seedSession(t, s, fxSession4, fxBranch, base+2700, base+2900)

	payload, err := svc.ReviewChanges(context.Background(), fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}

	if !payload.RepoFound {
		t.Fatal("RepoFound = false, want true")
	}
	if payload.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", payload.DefaultBranch)
	}

	// One open change + one merged row, open first.
	if len(payload.Changes) != 2 {
		t.Fatalf("len(Changes) = %d, want 2: %+v", len(payload.Changes), payload.Changes)
	}
	open := payload.Changes[0]
	if open.Branch != fxBranch || open.Merged {
		t.Fatalf("Changes[0] = %+v, want open %s", open, fxBranch)
	}
	if open.AheadCount != 3 || open.FilesChanged != 2 {
		t.Errorf("ahead/files = %d/%d, want 3/2", open.AheadCount, open.FilesChanged)
	}
	// Sessions: 1 bound + 3 candidates (touch, commit, branch arms).
	if open.SessionCount != 4 {
		t.Errorf("SessionCount = %d, want 4", open.SessionCount)
	}
	// Tasks overlapping the changed files: session1@0, session1@4, session2@0.
	if open.TaskCount != 3 {
		t.Errorf("TaskCount = %d, want 3", open.TaskCount)
	}
	// Structure delta: feat/x adds the a -> c import edge.
	if open.NewEdges != 1 || open.RemovedEdges != 0 || open.Violations != 0 {
		t.Errorf("structure delta = +%d/-%d viol %d, want +1/-0 viol 0",
			open.NewEdges, open.RemovedEdges, open.Violations)
	}
	// Last work: the latest end_ms among associated sessions (session 1).
	if open.LastWorkMs == nil || *open.LastWorkMs != base+4000 {
		t.Errorf("LastWorkMs = %v, want %d", open.LastWorkMs, base+4000)
	}
	// Graph anchors of the open row: the fork anchor is the merge-base, the
	// row position is the tip committer time (fxHashC, the newest branch
	// commit); the join anchor stays empty on open rows.
	if open.BaseHash != fxMergeBase {
		t.Errorf("open BaseHash = %q, want %q", open.BaseHash, fxMergeBase)
	}
	if open.TipCommitMs == nil || *open.TipCommitMs != base+5000 {
		t.Errorf("open TipCommitMs = %v, want %d", open.TipCommitMs, base+5000)
	}
	if open.MergeCommitHash != "" {
		t.Errorf("open MergeCommitHash = %q, want empty", open.MergeCommitHash)
	}

	merged := payload.Changes[1]
	if merged.Branch != fxMergedBranch || !merged.Merged {
		t.Errorf("Changes[1] = %+v, want merged %s", merged, fxMergedBranch)
	}
	if merged.MergedAtMs == nil || *merged.MergedAtMs != base+9000 {
		t.Errorf("MergedAtMs = %v, want %d", merged.MergedAtMs, base+9000)
	}
	// Graph anchors of the merged row: the join anchor is the merge commit;
	// fork anchor and tip time stay empty when merged facts are unavailable.
	if merged.MergeCommitHash != fxHashM {
		t.Errorf("merged MergeCommitHash = %q, want %q", merged.MergeCommitHash, fxHashM)
	}
	if merged.BaseHash != "" || merged.TipCommitMs != nil {
		t.Errorf("merged BaseHash/TipCommitMs = %q/%v, want empty/nil", merged.BaseHash, merged.TipCommitMs)
	}

	// Time strip: default-branch commits flagged by recorded linkage.
	if len(payload.RecentCommits) != 1 {
		t.Fatalf("len(RecentCommits) = %d, want 1", len(payload.RecentCommits))
	}
	if payload.RecentCommits[0].Hash != fxMergeBase || payload.RecentCommits[0].HasSession {
		t.Errorf("RecentCommits[0] = %+v, want unrecorded %s", payload.RecentCommits[0], fxMergeBase)
	}

	assertNoNullArrays(t, payload)
}

// TestReviewChanges_RepoMissing: graceful degrade — RepoFound=false, empty
// lists, no error.
func TestReviewChanges_RepoMissing(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, noRepo())

	payload, err := svc.ReviewChanges(context.Background(), fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}
	if payload.RepoFound {
		t.Error("RepoFound = true, want false")
	}
	if len(payload.Changes) != 0 || len(payload.RecentCommits) != 0 {
		t.Errorf("changes/commits = %d/%d, want 0/0", len(payload.Changes), len(payload.RecentCommits))
	}
	assertNoNullArrays(t, payload)
}

// TestReviewChanges_TipCommitTimeDegrades: a failing branch log never fails
// the list — the open row keeps its fork anchor (merge-base) but its tip
// time degrades to nil, and the lane-0 strip degrades to empty.
func TestReviewChanges_TipCommitTimeDegrades(t *testing.T) {
	t.Parallel()
	repo := fxStubRepo()
	repo.CommitsErr = errors.New("git log failed")
	svc, _ := newFixtureService(t, repo)

	payload, err := svc.ReviewChanges(context.Background(), fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}
	if len(payload.Changes) != 2 {
		t.Fatalf("len(Changes) = %d, want 2", len(payload.Changes))
	}
	open := payload.Changes[0]
	if open.Branch != fxBranch || open.Merged {
		t.Fatalf("Changes[0] = %+v, want open %s", open, fxBranch)
	}
	if open.TipCommitMs != nil {
		t.Errorf("TipCommitMs = %v, want nil when the branch log fails", open.TipCommitMs)
	}
	if open.BaseHash != fxMergeBase {
		t.Errorf("BaseHash = %q, want %q (independent of the branch log)", open.BaseHash, fxMergeBase)
	}
	if len(payload.RecentCommits) != 0 {
		t.Errorf("RecentCommits = %+v, want empty when the default-branch log fails", payload.RecentCommits)
	}
	assertNoNullArrays(t, payload)
}

// TestReviewChanges_Deterministic: same inputs => DeepEqual payloads,
// including the graph anchor fields.
func TestReviewChanges_Deterministic(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	seedCommit(t, s, fxSession1, fxHashA, fxBase()+3500)

	first, err := svc.ReviewChanges(context.Background(), fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges #1: %v", err)
	}
	second, err := svc.ReviewChanges(context.Background(), fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges #2: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("ReviewChanges not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}
}

// TestChangeDetail_WorkAndFootnotes covers the binding classification on the
// work rail, per-session task scoping, unrecorded commits, and the footnote
// sums (output tokens and cost over BOUND sessions only).
func TestChangeDetail_WorkAndFootnotes(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	base := fxBase()
	seedCommit(t, s, fxSession1, fxHashA, base+3500)
	seedSession(t, s, fxSession3, "", base+2400, base+2600)
	seedCommit(t, s, fxSession3, fxHashB, base+2500)
	seedMetrics(t, s, fxSession3, "commit-only session", schema.OutcomePartial, 250, 0, ptrFloat(9.0))
	seedSession(t, s, fxSession4, fxBranch, base+2700, base+2900)

	payload, err := svc.ChangeDetail(context.Background(), fxProjectHash, fxBranch)
	if err != nil {
		t.Fatalf("ChangeDetail: %v", err)
	}

	if payload.BaseRef != fxMergeBase {
		t.Errorf("BaseRef = %q, want %q", payload.BaseRef, fxMergeBase)
	}
	if payload.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q", payload.DefaultBranch)
	}

	// Changed files pass through with status codes.
	if len(payload.Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2", len(payload.Files))
	}
	if payload.Files[0].Path != "internal/a/a.go" || payload.Files[0].Status != "M" {
		t.Errorf("Files[0] = %+v", payload.Files[0])
	}
	if payload.Files[1].Path != "internal/c/c.go" || payload.Files[1].Status != "A" {
		t.Errorf("Files[1] = %+v", payload.Files[1])
	}

	// Per-file churn joins from gitops DiffStats.PerFile (5.3 treemap sizing):
	// the fixture seeds a.go +4/-3 and c.go +8/-0; the per-file sums equal the
	// payload totals (+12/-3).
	byPath := map[string]schema.FileChange{}
	for _, f := range payload.Files {
		byPath[f.Path] = f
	}
	if a := byPath["internal/a/a.go"]; a.LinesAdded != 4 || a.LinesRemoved != 3 {
		t.Errorf("internal/a/a.go churn = +%d/-%d, want +4/-3", a.LinesAdded, a.LinesRemoved)
	}
	if c := byPath["internal/c/c.go"]; c.LinesAdded != 8 || c.LinesRemoved != 0 {
		t.Errorf("internal/c/c.go churn = +%d/-%d, want +8/-0", c.LinesAdded, c.LinesRemoved)
	}

	// Work: bound first, then candidates; candidates never dropped.
	if len(payload.Work) != 4 {
		t.Fatalf("len(Work) = %d, want 4: %+v", len(payload.Work), payload.Work)
	}
	bindings := map[string]schema.ChangeBinding{}
	for _, w := range payload.Work {
		bindings[w.SessionID] = w.Binding
	}
	want := map[string]schema.ChangeBinding{
		fxSession1: schema.ChangeBindingBound,     // commit + touch
		fxSession2: schema.ChangeBindingCandidate, // touch only
		fxSession3: schema.ChangeBindingCandidate, // commit only
		fxSession4: schema.ChangeBindingCandidate, // git_branch only
	}
	if !reflect.DeepEqual(bindings, want) {
		t.Errorf("bindings = %v, want %v", bindings, want)
	}
	if payload.Work[0].SessionID != fxSession1 {
		t.Errorf("Work[0] = %s, want the bound session first", payload.Work[0].SessionID)
	}
	if payload.Work[0].Title != "Session one title" {
		t.Errorf("Work[0].Title = %q", payload.Work[0].Title)
	}
	if payload.Work[0].Harness == "" {
		t.Error("Work[0].Harness is empty")
	}
	// Session 1's tasks on the rail: both edit a changed file.
	if len(payload.Work[0].Tasks) != 2 {
		t.Errorf("Work[0] tasks = %d, want 2", len(payload.Work[0].Tasks))
	}
	// Session 4 (branch-arm candidate) has no overlapping tasks.
	for _, w := range payload.Work {
		if w.SessionID == fxSession4 && len(w.Tasks) != 0 {
			t.Errorf("session4 tasks = %d, want 0", len(w.Tasks))
		}
	}

	// Unrecorded commits: fxHashC is ahead of the merge-base and unlinked.
	if len(payload.UnrecordedCommits) != 1 {
		t.Fatalf("UnrecordedCommits = %+v, want [%s]", payload.UnrecordedCommits, fxHashC)
	}
	uc := payload.UnrecordedCommits[0]
	if uc.Hash != fxHashC || uc.HasSession || uc.Subject != "unrecorded tweak" {
		t.Errorf("UnrecordedCommits[0] = %+v", uc)
	}

	// Footnotes: sums over BOUND sessions only — session 3's 250 tokens and
	// $9.00 (candidate) must not leak in.
	if payload.OutputTokens != 1000 {
		t.Errorf("OutputTokens = %d, want 1000", payload.OutputTokens)
	}
	if payload.CostUsd == nil || *payload.CostUsd != 1.5 {
		t.Errorf("CostUsd = %v, want 1.5", payload.CostUsd)
	}
	// Line counts come from gitops DiffStats against the merge-base.
	if payload.LinesAdded != 12 || payload.LinesRemoved != 3 {
		t.Errorf("lines = +%d/-%d, want +12/-3", payload.LinesAdded, payload.LinesRemoved)
	}

	// No friction clusters here: the bound session (1) has no retry-loop task,
	// and session 2's retry loop is candidate-only (bound-only rule). Never nil.
	if len(payload.Frictions) != 0 {
		t.Errorf("Frictions = %+v, want empty (no bound retry loops)", payload.Frictions)
	}

	assertNoNullArrays(t, payload)
}

// TestChangeDetail_FrictionClusters covers the recurring-friction rollup: a
// retry-loop task on a BOUND session, keyed to a changed file,
// becomes one neutral cluster — and the candidate session's identical retry
// activity is excluded (bound-only, like every other change-detail footnote).
func TestChangeDetail_FrictionClusters(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	base := fxBase()
	// Make session 1 bound (commit arm; the touch arm holds via its edits).
	seedCommit(t, s, fxSession1, fxHashA, base+3500)
	// Re-seed session 1 as a single retry-loop task editing a CHANGED file
	// (internal/a/a.go). Two consecutive depth-0 assistant errors => retryLoop.
	seedEntries(t, s, fxSession1, []entrySpec{
		userTurn(base+3000, "fix the failing a test"),
		assistantTurn(base+3100, false),
		toolUse(base+3200, "Edit", fxCwd+"/internal/a/a.go"),
		assistantTurn(base+3300, true),
		assistantTurn(base+3400, true),
	})

	payload, err := svc.ChangeDetail(context.Background(), fxProjectHash, fxBranch)
	if err != nil {
		t.Fatalf("ChangeDetail: %v", err)
	}

	if len(payload.Frictions) != 1 {
		t.Fatalf("Frictions = %d, want 1 (one bound retry-loop file): %+v",
			len(payload.Frictions), payload.Frictions)
	}
	fc := payload.Frictions[0]
	want := schema.FrictionCluster{
		Kind: "retryLoop", Label: "retry loops",
		File: "internal/a/a.go", Count: 1, Sessions: 1,
	}
	// Count==1/Sessions==1 (not 2) proves session 2's candidate retry loop on
	// the same file was excluded by the bound-only rule.
	if fc != want {
		t.Errorf("Frictions[0] = %+v, want %+v", fc, want)
	}
	assertNoNullArrays(t, payload)
}

// TestChangeDetail_SliceAndDelta covers the structure delta payload and the
// changed slice: touched nodes + ancestors + one-hop neighbors, with edges
// filtered to the slice.
func TestChangeDetail_SliceAndDelta(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.ChangeDetail(context.Background(), fxProjectHash, fxBranch)
	if err != nil {
		t.Fatalf("ChangeDetail: %v", err)
	}

	// Delta: the new a -> c edge and the new internal/c nodes.
	if len(payload.NewEdges) != 1 || payload.NewEdges[0].From != "internal/a" || payload.NewEdges[0].To != "internal/c" {
		t.Errorf("NewEdges = %+v, want internal/a -> internal/c", payload.NewEdges)
	}
	if len(payload.RemovedEdges) != 0 {
		t.Errorf("RemovedEdges = %+v, want none", payload.RemovedEdges)
	}
	wantNew := []string{"internal/c", "internal/c/c.go"}
	if !reflect.DeepEqual(payload.NewNodes, wantNew) {
		t.Errorf("NewNodes = %v, want %v", payload.NewNodes, wantNew)
	}
	if len(payload.RemovedNodes) != 0 {
		t.Errorf("RemovedNodes = %v, want none", payload.RemovedNodes)
	}
	if len(payload.Violations) != 0 {
		t.Errorf("Violations = %+v, want none", payload.Violations)
	}

	// Slice: changed file nodes + ancestors + 1-hop (internal/b via the
	// a -> b edge). b's file node is NOT included (only the package is a
	// neighbor).
	sliceIDs := []string{}
	for _, n := range payload.Slice.Nodes {
		sliceIDs = append(sliceIDs, n.ID)
	}
	wantSlice := []string{"internal", "internal/a", "internal/a/a.go", "internal/b", "internal/c", "internal/c/c.go"}
	if !reflect.DeepEqual(sliceIDs, wantSlice) {
		t.Errorf("Slice nodes = %v, want %v", sliceIDs, wantSlice)
	}
	// Both head structure edges survive the slice filter.
	if len(payload.Slice.StructureEdges) != 2 {
		t.Errorf("Slice.StructureEdges = %+v, want 2", payload.Slice.StructureEdges)
	}
	// The activity edge between internal/a and internal/b is in the slice.
	if len(payload.Slice.ActivityEdges) != 1 {
		t.Errorf("Slice.ActivityEdges = %+v, want 1", payload.Slice.ActivityEdges)
	}
}

// TestReviewChanges_MergedBranchDedup: a fully-merged branch keeps its local
// ref and shows up in the branch listing (ahead 0 / behind N) — it must
// appear in the Review list exactly once, as the merged:true row. The
// default branch never appears as an open change either, even when the
// listing includes it.
func TestReviewChanges_MergedBranchDedup(t *testing.T) {
	t.Parallel()
	repo := fxStubRepo()
	repo.BranchList = []string{fxBranch, fxMergedBranch, testutil.TestDefaultBranch}
	repo.BranchStates[fxMergedBranch] = &gitops.BranchState{
		Name:        fxMergedBranch,
		MergeBase:   fxMergeBase,
		AheadCount:  0,
		BehindCount: 218,
	}
	repo.BranchStates[testutil.TestDefaultBranch] = &gitops.BranchState{
		Name:      testutil.TestDefaultBranch,
		MergeBase: fxMergeBase,
	}
	svc, _ := newFixtureService(t, repo)

	payload, err := svc.ReviewChanges(context.Background(), fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}

	rowsByBranch := map[string][]schema.ChangeSummary{}
	for _, c := range payload.Changes {
		rowsByBranch[c.Branch] = append(rowsByBranch[c.Branch], c)
	}
	if len(payload.Changes) != 2 {
		t.Fatalf("len(Changes) = %d, want 2 (one open + one merged): %+v", len(payload.Changes), payload.Changes)
	}
	mergedRows := rowsByBranch[fxMergedBranch]
	if len(mergedRows) != 1 || !mergedRows[0].Merged {
		t.Errorf("rows for %s = %+v, want exactly one merged:true row", fxMergedBranch, mergedRows)
	}
	if rows := rowsByBranch[testutil.TestDefaultBranch]; len(rows) != 0 {
		t.Errorf("default branch %s has %d rows, want none", testutil.TestDefaultBranch, len(rows))
	}
	if rows := rowsByBranch[fxBranch]; len(rows) != 1 || rows[0].Merged {
		t.Errorf("rows for %s = %+v, want exactly one open row", fxBranch, rows)
	}
}

// TestChangeDetail_RemovedNodesInSlice: a branch that deletes a package must
// surface the deleted nodes in the changed slice (with their BASE
// layer/order) so the 'removed' delta state can render, with the
// removed import edge's endpoints both present in the slice.
func TestChangeDetail_RemovedNodesInSlice(t *testing.T) {
	t.Parallel()
	const (
		delBranch     = "feat/del-z"
		delBase       = "aaaa444444444444444444444444444444444444"
		fileAImportsZ = "package a\n\nimport (\n\t_ \"example.com/proj/internal/b\"\n\t_ \"example.com/proj/internal/z\"\n)\n"
		fileZ         = "package z\n"
	)

	// Merge-base tree: the HEAD tree plus internal/z (imported by a). The
	// branch head matches HEAD — internal/z deleted, a's import dropped.
	repo := fxStubRepo()
	repo.BranchList = append(repo.BranchList, delBranch)
	repo.BranchStates[delBranch] = &gitops.BranchState{
		Name:       delBranch,
		MergeBase:  delBase,
		AheadCount: 1,
		ChangedFiles: []gitops.FileChange{
			{Path: "internal/a/a.go", Status: gitops.FileStatusModified},
			{Path: "internal/z/z.go", Status: gitops.FileStatusDeleted},
		},
	}
	repo.FilesByRef[delBase] = []string{"go.mod", "internal/a/a.go", "internal/b/b.go", "internal/z/z.go", "docs/readme.md"}
	repo.FilesByRef[delBranch] = []string{"go.mod", "internal/a/a.go", "internal/b/b.go", "docs/readme.md"}
	repo.FileContents[delBase+":go.mod"] = []byte(fxGoMod)
	repo.FileContents[delBase+":internal/a/a.go"] = []byte(fileAImportsZ)
	repo.FileContents[delBase+":internal/b/b.go"] = []byte(fxFileB)
	repo.FileContents[delBase+":internal/z/z.go"] = []byte(fileZ)
	repo.FileContents[delBranch+":go.mod"] = []byte(fxGoMod)
	repo.FileContents[delBranch+":internal/a/a.go"] = []byte(fxFileA)
	repo.FileContents[delBranch+":internal/b/b.go"] = []byte(fxFileB)
	svc, _ := newFixtureService(t, repo)

	payload, err := svc.ChangeDetail(context.Background(), fxProjectHash, delBranch)
	if err != nil {
		t.Fatalf("ChangeDetail: %v", err)
	}

	// Delta: the z package + file are removed, along with a's import edge.
	wantRemoved := []string{"internal/z", "internal/z/z.go"}
	if !reflect.DeepEqual(payload.RemovedNodes, wantRemoved) {
		t.Errorf("RemovedNodes = %v, want %v", payload.RemovedNodes, wantRemoved)
	}
	if len(payload.RemovedEdges) != 1 || payload.RemovedEdges[0].From != "internal/a" || payload.RemovedEdges[0].To != "internal/z" {
		t.Errorf("RemovedEdges = %+v, want internal/a -> internal/z", payload.RemovedEdges)
	}

	// The removed nodes render in the slice with their base attributes —
	// both endpoints of the removed edge are present, so the dashed edge has
	// visible evidence.
	zPkg := findNode(t, payload.Slice.Nodes, "internal/z")
	if zPkg.Kind != schema.MapNodeKindPackage {
		t.Errorf("internal/z Kind = %q, want %q", zPkg.Kind, schema.MapNodeKindPackage)
	}
	zFile := findNode(t, payload.Slice.Nodes, "internal/z/z.go")
	if zFile.Kind != schema.MapNodeKindFile {
		t.Errorf("internal/z/z.go Kind = %q, want %q", zFile.Kind, schema.MapNodeKindFile)
	}
	if zFile.Loc != 1 {
		t.Errorf("internal/z/z.go Loc = %d, want 1 (base attributes preserved)", zFile.Loc)
	}
	findNode(t, payload.Slice.Nodes, "internal/a")
	// Base layer preserved: at the merge-base, z is imported only by a —
	// exactly like b at head — so it keeps b's layer, not a default 0.
	bPkg := findNode(t, payload.Slice.Nodes, "internal/b")
	if zPkg.Layer != bPkg.Layer {
		t.Errorf("internal/z Layer = %d, want %d (base layer preserved)", zPkg.Layer, bPkg.Layer)
	}

	assertNoNullArrays(t, payload)
}

// TestChangeDetail_RepoMissing: change detail requires a repo.
func TestChangeDetail_RepoMissing(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, noRepo())

	_, err := svc.ChangeDetail(context.Background(), fxProjectHash, fxBranch)
	if !errors.Is(err, codemap.ErrRepoNotFound) {
		t.Errorf("ChangeDetail error = %v, want ErrRepoNotFound", err)
	}
}

// TestChangeDetail_UnknownBranch: a branch that is neither the default nor a
// local branch is an honest not-found, not a generic failure.
func TestChangeDetail_UnknownBranch(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	_, err := svc.ChangeDetail(context.Background(), fxProjectHash, "no/such-branch")
	if !errors.Is(err, codemap.ErrBranchNotFound) {
		t.Errorf("ChangeDetail error = %v, want ErrBranchNotFound", err)
	}
}

// TestChangeDetail_BranchStateFailure_KnownBranch: a git failure on a branch
// that DOES exist must not masquerade as not-found.
func TestChangeDetail_BranchStateFailure_KnownBranch(t *testing.T) {
	t.Parallel()
	repo := fxStubRepo()
	delete(repo.BranchStates, fxBranch) // listed in BranchList, state read fails
	svc, _ := newFixtureService(t, repo)

	_, err := svc.ChangeDetail(context.Background(), fxProjectHash, fxBranch)
	if err == nil || errors.Is(err, codemap.ErrBranchNotFound) {
		t.Errorf("ChangeDetail error = %v, want a non-ErrBranchNotFound failure", err)
	}
}

// countingBuilder wraps a codegraph.Builder and counts Build invocations —
// the observable contract of the SHA-keyed graph memo.
type countingBuilder struct {
	inner codegraph.Builder
	calls atomic.Int32
}

func (b *countingBuilder) Build(ctx context.Context, files []string, read codegraph.FileReader) (*codegraph.Graph, error) {
	b.calls.Add(1)
	return b.inner.Build(ctx, files, read)
}

// TestReviewChanges_StructureDeltaCapAndMemo seeds 25 open branches sharing
// one merge-base. It asserts the perf contract of the list endpoint:
//
//   - only the maxStructureDeltaBranches (20) most recently active branches
//     (by tip commit time) get structure-delta columns; the 5 oldest keep
//     zero-valued columns;
//   - graph builds are memoized by resolved commit SHA, so the shared
//     merge-base graph builds once: 1 base + 20 head = 21 builds total.
func TestReviewChanges_StructureDeltaCapAndMemo(t *testing.T) {
	t.Parallel()
	const branchCount = 25
	base := fxBase()

	repo := fxStubRepo()
	repo.BranchList = nil
	repo.BranchStates = map[string]*gitops.BranchState{}
	for i := 0; i < branchCount; i++ {
		name := fmt.Sprintf("feat/b%02d", i)
		tip := fmt.Sprintf("bbbb%036d", i) // full 40-hex tip SHA per branch
		repo.BranchList = append(repo.BranchList, name)
		repo.BranchStates[name] = &gitops.BranchState{
			Name:      name,
			MergeBase: fxMergeBase,
			ChangedFiles: []gitops.FileChange{
				{Path: "internal/a/a.go", Status: gitops.FileStatusModified},
				{Path: "internal/c/c.go", Status: gitops.FileStatusAdded},
			},
		}
		// Branch content mirrors feat/x: a new internal/c plus the a -> c
		// import, so a computed structure delta shows NewEdges = 1.
		repo.FilesByRef[name] = []string{"go.mod", "internal/a/a.go", "internal/b/b.go", "internal/c/c.go"}
		repo.FileContents[name+":go.mod"] = []byte(fxGoMod)
		repo.FileContents[name+":internal/a/a.go"] = []byte(fxFileAv2)
		repo.FileContents[name+":internal/b/b.go"] = []byte(fxFileB)
		repo.FileContents[name+":internal/c/c.go"] = []byte(fxFileC)
		// Tip times ascend with the index: b05..b24 are the 20 most recent.
		repo.CommitsByRef[name] = []gitops.Commit{
			{Hash: tip, Subject: "tip of " + name, TimeMs: base + int64(i+1)*1000},
		}
	}

	s := storetest.Open(t)
	seedActivityFixture(t, s)
	builder := &countingBuilder{inner: codegraph.NewGraphBuilder()}
	svc := codemap.NewService(s, func(string) gitops.Repository { return repo }, builder, sessionvisibility.All())

	payload, err := svc.ReviewChanges(context.Background(), fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}

	withDelta, withoutDelta := 0, 0
	for _, c := range payload.Changes {
		if c.Merged {
			continue
		}
		idx := 0
		if _, scanErr := fmt.Sscanf(c.Branch, "feat/b%d", &idx); scanErr != nil {
			t.Fatalf("unexpected open branch %q", c.Branch)
		}
		if c.NewEdges > 0 {
			withDelta++
			if idx < branchCount-20 {
				t.Errorf("branch %s (older than the top-20 cohort) has structure columns", c.Branch)
			}
		} else {
			withoutDelta++
			if idx >= branchCount-20 {
				t.Errorf("branch %s (in the top-20 cohort) is missing structure columns", c.Branch)
			}
		}
	}
	if withDelta != 20 || withoutDelta != branchCount-20 {
		t.Errorf("delta cohort = %d with / %d without, want 20 / %d", withDelta, withoutDelta, branchCount-20)
	}

	// Memo: the shared merge-base graph builds once, each cohort head once.
	if got := builder.calls.Load(); got != 21 {
		t.Errorf("codegraph builds = %d, want 21 (1 memoized base + 20 heads)", got)
	}
}

// TestChangeDetail_Deterministic: same inputs => DeepEqual payloads.
func TestChangeDetail_Deterministic(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	seedCommit(t, s, fxSession1, fxHashA, fxBase()+3500)

	first, err := svc.ChangeDetail(context.Background(), fxProjectHash, fxBranch)
	if err != nil {
		t.Fatalf("ChangeDetail #1: %v", err)
	}
	second, err := svc.ChangeDetail(context.Background(), fxProjectHash, fxBranch)
	if err != nil {
		t.Fatalf("ChangeDetail #2: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("ChangeDetail not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}
}
