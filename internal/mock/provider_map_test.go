package mock

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/schema"
)

const testProjectHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestProvider_MapGraph_MockShape(t *testing.T) {
	p := NewProvider()
	graph, err := p.MapGraph(context.Background(), testProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph: %v", err)
	}

	if graph.ProjectHash != testProjectHash {
		t.Errorf("ProjectHash = %q, want echo of request", graph.ProjectHash)
	}
	if !graph.RepoFound {
		t.Error("RepoFound = false, want true")
	}
	if len(graph.ParsedLanguages) == 0 {
		t.Error("ParsedLanguages is empty")
	}

	// 3 modules + 8 packages.
	var modules, packages int
	for _, n := range graph.Nodes {
		if !n.Kind.IsValid() {
			t.Errorf("node %s: invalid kind %q", n.ID, n.Kind)
		}
		switch n.Kind {
		case schema.MapNodeKindModule:
			modules++
		case schema.MapNodeKindPackage:
			packages++
		}
	}
	if modules != 3 {
		t.Errorf("modules = %d, want 3", modules)
	}
	if packages != 8 {
		t.Errorf("packages = %d, want 8", packages)
	}

	// Edges reference existing nodes; one cycle violation present.
	ids := make(map[string]bool, len(graph.Nodes))
	for _, n := range graph.Nodes {
		ids[n.ID] = true
	}
	for _, e := range graph.StructureEdges {
		if !ids[e.From] || !ids[e.To] {
			t.Errorf("structure edge %s -> %s references unknown node", e.From, e.To)
		}
	}
	for _, e := range graph.ActivityEdges {
		if e.TaskCount < 2 {
			t.Errorf("activity edge %s -> %s below floor: %d", e.From, e.To, e.TaskCount)
		}
	}
	if len(graph.Violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(graph.Violations))
	}
	if v := graph.Violations[0]; v.Kind != schema.EdgeViolationCycle || !v.Kind.IsValid() {
		t.Errorf("violation kind = %q, want cycle", v.Kind)
	}
	if graph.GeneratedAtMs == 0 {
		t.Error("GeneratedAtMs is zero")
	}
}

// TestProvider_ProjectSummaries_MockShape: two picker rows ordered by name —
// the fortuna repo project (coherent with the map graph's module roll-up and
// the review list's open changes) and one repo-less project (recorded-edit-
// only coverage, zero open changes).
func TestProvider_ProjectSummaries_MockShape(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	payload, err := p.ProjectSummaries(ctx)
	if err != nil {
		t.Fatalf("ProjectSummaries: %v", err)
	}
	if payload.Projects == nil {
		t.Fatal("Projects is nil, want empty-or-populated array")
	}
	if len(payload.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(payload.Projects))
	}

	// Ordered by project name: docs-site < fortuna.
	repoLess, fortuna := payload.Projects[0], payload.Projects[1]
	if repoLess.Project != mockRepoLessProject || fortuna.Project != mockRepoProject {
		t.Fatalf("project order = [%q, %q], want [%q, %q]",
			repoLess.Project, fortuna.Project, mockRepoLessProject, mockRepoProject)
	}

	// Hashes round-trip through the same derivation as SessionSummaries.
	for _, row := range payload.Projects {
		if row.ProjectHash != mockProjectHash(row.Project) {
			t.Errorf("%s: ProjectHash %q != mockProjectHash(%q)", row.Project, row.ProjectHash, row.Project)
		}
		if row.LastWorkMs == nil {
			t.Errorf("%s: LastWorkMs is nil", row.Project)
		}
	}

	// fortuna: coverage equals the map graph's module roll-up; open changes
	// equal the review list's open rows.
	graph, err := p.MapGraph(ctx, mockProjectHash(mockRepoProject), "")
	if err != nil {
		t.Fatalf("MapGraph: %v", err)
	}
	var wantRecorded, wantTotal int
	for _, n := range graph.Nodes {
		if n.Kind == schema.MapNodeKindModule {
			wantRecorded += n.RecordedFiles
			wantTotal += n.TotalFiles
		}
	}
	if fortuna.RecordedFiles != wantRecorded || fortuna.TotalFiles != wantTotal {
		t.Errorf("fortuna coverage = %d/%d, want %d/%d (map module roll-up)",
			fortuna.RecordedFiles, fortuna.TotalFiles, wantRecorded, wantTotal)
	}
	list, err := p.ReviewChanges(ctx, mockProjectHash(mockRepoProject))
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}
	wantOpen := 0
	for _, c := range list.Changes {
		if !c.Merged {
			wantOpen++
		}
	}
	if fortuna.OpenChanges != wantOpen {
		t.Errorf("fortuna OpenChanges = %d, want %d (review open rows)", fortuna.OpenChanges, wantOpen)
	}
	if fortuna.Sessions == 0 {
		t.Error("fortuna Sessions = 0, want > 0")
	}

	// Repo-less project: recorded-edit-only mode (recorded == total), no
	// open changes, sessions coherent with the mock session list.
	if repoLess.RecordedFiles != repoLess.TotalFiles || repoLess.TotalFiles == 0 {
		t.Errorf("repo-less coverage = %d/%d, want equal and > 0", repoLess.RecordedFiles, repoLess.TotalFiles)
	}
	if repoLess.OpenChanges != 0 {
		t.Errorf("repo-less OpenChanges = %d, want 0", repoLess.OpenChanges)
	}
	summaries, err := p.SessionSummaries(ctx)
	if err != nil {
		t.Fatalf("SessionSummaries: %v", err)
	}
	wantSessions := 0
	for _, s := range summaries {
		if s.Project == mockRepoLessProject {
			wantSessions++
		}
	}
	if repoLess.Sessions != wantSessions {
		t.Errorf("repo-less Sessions = %d, want %d (mock session list)", repoLess.Sessions, wantSessions)
	}
}

