package mock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/schema"
)

// Branch names served by the Review mock. List and detail must agree on
// caption-grade facts: the cycle branch carries the rich slice, the
// no-changes branch a zero-delta detail, the merged branch a minimal
// identity-only detail. Unknown branches take the same not-found path as
// the real provider (codemap.ErrBranchNotFound).
const (
	mockBranchCycle   = "feat/queue-retry"
	mockBranchNoDelta = "fix/api-timeout"
	mockBranchMerged  = "feat/ingest-batching"
	mockDefaultBranch = "main"
)

// Changes-graph anchor hashes. Every anchor MUST appear in
// mockRecentCommits so the graph can attach forks and joins to lane 0
// (the default-branch commit strip): feat/queue-retry forks mid-list,
// fix/api-timeout forks earlier, feat/ingest-batching forked at the oldest
// listed commit and rejoins at its merge commit.
const (
	mockForkCycleHash   = "9f8e7d6c5b4a3210" // feat/queue-retry merge-base (mid-list)
	mockForkTimeoutHash = "0a1b2c3d4e5f6071" // fix/api-timeout merge-base (earlier)
	mockForkIngestHash  = "6d7e8f9001122334" // feat/ingest-batching merge-base (oldest listed)
	mockMergeIngestHash = "7c8d9e0f1a2b3c4d" // feat/ingest-batching merge commit (join anchor)
)

// Map / Review mock data.
//
// The mock serves one believable small project — a Go service with a TS
// frontend — so `--mock-data-store=web,map,review` demos the Map and Review
// surfaces without a populated store: 3 modules / 8 packages with structure
// edges including one import cycle (internal/ingest ↔ internal/queue),
// activity edges, ~15 tasks, a review list with two open and one merged
// change, and a change detail with a slice and bound/candidate sessions.
//
// Whatever projectHash the client asks for is echoed back (the mock has no
// project registry), mirroring the ProjectFamiliarity mock.

// mockProjectHash derives a deterministic 64-hex project hash from a project
// display name, shaped like the store's HMAC-SHA256 project_hash. It lets the
// frontend resolve name → hash from mock SessionSummaries and round-trip it
// through the Map/Review REST endpoints.
func mockProjectHash(name string) schema.ProjectHash {
	sum := sha256.Sum256([]byte("peasant-mock:" + name))
	encoded := hex.EncodeToString(sum[:])
	projectHash, err := schema.NewProjectHash(encoded)
	if err != nil {
		panic(fmt.Sprintf("mock project hash generation produced invalid SHA-256 identity %q for project %q: %v", encoded, name, err))
	}
	return projectHash
}

// mockMsAgo returns a pointer to a Unix-ms timestamp d before now.
func mockMsAgo(d time.Duration) *int64 {
	ms := time.Now().Add(-d).UnixMilli()
	return &ms
}

// taskSessionID returns the ID of the i-th mock session (cycling), so task
// and work links drill into sessions that SessionByID can actually serve.
func (p *Provider) taskSessionID(i int) string {
	if len(p.sessions) == 0 {
		return "mock-session-0"
	}
	return string(p.sessions[i%len(p.sessions)].ID)
}

// taskSessionHarness returns the harness of the i-th mock session (cycling).
func (p *Provider) taskSessionHarness(i int) string {
	if len(p.sessions) == 0 {
		return ""
	}
	return string(p.sessions[i%len(p.sessions)].Harness)
}