func TestProvider_MapNodeDetail_KnownNode(t *testing.T) {
	p := NewProvider()
	detail, err := p.MapNodeDetail(context.Background(), testProjectHash, "internal/api")
	if err != nil {
		t.Fatalf("MapNodeDetail: %v", err)
	}
	if detail.Path != "internal/api" {
		t.Errorf("Path = %q, want internal/api", detail.Path)
	}
	if !detail.Kind.IsValid() {
		t.Errorf("invalid kind %q", detail.Kind)
	}
	if len(detail.ShapedBy) == 0 {
		t.Error("ShapedBy is empty")
	}
	for _, task := range detail.ShapedBy {
		if task.SessionID == "" || task.Title == "" {
			t.Errorf("task %+v missing sessionID/title", task)
		}
	}
	if len(detail.RecentCommits) == 0 {
		t.Error("RecentCommits is empty")
	}
	if detail.SessionCount == 0 || detail.TaskCount == 0 {
		t.Errorf("SessionCount/TaskCount = %d/%d, want > 0", detail.SessionCount, detail.TaskCount)
	}
}

func TestProvider_ProjectTasks_FilterAndCount(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	all, err := p.ProjectTasks(ctx, testProjectHash, "")
	if err != nil {
		t.Fatalf("ProjectTasks: %v", err)
	}
	if len(all.Tasks) != 15 {
		t.Errorf("tasks = %d, want 15", len(all.Tasks))
	}
	for _, task := range all.Tasks {
		if task.Title == "" || task.SessionID == "" {
			t.Errorf("task %+v missing title/sessionID", task)
		}
		if task.EditedFiles == nil || task.Labels == nil {
			t.Errorf("task %q has nil slices", task.Title)
		}
	}

	// Directory filter restricts to tasks editing under that path.
	filtered, err := p.ProjectTasks(ctx, testProjectHash, "internal/queue")
	if err != nil {
		t.Fatalf("ProjectTasks(filtered): %v", err)
	}
	if len(filtered.Tasks) == 0 || len(filtered.Tasks) >= len(all.Tasks) {
		t.Errorf("filtered tasks = %d, want 0 < n < %d", len(filtered.Tasks), len(all.Tasks))
	}
	if filtered.FileFilter != "internal/queue" {
		t.Errorf("FileFilter = %q", filtered.FileFilter)
	}
}

func TestProvider_ReviewChanges_TwoOpenOneMerged(t *testing.T) {
	p := NewProvider()
	list, err := p.ReviewChanges(context.Background(), testProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}
	if !list.RepoFound || list.DefaultBranch == "" {
		t.Errorf("RepoFound/DefaultBranch = %v/%q", list.RepoFound, list.DefaultBranch)
	}

	var open, merged int
	for _, c := range list.Changes {
		if c.Merged {
			merged++
			if c.MergedAtMs == nil {
				t.Errorf("merged change %q has nil MergedAtMs", c.Branch)
			}
		} else {
			open++
		}
	}
	if open != 2 || merged != 1 {
		t.Errorf("open/merged = %d/%d, want 2/1", open, merged)
	}
	if len(list.RecentCommits) == 0 {
		t.Error("RecentCommits is empty")
	}
}

func TestProvider_StrikeFixtureSpansMountedWebSurfaces(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()
	fixture := canonicalStrikeMockFixture

	detail, err := p.SessionByID(ctx, fixture.SessionDetail.ID)
	if err != nil {
		t.Fatalf("SessionByID(%s): %v", fixture.SessionDetail.ID, err)
	}
	if detail.Harness != schema.HarnessStrike || detail.Project != fixture.Project.Name || len(detail.Turns) != fixture.SessionDetail.TurnCount {
		t.Fatalf("Strike session detail = harness %q project %q turns %d, want %q/%q/%d", detail.Harness, detail.Project, len(detail.Turns), schema.HarnessStrike, fixture.Project.Name, fixture.SessionDetail.TurnCount)
	}
	if detail.Turns[1].Content != fixture.Expected.AssistantContent {
		t.Errorf("Strike assistant content = %q, want %q", detail.Turns[1].Content, fixture.Expected.AssistantContent)
	}

	summaries, err := p.SessionSummaries(ctx)
	if err != nil {
		t.Fatalf("SessionSummaries: %v", err)
	}
	foundSummary := false
	for _, summary := range summaries {
		if summary.ID != fixture.SessionDetail.ID {
			continue
		}
		foundSummary = true
		if summary.Harness != schema.HarnessStrike || summary.ProjectHash.String() != fixture.Project.Hash || summary.Preview != fixture.Expected.MapConversationTitle {
			t.Errorf("Strike Map summary = harness %q hash %q preview %q", summary.Harness, summary.ProjectHash, summary.Preview)
		}
	}
	if !foundSummary {
		t.Fatalf("SessionSummaries omitted shared Strike fixture %s", fixture.SessionDetail.ID)
	}

	node, err := p.MapNodeDetail(ctx, mockProjectHash(fixture.Project.Name), "internal")
	if err != nil {
		t.Fatalf("MapNodeDetail: %v", err)
	}
	if len(node.RecentCommits) == 0 || node.RecentCommits[0].Hash != fixture.MapCommit.Hash || len(node.RecentCommits[0].SessionIDs) == 0 || node.RecentCommits[0].SessionIDs[0].String() != fixture.SessionDetail.ID {
		t.Fatalf("selected Map node did not bind shared Strike commit: %+v", node.RecentCommits)
	}

	review, err := p.ReviewChanges(ctx, mockProjectHash(fixture.Project.Name))
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}
	foundReview := false
	for _, session := range review.Sessions {
		if session.SessionID.String() != fixture.SessionDetail.ID {
			continue
		}
		foundReview = true
		if session.Harness != schema.HarnessStrike || session.Title != fixture.Expected.ReviewSessionTitle || !session.HasCommitBinding {
			t.Errorf("Strike Review session = harness %q title %q bound %v", session.Harness, session.Title, session.HasCommitBinding)
		}
	}
	if !foundReview {
		t.Fatalf("ReviewChanges omitted shared Strike fixture %s", fixture.SessionDetail.ID)
	}
}