// mockMapNodes is the node tree of the mock project: 3 modules (cmd,
// internal, web) and 8 packages. Layer/order follow the codegraph
// discipline: cmd pinned to layer 0, dependencies below their dependents.
func mockMapNodes() []schema.MapNode {
	const (
		goLang = "go"
		tsLang = "typescript"
	)
	nodes := []schema.MapNode{
		// Modules (top-level directories).
		{ID: "cmd", Kind: schema.MapNodeKindModule, Name: "cmd", Language: goLang, Layer: 0, Order: 0, Loc: 410, FileCount: 3, RecordedFiles: 2, TotalFiles: 3, TouchCount: 6, EffortDensity: 0.10},
		{ID: "internal", Kind: schema.MapNodeKindModule, Name: "internal", Language: goLang, Layer: 1, Order: 0, Loc: 6240, FileCount: 38, RecordedFiles: 29, TotalFiles: 38, TouchCount: 73, EffortDensity: 0.34},
		{ID: "web", Kind: schema.MapNodeKindModule, Name: "web", Language: tsLang, Layer: 1, Order: 1, Loc: 2810, FileCount: 22, RecordedFiles: 12, TotalFiles: 22, TouchCount: 21, EffortDensity: 0.18},

		// Packages.
		{ID: "cmd/fortuna", Parent: "cmd", Kind: schema.MapNodeKindPackage, Name: "fortuna", Language: goLang, Layer: 0, Order: 0, Loc: 410, FileCount: 3, RecordedFiles: 2, TotalFiles: 3, TouchCount: 6, EffortDensity: 0.10},
		{ID: "internal/api", Parent: "internal", Kind: schema.MapNodeKindPackage, Name: "api", Language: goLang, Layer: 1, Order: 0, Loc: 1980, FileCount: 11, RecordedFiles: 10, TotalFiles: 11, TouchCount: 31, EffortDensity: 0.42},
		{ID: "internal/ingest", Parent: "internal", Kind: schema.MapNodeKindPackage, Name: "ingest", Language: goLang, Layer: 2, Order: 0, Loc: 2140, FileCount: 13, RecordedFiles: 9, TotalFiles: 13, TouchCount: 24, EffortDensity: 0.51},
		{ID: "internal/queue", Parent: "internal", Kind: schema.MapNodeKindPackage, Name: "queue", Language: goLang, Layer: 2, Order: 1, Loc: 640, FileCount: 4, RecordedFiles: 3, TotalFiles: 4, TouchCount: 9, EffortDensity: 0.27},
		{ID: "internal/store", Parent: "internal", Kind: schema.MapNodeKindPackage, Name: "store", Language: goLang, Layer: 3, Order: 0, Loc: 1190, FileCount: 7, RecordedFiles: 5, TotalFiles: 7, TouchCount: 7, EffortDensity: 0.15},
		{ID: "internal/config", Parent: "internal", Kind: schema.MapNodeKindPackage, Name: "config", Language: goLang, Layer: 3, Order: 1, Loc: 290, FileCount: 3, RecordedFiles: 2, TotalFiles: 3, TouchCount: 2, EffortDensity: 0.0},
		{ID: "web/src/components", Parent: "web", Kind: schema.MapNodeKindPackage, Name: "components", Language: tsLang, Layer: 1, Order: 1, Loc: 1760, FileCount: 14, RecordedFiles: 8, TotalFiles: 14, TouchCount: 14, EffortDensity: 0.22},
		{ID: "web/src/lib", Parent: "web", Kind: schema.MapNodeKindPackage, Name: "lib", Language: tsLang, Layer: 2, Order: 2, Loc: 1050, FileCount: 8, RecordedFiles: 4, TotalFiles: 8, TouchCount: 7, EffortDensity: 0.12},
	}
	for index := range nodes {
		nodes[index].ReadAttribution = schema.ReadAttributionUnavailable
		nodes[index].ReadState = schema.ReadStateGradeNone
	}
	return nodes
}

// mockStructureEdges is the package-grain import graph of the mock project.
// internal/ingest ↔ internal/queue is a deliberate cycle (see violations).
func mockStructureEdges() []schema.MapEdge {
	return []schema.MapEdge{
		{From: "cmd/fortuna", To: "internal/api", Count: 3},
		{From: "cmd/fortuna", To: "internal/config", Count: 1},
		{From: "internal/api", To: "internal/store", Count: 7},
		{From: "internal/api", To: "internal/ingest", Count: 4},
		{From: "internal/ingest", To: "internal/store", Count: 5},
		{From: "internal/ingest", To: "internal/queue", Count: 2},
		{From: "internal/queue", To: "internal/ingest", Count: 1}, // cycle arm
		{From: "internal/store", To: "internal/config", Count: 2},
		{From: "web/src/components", To: "web/src/lib", Count: 9},
	}
}

// mockActivityEdges are co-edit observations at package grain (floor ≥2).
func mockActivityEdges() []schema.ActivityEdge {
	return []schema.ActivityEdge{
		{From: "internal/api", To: "internal/store", TaskCount: 4},
		{From: "internal/api", To: "web/src/lib", TaskCount: 2},
		{From: "internal/ingest", To: "internal/queue", TaskCount: 3},
	}
}

// mockViolations flags the ingest ↔ queue import cycle.
func mockViolations() []schema.EdgeViolation {
	return []schema.EdgeViolation{
		{Kind: schema.EdgeViolationCycle, From: "internal/queue", To: "internal/ingest"},
	}
}

// mockTaskTitles are the depth-0 user-turn titles of the mock tasks.
var mockTaskTitles = []string{
	"Add retry backoff to the queue consumer",
	"Fix flaky ingest test on empty batches",
	"Wire the sessions endpoint to the new store reader",
	"Refactor config loading to support env overrides",
	"Add pagination to the sessions API",
	"Investigate timeout on large transcript uploads",
	"Render the activity sparkline on the dashboard",
	"Dedupe commits in the ingest pipeline",
	"Add health check endpoint",
	"Fix CORS errors on the dev proxy",
	"Batch queue writes to reduce lock contention",
	"Extract shared API client into web/src/lib",
	"Add store migration for session labels",
	"Handle renamed files in the diff classifier",
	"Tighten validation on the push payload",
}

// mockTaskFiles maps each task (by index) to the files it edited, spread
// across the mock packages so activity rolls up plausibly.
var mockTaskFiles = [][]string{
	{"internal/queue/consumer.go", "internal/queue/backoff.go"},
	{"internal/ingest/pipeline_test.go"},
	{"internal/api/sessions.go", "internal/store/reader.go"},
	{"internal/config/config.go"},
	{"internal/api/sessions.go", "internal/api/pagination.go"},
	{"internal/api/upload.go", "internal/ingest/transcript.go"},
	{"web/src/components/Sparkline.tsx", "web/src/lib/api.ts"},
	{"internal/ingest/commits.go", "internal/queue/dedupe.go"},
	{"internal/api/health.go"},
	{"internal/api/server.go", "web/src/lib/api.ts"},
	{"internal/queue/writer.go", "internal/store/writer.go"},
	{"web/src/lib/client.ts", "web/src/components/SessionList.tsx"},
	{"internal/store/migrations.go"},
	{"internal/ingest/diff.go"},
	{"internal/api/push.go", "internal/store/writer.go"},
}

// mockTasks builds ~15 deterministic tasks, newest first, linked to real
// mock session IDs so viewer drills resolve.
func (p *Provider) mockTasks() []schema.TaskSummary {
	outcomes := []string{
		string(schema.OutcomeResolved),
		string(schema.OutcomeResolved),
		string(schema.OutcomePartial),
		string(schema.OutcomeResolved),
		string(schema.OutcomeFailed),
	}
	labels := [][]string{
		{"feature"},
		{"bug"},
		{"feature", "frustration:detected"},
		{"refactor"},
		{"bug"},
	}

	tasks := make([]schema.TaskSummary, 0, len(mockTaskTitles))
	for i, title := range mockTaskTitles {
		t := schema.NewTaskSummary(p.taskSessionID(i), 2*i)
		t.Title = title
		t.StartMs = mockMsAgo(time.Duration(i*7+3) * time.Hour)
		t.Outcome = outcomes[i%len(outcomes)]
		t.EditedFiles = append(t.EditedFiles, mockTaskFiles[i%len(mockTaskFiles)]...)
		t.ReadCount = 2 + i%5
		t.RetryLoop = i%5 == 2 // one retry loop per outcome cycle
		t.Labels = append(t.Labels, labels[i%len(labels)]...)
		tasks = append(tasks, t)
	}
	return tasks
}

// tasksTouching filters the mock tasks to those that edited the given file
// or any file under the given directory path.
func tasksTouching(tasks []schema.TaskSummary, path string) []schema.TaskSummary {
	if path == "" {
		return tasks
	}
	matched := make([]schema.TaskSummary, 0, len(tasks))
	for _, t := range tasks {
		for _, f := range t.EditedFiles {
			if f == path || (len(f) > len(path) && f[:len(path)] == path && f[len(path)] == '/') {
				matched = append(matched, t)
				break
			}
		}
	}
	return matched
}

// Mock home-picker projects. fortuna is the repo project the rest of the
// Map/Review mock describes; docs-site is the repo-less project (recorded
// activity only, so coverage is recorded-edit-only and openChanges is 0).
const (
	mockRepoProject     = "fortuna"
	mockRepoLessProject = "docs-site"
)

// ProjectSummaries returns the home-picker rows: the fortuna repo project
// (coherent with the map graph's module roll-up and the two open review
// changes) and one repo-less project. Ordered by project name.
func (p *Provider) ProjectSummaries(_ context.Context) (*codemap.ProjectSummariesResult, error) {
	// fortuna coverage = the sum over the mock map's module nodes
	// (cmd 2/3 + internal 29/38 + web 12/22).
	var recorded, total int
	for _, n := range mockMapNodes() {
		if n.Kind == schema.MapNodeKindModule {
			recorded += n.RecordedFiles
			total += n.TotalFiles
		}
	}

	// The repo-less project's session count stays coherent with the mock
	// session list (SessionSummaries serves the same names and hashes).
	repoLessSessions := 0
	for _, s := range p.sessions {
		if s.Project == mockRepoLessProject {
			repoLessSessions++
		}
	}

	result := &codemap.ProjectSummariesResult{
		Projects: []schema.ProjectSummary{},
		// The mock always simulates an unfiltered store: no persisted
		// kickstart selection narrows it, so nothing is reported hidden.
		Selection: codemap.SelectionState{Active: false},
	}
	result.Projects = append(result.Projects,
		schema.ProjectSummary{
			ProjectHash:   mockProjectHash(mockRepoLessProject),
			Project:       mockRepoLessProject,
			Sessions:      repoLessSessions,
			RecordedFiles: 9, // repo-less: recorded-edit-only mode, so recorded == total
			TotalFiles:    9,
			LastWorkMs:    mockMsAgo(76 * time.Hour),
			OpenChanges:   0,
		},
		schema.ProjectSummary{
			ProjectHash:   mockProjectHash(mockRepoProject),
			Project:       mockRepoProject,
			Sessions:      len(p.sessions),
			RecordedFiles: recorded,
			TotalFiles:    total,
			LastWorkMs:    mockMsAgo(3 * time.Hour), // matches the freshest review change
			OpenChanges:   2,                        // matches ReviewChanges' open rows
		},
	)
	return result, nil
}