// TestProvider_ReviewChanges_GraphAnchorCoherence pins the Changes-graph
// contract of the mock: lane 0 (recentCommits) is a newest-first strip with
// a session mix, every open row forks at a listed commit and sits at a tip
// time newer than its fork, the branches fork at distinct points
// (feat/queue-retry mid-list, fix/api-timeout earlier), and the merged row
// rejoins at a listed merge commit whose time is the row's MergedAtMs.
func TestProvider_ReviewChanges_GraphAnchorCoherence(t *testing.T) {
	p := NewProvider()
	list, err := p.ReviewChanges(context.Background(), testProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}

	// Lane 0: ~12 commits, newest first, with recorded and unrecorded mix.
	if len(list.RecentCommits) != 12 {
		t.Fatalf("len(RecentCommits) = %d, want 12", len(list.RecentCommits))
	}
	laneIdx := make(map[string]int, len(list.RecentCommits))
	laneTime := make(map[string]int64, len(list.RecentCommits))
	withSession := 0
	for i, c := range list.RecentCommits {
		if c.Hash == "" || c.Subject == "" || c.TimeMs == nil {
			t.Errorf("RecentCommits[%d] = %+v, want hash/subject/time", i, c)
			continue
		}
		if i > 0 && list.RecentCommits[i-1].TimeMs != nil && *list.RecentCommits[i-1].TimeMs < *c.TimeMs {
			t.Errorf("RecentCommits not newest-first at %d: %d < %d", i, *list.RecentCommits[i-1].TimeMs, *c.TimeMs)
		}
		laneIdx[c.Hash] = i
		laneTime[c.Hash] = *c.TimeMs
		if c.HasSession {
			withSession++
		}
	}
	if withSession == 0 || withSession == len(list.RecentCommits) {
		t.Errorf("hasSession count = %d of %d, want a mix", withSession, len(list.RecentCommits))
	}

	for _, row := range list.Changes {
		if row.Merged {
			// Join anchor: the merge commit is ON the strip, and the row's
			// merge time IS that commit's time.
			idx, listed := laneIdx[row.MergeCommitHash]
			if !listed {
				t.Errorf("%s: MergeCommitHash %q not in recentCommits", row.Branch, row.MergeCommitHash)
				continue
			}
			if row.MergedAtMs == nil || *row.MergedAtMs != laneTime[row.MergeCommitHash] {
				t.Errorf("%s: MergedAtMs = %v, want the listed merge commit's time %d",
					row.Branch, row.MergedAtMs, laneTime[row.MergeCommitHash])
			}
			if idx == 0 || idx == len(list.RecentCommits)-1 {
				t.Errorf("%s: merge commit at lane index %d, want an interior join", row.Branch, idx)
			}
			continue
		}
		// Fork anchor: the merge-base is ON the strip; the tip sits after it.
		if _, listed := laneIdx[row.BaseHash]; !listed {
			t.Errorf("%s: BaseHash %q not in recentCommits", row.Branch, row.BaseHash)
			continue
		}
		if row.TipCommitMs == nil {
			t.Errorf("%s: TipCommitMs is nil", row.Branch)
			continue
		}
		if *row.TipCommitMs <= laneTime[row.BaseHash] {
			t.Errorf("%s: tip %d not newer than fork commit %d", row.Branch, *row.TipCommitMs, laneTime[row.BaseHash])
		}
	}

	// Distinct fork shapes: feat/queue-retry forks mid-list, fix/api-timeout
	// forks earlier (deeper in the strip).
	rowByBranch := make(map[string]schema.ChangeSummary, len(list.Changes))
	for _, row := range list.Changes {
		rowByBranch[row.Branch] = row
	}
	cycleIdx, timeoutIdx := laneIdx[rowByBranch[mockBranchCycle].BaseHash], laneIdx[rowByBranch[mockBranchNoDelta].BaseHash]
	if cycleIdx == 0 || cycleIdx >= len(list.RecentCommits)-1 {
		t.Errorf("%s forks at lane index %d, want mid-list", mockBranchCycle, cycleIdx)
	}
	if timeoutIdx <= cycleIdx {
		t.Errorf("%s forks at %d, want earlier (deeper) than %s at %d",
			mockBranchNoDelta, timeoutIdx, mockBranchCycle, cycleIdx)
	}
}

func TestProvider_ChangeDetail_BoundAndCandidate(t *testing.T) {
	p := NewProvider()
	detail, err := p.ChangeDetail(context.Background(), testProjectHash, mockBranchCycle)
	if err != nil {
		t.Fatalf("ChangeDetail: %v", err)
	}
	if detail.Branch != mockBranchCycle {
		t.Errorf("Branch = %q", detail.Branch)
	}
	if len(detail.Files) == 0 || len(detail.Slice.Nodes) == 0 {
		t.Errorf("files/slice nodes = %d/%d, want > 0", len(detail.Files), len(detail.Slice.Nodes))
	}

	var bound, candidate int
	for _, w := range detail.Work {
		if !w.Binding.IsValid() {
			t.Errorf("session %s: invalid binding %q", w.SessionID, w.Binding)
		}
		switch w.Binding {
		case schema.ChangeBindingBound:
			bound++
		case schema.ChangeBindingCandidate:
			candidate++
		}
		if len(w.Tasks) == 0 {
			t.Errorf("session %s has no tasks", w.SessionID)
		}
	}
	if bound != 1 || candidate != 1 {
		t.Errorf("bound/candidate = %d/%d, want 1/1", bound, candidate)
	}
	if len(detail.Violations) != 1 {
		t.Errorf("violations = %d, want 1", len(detail.Violations))
	}
	if len(detail.UnrecordedCommits) == 0 {
		t.Error("UnrecordedCommits is empty")
	}
}

// TestProvider_ChangeDetail_CoherentWithList: the detail payloads must agree
// with the list rows on caption-grade facts — file counts, session counts,
// task counts, structure-delta columns — so a mock demo never contradicts
// itself between list and detail.
func TestProvider_ChangeDetail_CoherentWithList(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	list, err := p.ReviewChanges(ctx, testProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}
	if len(list.Changes) == 0 {
		t.Fatal("no list rows")
	}

	for _, row := range list.Changes {
		detail, err := p.ChangeDetail(ctx, testProjectHash, row.Branch)
		if err != nil {
			t.Fatalf("ChangeDetail(%s): %v", row.Branch, err)
		}
		if detail.Branch != row.Branch {
			t.Errorf("%s: detail Branch = %q", row.Branch, detail.Branch)
		}
		if row.Merged {
			// Minimal merged detail: identity only, like the list row.
			if len(detail.Files)+len(detail.Work)+len(detail.NewEdges)+len(detail.RemovedEdges)+
				len(detail.NewNodes)+len(detail.RemovedNodes)+len(detail.Violations) != 0 {
				t.Errorf("%s: merged detail is not minimal: %+v", row.Branch, detail)
			}
			continue
		}
		if detail.BaseRef != row.BaseHash {
			t.Errorf("%s: detail BaseRef = %q, list baseHash = %q", row.Branch, detail.BaseRef, row.BaseHash)
		}
		if got := len(detail.Files); got != row.FilesChanged {
			t.Errorf("%s: detail files = %d, list filesChanged = %d", row.Branch, got, row.FilesChanged)
		}
		if got := len(detail.Work); got != row.SessionCount {
			t.Errorf("%s: detail sessions = %d, list sessionCount = %d", row.Branch, got, row.SessionCount)
		}
		tasks := 0
		for _, w := range detail.Work {
			tasks += len(w.Tasks)
		}
		if tasks != row.TaskCount {
			t.Errorf("%s: detail tasks = %d, list taskCount = %d", row.Branch, tasks, row.TaskCount)
		}
		if got := len(detail.NewEdges); got != row.NewEdges {
			t.Errorf("%s: detail newEdges = %d, list newEdges = %d", row.Branch, got, row.NewEdges)
		}
		if got := len(detail.RemovedEdges); got != row.RemovedEdges {
			t.Errorf("%s: detail removedEdges = %d, list removedEdges = %d", row.Branch, got, row.RemovedEdges)
		}
		if got := len(detail.Violations); got != row.Violations {
			t.Errorf("%s: detail violations = %d, list violations = %d", row.Branch, got, row.Violations)
		}
	}

	// The no-changes branch drills into a genuinely zero-delta detail.
	noDelta, err := p.ChangeDetail(ctx, testProjectHash, mockBranchNoDelta)
	if err != nil {
		t.Fatalf("ChangeDetail(%s): %v", mockBranchNoDelta, err)
	}
	if len(noDelta.NewEdges)+len(noDelta.RemovedEdges)+len(noDelta.NewNodes)+
		len(noDelta.RemovedNodes)+len(noDelta.Violations) != 0 {
		t.Errorf("%s: structure delta is not zero: %+v", mockBranchNoDelta, noDelta)
	}
	if len(noDelta.Slice.Nodes) == 0 {
		t.Errorf("%s: slice is empty", mockBranchNoDelta)
	}
}

// TestProvider_ChangeDetail_UnknownBranch: an unknown branch takes the same
// not-found sentinel path as the real provider (API maps it to 404).
func TestProvider_ChangeDetail_UnknownBranch(t *testing.T) {
	p := NewProvider()
	_, err := p.ChangeDetail(context.Background(), testProjectHash, "no/such-branch")
	if !errors.Is(err, codemap.ErrBranchNotFound) {
		t.Errorf("ChangeDetail error = %v, want ErrBranchNotFound", err)
	}
}

func TestProvider_SessionSummaries_CarryProjectHash(t *testing.T) {
	p := NewProvider()
	summaries, err := p.SessionSummaries(context.Background())
	if err != nil {
		t.Fatalf("SessionSummaries: %v", err)
	}
	if len(summaries) == 0 {
		t.Fatal("no mock summaries")
	}

	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	byProject := make(map[string]string)
	for _, s := range summaries {
		hash := s.ProjectHash.String()
		if !hexRe.MatchString(hash) {
			t.Errorf("session %s: ProjectHash %q is not 64-hex", s.ID, s.ProjectHash)
		}
		// Deterministic per project name.
		if prev, ok := byProject[s.Project]; ok && prev != hash {
			t.Errorf("project %q maps to two hashes: %q vs %q", s.Project, prev, s.ProjectHash)
		}
		byProject[s.Project] = hash
		if s.ProjectHash != mockProjectHash(s.Project) {
			t.Errorf("session %s: hash mismatch with mockProjectHash(%q)", s.ID, s.Project)
		}
	}
}