// MapGraph returns the static mock map graph, echoing the requested hash.
func (p *Provider) MapGraph(_ context.Context, projectHash schema.ProjectHash, commit string) (*schema.MapGraphPayload, error) {
	payload := schema.NewMapGraphPayload(projectHash)
	payload.RepoFound = true
	payload.RepoPath = "/home/mock/projects/fortuna"
	payload.ParsedLanguages = append(payload.ParsedLanguages, "go", "typescript")
	payload.Nodes = append(payload.Nodes, mockMapNodes()...)
	payload.StructureEdges = append(payload.StructureEdges, mockStructureEdges()...)
	payload.ActivityEdges = append(payload.ActivityEdges, mockActivityEdges()...)
	payload.Violations = append(payload.Violations, mockViolations()...)
	payload.GeneratedAtMs = time.Now().UnixMilli()
	payload.AtCommit = commit
	return payload, nil
}

// MapNodeDetail returns a rail panel for the requested node. Known node IDs
// get their graph metrics; unknown paths still get a plausible panel (the
// mock has no notion of a missing node).
func (p *Provider) MapNodeDetail(_ context.Context, _ schema.ProjectHash, path string) (*schema.MapNodeDetailPayload, error) {
	payload := schema.NewMapNodeDetailPayload(path)
	payload.Kind = schema.MapNodeKindPackage
	payload.Loc = 980
	payload.RecordedFiles = 5
	payload.TotalFiles = 7
	for _, n := range mockMapNodes() {
		if n.ID == path {
			payload.Kind = n.Kind
			payload.Loc = n.Loc
			payload.RecordedFiles = n.RecordedFiles
			payload.TotalFiles = n.TotalFiles
			break
		}
	}

	shaped := tasksTouching(p.mockTasks(), path)
	if len(shaped) == 0 {
		shaped = p.mockTasks()[:5]
	}
	if len(shaped) > 5 {
		shaped = shaped[:5]
	}
	payload.ShapedBy = append(payload.ShapedBy, shaped...)

	sessions := make(map[string]bool, len(payload.ShapedBy))
	for _, t := range payload.ShapedBy {
		sessions[t.SessionID] = true
	}
	payload.SessionCount = len(sessions)
	payload.TaskCount = len(payload.ShapedBy)
	payload.LastTouchMs = mockMsAgo(3 * time.Hour)
	// Relative for the same reason as mockRecentCommits: this panel's other
	// commits age with the wall clock, so the shared fixture's absolute
	// timestamp must not be used to place this one.
	first := schema.NewCommitRef(canonicalStrikeMockFixture.MapCommit.Hash, canonicalStrikeMockFixture.MapCommit.Subject)
	first.TimeMs = mockMsAgo(0)
	first.SessionIDs = append(first.SessionIDs, schema.SessionID(p.taskSessionID(0)))
	first.HasSession = true
	populateMockCommitAssociations(&first)
	second := schema.NewCommitRef("b2c3d4e5f6071829", "fix: empty-batch ingest crash")
	second.TimeMs = mockMsAgo(26 * time.Hour)
	second.SessionIDs = append(second.SessionIDs, schema.SessionID(p.taskSessionID(1)))
	second.HasSession = true
	populateMockCommitAssociations(&second)
	third := schema.NewCommitRef("c3d4e5f607182930", "chore: bump deps")
	third.TimeMs = mockMsAgo(50 * time.Hour)
	payload.RecentCommits = append(payload.RecentCommits, first, second, third)
	payload.RetryLoops = 1
	payload.ReEdits = 2
	cost := 4.18
	payload.CostUsd = &cost
	return payload, nil
}

// ProjectTasks returns the mock task list, optionally filtered by file.
func (p *Provider) ProjectTasks(_ context.Context, projectHash schema.ProjectHash, file string) (*schema.ProjectTasksPayload, error) {
	payload := schema.NewProjectTasksPayload(projectHash)
	payload.FileFilter = file
	payload.Tasks = append(payload.Tasks, tasksTouching(p.mockTasks(), file)...)
	return payload, nil
}

// mockRecentCommits builds the lane-0 commit strip: 12 default-branch
// commits, newest first, containing every Changes-graph anchor hash. A few
// commits carry recorded sessions; the rest are unrecorded.
func (p *Provider) mockRecentCommits() []schema.CommitRef {
	rows := []struct {
		hash       string
		subject    string
		hoursAgo   time.Duration
		hasSession bool
	}{
		{canonicalStrikeMockFixture.MapCommit.Hash, canonicalStrikeMockFixture.MapCommit.Subject, 0, true},
		{"f60718293a4b5c6d", "feat: activity sparkline on the dashboard", 9, true},
		{"0718293a4b5c6d7e", "chore: tighten lint config", 16, false},
		{"18293a4b5c6d7e8f", "feat: pagination on the sessions API", 24, true},
		{mockForkCycleHash, "refactor: extract store reader", 30, true},
		{"293a4b5c6d7e8f90", "fix: CORS errors on the dev proxy", 38, false},
		{"3a4b5c6d7e8f9001", "feat: project picker on the map home", 47, true},
		{"4b5c6d7e8f900112", "docs: annotate ingest pipeline stages", 56, false},
		{"5c6d7e8f90011223", "fix: pagination off-by-one in sessions API", 64, true},
		{mockMergeIngestHash, "Merge branch 'feat/ingest-batching'", 72, true},
		{mockForkTimeoutHash, "feat: batch queue writes to reduce lock contention", 80, true},
		{mockForkIngestHash, "chore: bump deps", 90, false},
	}
	refs := make([]schema.CommitRef, 0, len(rows))
	// Every row's time is relative to now. Do not substitute the shared fixture's
	// absolute mapCommit.timeMs here: the rest of the strip ages with the wall
	// clock, so a fixed timestamp silently stops being the newest entry once
	// enough of the day has passed, breaking the newest-first ordering this
	// strip guarantees. The fixture's absolute value stays canonical for the
	// frontend tests, which pin their own clock.
	for i, r := range rows {
		ref := schema.NewCommitRef(r.hash, r.subject)
		ref.TimeMs = mockMsAgo(r.hoursAgo * time.Hour)
		if r.hasSession {
			ref.SessionIDs = append(ref.SessionIDs, schema.SessionID(p.taskSessionID(i)))
			ref.HasSession = true
		}
		populateMockCommitAssociations(&ref)
		refs = append(refs, ref)
	}
	return refs
}

// ReviewChanges returns two open changes and one merged change, anchored to
// the lane-0 commit strip: feat/queue-retry forks mid-list and carries the
// freshest tip, fix/api-timeout forks earlier, and feat/ingest-batching
// rejoins at its listed merge commit.
func (p *Provider) ReviewChanges(_ context.Context, projectHash schema.ProjectHash) (*schema.ReviewListPayload, error) {
	payload := schema.NewReviewListPayload(projectHash)
	payload.RepoFound = true
	payload.DefaultBranch = mockDefaultBranch
	for i, session := range p.sessions {
		startMs := session.StartTime.UnixMilli()
		title := mockTaskTitles[i%len(mockTaskTitles)]
		if session.ID.String() == canonicalStrikeMockFixture.SessionDetail.ID {
			title = canonicalStrikeMockFixture.Expected.ReviewSessionTitle
		}
		payload.Sessions = append(payload.Sessions, schema.TimelineSessionRef{
			SessionID:        schema.SessionID(session.ID),
			Title:            title,
			Harness:          schema.Harness(session.Harness),
			StartMs:          &startMs,
			HasCommitBinding: true,
		})
	}
	payload.RecentCommits = append(payload.RecentCommits, p.mockRecentCommits()...)
	canonicalizeReviewTimeline(payload)

	// The merged row's MergedAtMs is the listed merge commit's time — the
	// join anchor and the row time must agree exactly.
	var mergedAt *int64
	for _, c := range payload.RecentCommits {
		if c.Hash == mockMergeIngestHash {
			mergedAt = c.TimeMs
			break
		}
	}

	payload.Changes = append(payload.Changes,
		schema.ChangeSummary{
			Branch:       mockBranchCycle,
			AheadCount:   5,
			BehindCount:  1,
			FilesChanged: 6,
			SessionCount: 2,
			TaskCount:    3,
			NewEdges:     1,
			RemovedEdges: 0,
			Violations:   1,
			LastWorkMs:   mockMsAgo(3 * time.Hour),
			BaseHash:     mockForkCycleHash,
			TipCommitMs:  mockMsAgo(4 * time.Hour),
		},
		schema.ChangeSummary{
			Branch:       mockBranchNoDelta,
			AheadCount:   2,
			BehindCount:  0,
			FilesChanged: 3,
			SessionCount: 1,
			TaskCount:    1,
			NewEdges:     0,
			RemovedEdges: 0,
			Violations:   0,
			LastWorkMs:   mockMsAgo(20 * time.Hour),
			BaseHash:     mockForkTimeoutHash,
			TipCommitMs:  mockMsAgo(22 * time.Hour),
		},
		schema.ChangeSummary{
			Branch:          mockBranchMerged,
			Merged:          true,
			MergedAtMs:      mergedAt,
			MergeCommitHash: mockMergeIngestHash,
		},
	)
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("mock: invalid review timeline for project %q during ReviewChanges: %w; the mock cannot safely render session actions; update mock sessions and commit bindings together", projectHash, err)
	}
	return payload, nil
}

func canonicalizeReviewTimeline(payload *schema.ReviewListPayload) {
	sort.Slice(payload.Sessions, func(i, j int) bool {
		left, right := payload.Sessions[i], payload.Sessions[j]
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
	sessionRank := make(map[schema.SessionID]int, len(payload.Sessions))
	for rank, session := range payload.Sessions {
		sessionRank[session.SessionID] = rank
	}
	for index := range payload.RecentCommits {
		commit := &payload.RecentCommits[index]
		type binding struct {
			sessionID   schema.SessionID
			association schema.SessionAssociation
		}
		bindings := make([]binding, len(commit.SessionIDs))
		for bindingIndex, sessionID := range commit.SessionIDs {
			bindings[bindingIndex] = binding{sessionID: sessionID, association: commit.Associations[bindingIndex]}
		}
		sort.Slice(bindings, func(i, j int) bool {
			return sessionRank[bindings[i].sessionID] < sessionRank[bindings[j].sessionID]
		})
		for bindingIndex, binding := range bindings {
			commit.SessionIDs[bindingIndex] = binding.sessionID
			commit.Associations[bindingIndex] = binding.association
		}
	}
}

// populateMockCommitAssociations mirrors mock commit bindings into the current
// contract's explicit association array. Mock bindings are all direct recorded
// commit observations, so they are confirmed with high confidence.
func populateMockCommitAssociations(commit *schema.CommitRef) {
	commit.Associations = make([]schema.SessionAssociation, 0, len(commit.SessionIDs))
	for _, sessionID := range commit.SessionIDs {
		associationID, err := schema.NewAssociationID(fmt.Sprintf("mock:%s:%s", commit.Hash, sessionID))
		if err != nil {
			panic(fmt.Sprintf("mock association ID generation failed for commit %q and session %q: %v", commit.Hash, sessionID, err))
		}
		hash := commit.Hash
		commit.Associations = append(commit.Associations, schema.SessionAssociation{
			ID:         associationID,
			SessionID:  sessionID,
			Conclusion: schema.AssociationConclusionConfirmed,
			Confidence: schema.ConfidenceHigh,
			Evidence: []schema.AssociationEvidenceObservation{{
				Kind:               schema.AssociationEvidenceRecordedCommit,
				RecordedCommitHash: &hash,
			}},
		})
	}
}

// ResolveProject resolves one explicit mock project identity for deep-link
// behavior parity with the store provider.
func (p *Provider) ResolveProject(ctx context.Context, project string) (*schema.ProjectResolutionPayload, error) {
	if canonical, err := schema.NewProjectHash(project); err == nil {
		payload, summaryErr := p.ProjectSummaries(ctx)
		if summaryErr != nil {
			return nil, summaryErr
		}
		for _, candidate := range payload.Projects {
			if candidate.ProjectHash == canonical {
				return &schema.ProjectResolutionPayload{Project: candidate.Project, ProjectHash: canonical}, nil
			}
		}
		return nil, fmt.Errorf("%w: explicit mock project hash %q", codemap.ErrProjectNotFound, canonical)
	}
	payload, err := p.ProjectSummaries(ctx)
	if err != nil {
		return nil, err
	}
	for _, candidate := range payload.Projects {
		if candidate.Project == project {
			return &schema.ProjectResolutionPayload{Project: candidate.Project, ProjectHash: candidate.ProjectHash}, nil
		}
	}
	return nil, fmt.Errorf("%w: explicit mock project %q", codemap.ErrProjectNotFound, project)
}

// ChangeDetail returns a change detail coherent with the ReviewChanges list
// row for the requested branch. Unknown branches take the same not-found
// path as the real provider.
func (p *Provider) ChangeDetail(_ context.Context, _ schema.ProjectHash, branch string) (*schema.ChangeDetailPayload, error) {
	switch branch {
	case mockBranchCycle:
		return p.cycleChangeDetail(branch), nil
	case mockBranchNoDelta:
		return p.noDeltaChangeDetail(branch), nil
	case mockBranchMerged:
		return mergedChangeDetail(branch), nil
	default:
		return nil, fmt.Errorf("%w: %q", codemap.ErrBranchNotFound, branch)
	}
}

// ChangeDiff returns a small representative rendered diff for the mock branches
// so the per-file diff view has something to render in mock mode.
func (p *Provider) ChangeDiff(_ context.Context, _ schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error) {
	switch branch {
	case mockBranchCycle, mockBranchNoDelta, mockBranchMerged:
		payload := schema.NewChangeDiffPayload(branch, file)
		payload.Status = "M"
		payload.Hunks = []schema.DiffHunk{{
			OldStart: 10, OldLines: 5, NewStart: 10, NewLines: 6,
			Header:       "func process()",
			SessionID:    p.taskSessionID(0),
			SessionTitle: "Add retry backoff to the queue worker",
			Lines: []schema.DiffLine{
				{Kind: "context", Text: "func process() error {"},
				{Kind: "del", Text: "\tretries := 3"},
				{Kind: "add", Text: "\tretries := 5"},
				{Kind: "add", Text: "\tbackoff := time.Second"},
				{Kind: "context", Text: "\tfor i := 0; i < retries; i++ {"},
				{Kind: "context", Text: "\t\tif err := attempt(); err == nil {"},
			},
		}}
		return payload, nil
	default:
		return nil, fmt.Errorf("%w: %q", codemap.ErrBranchNotFound, branch)
	}
}

// Search returns canned full-text results over the mock tasks so the Cmd-K
// "Messages" group has something to drill into under --mock-data-store=web,search.
// Each result points at a real mock session + its task entry index (2*i, the
// same coordinates mockTasks uses), so the deep-link resolves. The snippet
// brackets the query substring the way FTS5's snippet() would; tasks whose title
// contains the query rank first, and if none match the first few are returned
// with the query spliced in so the group is never empty for a real query.
func (p *Provider) Search(_ context.Context, query string, limit int) (*schema.SearchPayload, error) {
	payload := schema.NewSearchPayload(query)
	q := strings.TrimSpace(query)
	if len(q) < 2 {
		return payload, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	hash := mockProjectHash(mockRepoProject)
	n := len(mockTaskTitles)
	add := func(i int, snippet string) {
		payload.Results = append(payload.Results, schema.SearchResult{
			SessionID:   p.taskSessionID(i),
			Project:     mockRepoProject,
			ProjectHash: hash,
			EntryIndex:  2 * i,
			Role:        string(schema.RoleUser),
			Snippet:     snippet,
			Score:       float64(n-i) / float64(n), // newest tasks rank highest
		})
	}

	lower := strings.ToLower(q)
	for i, title := range mockTaskTitles {
		if len(payload.Results) >= limit {
			break
		}
		if idx := strings.Index(strings.ToLower(title), lower); idx >= 0 {
			add(i, title[:idx]+"["+title[idx:idx+len(q)]+"]"+title[idx+len(q):])
		}
	}
	for i := 0; len(payload.Results) == 0 && i < 3 && i < n; i++ {
		add(i, mockTaskTitles[i]+" … ["+q+"]")
	}
	return payload, nil
}

// mockSlice scopes the static mock map to the given node IDs: touched
// packages + ancestors + 1-hop structure neighborhood, layer/order preserved
// from the full map.
func mockSlice(ids ...string) schema.MapSlice {
	in := make(map[string]bool, len(ids))
	for _, id := range ids {
		in[id] = true
	}
	slice := schema.NewMapSlice()
	for _, n := range mockMapNodes() {
		if in[n.ID] {
			slice.Nodes = append(slice.Nodes, n)
		}
	}
	for _, e := range mockStructureEdges() {
		if in[e.From] && in[e.To] {
			slice.StructureEdges = append(slice.StructureEdges, e)
		}
	}
	for _, e := range mockActivityEdges() {
		if in[e.From] && in[e.To] {
			slice.ActivityEdges = append(slice.ActivityEdges, e)
		}
	}
	return slice
}

// cycleChangeDetail is the rich detail behind the feat/queue-retry list row:
// the queue/ingest slice with one new cycle violation, one bound and one
// candidate session, and an unrecorded commit.
func (p *Provider) cycleChangeDetail(branch string) *schema.ChangeDetailPayload {
	payload := schema.NewChangeDetailPayload(branch)
	payload.BaseRef = mockForkCycleHash
	payload.DefaultBranch = mockDefaultBranch

	oldPath := "internal/queue/retry.go"
	payload.Files = append(payload.Files,
		schema.FileChange{Path: "internal/queue/consumer.go", Status: "M", LinesAdded: 64, LinesRemoved: 22},
		schema.FileChange{Path: "internal/queue/backoff.go", Status: "A", LinesAdded: 120, LinesRemoved: 0},
		schema.FileChange{Path: "internal/queue/dedupe.go", Status: "R", OldPath: &oldPath, LinesAdded: 8, LinesRemoved: 6},
		schema.FileChange{Path: "internal/ingest/pipeline.go", Status: "M", LinesAdded: 31, LinesRemoved: 14},
		schema.FileChange{Path: "internal/ingest/commits.go", Status: "M", LinesAdded: 12, LinesRemoved: 9},
		schema.FileChange{Path: "internal/store/writer.go", Status: "M", LinesAdded: 5, LinesRemoved: 2},
	)

	payload.Slice = mockSlice("internal", "internal/api", "internal/ingest", "internal/queue", "internal/store")

	payload.NewEdges = append(payload.NewEdges, schema.MapEdge{From: "internal/queue", To: "internal/ingest", Count: 1})
	payload.NewNodes = append(payload.NewNodes, "internal/queue/backoff.go")
	payload.Violations = append(payload.Violations, mockViolations()...)

	// Work: one bound session (two tasks) and one candidate (one task).
	tasks := p.mockTasks()
	bound := schema.NewChangeSession(p.taskSessionID(0), schema.ChangeBindingBound)
	bound.Title = tasks[0].Title
	bound.Harness = p.taskSessionHarness(0)
	bound.StartMs = mockMsAgo(5 * time.Hour)
	bound.Tasks = append(bound.Tasks, tasks[0], tasks[10])

	candidate := schema.NewChangeSession(p.taskSessionID(1), schema.ChangeBindingCandidate)
	candidate.Title = tasks[7].Title
	candidate.Harness = p.taskSessionHarness(1)
	candidate.StartMs = mockMsAgo(30 * time.Hour)
	candidate.Tasks = append(candidate.Tasks, tasks[7])

	payload.Work = append(payload.Work, bound, candidate)

	unrecorded := schema.NewCommitRef("d4e5f60718293a4b", "fixup: gofmt")
	unrecorded.TimeMs = mockMsAgo(28 * time.Hour)
	payload.UnrecordedCommits = append(payload.UnrecordedCommits, unrecorded)

	payload.LinesAdded = 312
	payload.LinesRemoved = 87
	payload.OutputTokens = 48230
	cost := 2.74
	payload.CostUsd = &cost
	return payload
}

// noDeltaChangeDetail is the detail behind the fix/api-timeout list row:
// three modified files, no structure delta, no violations, one bound session
// with one task — matching the row's "no changes" structure columns.
func (p *Provider) noDeltaChangeDetail(branch string) *schema.ChangeDetailPayload {
	payload := schema.NewChangeDetailPayload(branch)
	payload.BaseRef = mockForkTimeoutHash
	payload.DefaultBranch = mockDefaultBranch

	payload.Files = append(payload.Files,
		schema.FileChange{Path: "internal/api/upload.go", Status: "M", LinesAdded: 18, LinesRemoved: 7},
		schema.FileChange{Path: "internal/api/server.go", Status: "M", LinesAdded: 9, LinesRemoved: 3},
		schema.FileChange{Path: "internal/ingest/transcript.go", Status: "M", LinesAdded: 41, LinesRemoved: 12},
	)

	payload.Slice = mockSlice("internal", "internal/api", "internal/ingest", "internal/store", "internal/queue")

	// Task 5 edits internal/api/upload.go + internal/ingest/transcript.go —
	// the timeout investigation behind this branch.
	tasks := p.mockTasks()
	bound := schema.NewChangeSession(p.taskSessionID(5), schema.ChangeBindingBound)
	bound.Title = tasks[5].Title
	bound.Harness = p.taskSessionHarness(5)
	bound.StartMs = mockMsAgo(21 * time.Hour)
	bound.Tasks = append(bound.Tasks, tasks[5])
	payload.Work = append(payload.Work, bound)

	payload.LinesAdded = 64
	payload.LinesRemoved = 18
	payload.OutputTokens = 9120
	cost := 0.42
	payload.CostUsd = &cost
	return payload
}

// mergedChangeDetail is the minimal detail behind the merged
// feat/ingest-batching row: like the list's merged rows it carries identity
// only — no files, no deltas, no work (merged-branch facts need merge-base
// historical diffing).
func mergedChangeDetail(branch string) *schema.ChangeDetailPayload {
	payload := schema.NewChangeDetailPayload(branch)
	payload.BaseRef = mockForkIngestHash
	payload.DefaultBranch = mockDefaultBranch
	return payload
}
