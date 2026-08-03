package codemap_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// taskByIdentity finds a task by (sessionID, entryIndex).
func taskByIdentity(t *testing.T, tasks []schema.TaskSummary, sessionID string, entryIndex int) schema.TaskSummary {
	t.Helper()
	for _, task := range tasks {
		if task.SessionID == sessionID && task.EntryIndex == entryIndex {
			return task
		}
	}
	t.Fatalf("task %s@%d not found in %d tasks", sessionID, entryIndex, len(tasks))
	return schema.TaskSummary{}
}

// TestProjectTasks_RollupGrain: one task per depth-0 user turn, reverse
// chronological, with the edit/read split per the Touch rule.
func TestProjectTasks_RollupGrain(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.ProjectTasks(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("ProjectTasks: %v", err)
	}

	// 2 user turns in session 1 + 1 in session 2.
	if len(payload.Tasks) != 3 {
		t.Fatalf("len(Tasks) = %d, want 3", len(payload.Tasks))
	}

	// Reverse chronological: session1@4 (base+3500), session1@0 (base+3000),
	// session2@0 (base+1000).
	gotOrder := []string{}
	for _, task := range payload.Tasks {
		gotOrder = append(gotOrder, task.SessionID)
	}
	wantOrder := []string{fxSession1, fxSession1, fxSession2}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("task session order = %v, want %v", gotOrder, wantOrder)
	}
	if payload.Tasks[0].EntryIndex != 4 || payload.Tasks[1].EntryIndex != 0 {
		t.Errorf("session1 task order = [%d %d], want [4 0]",
			payload.Tasks[0].EntryIndex, payload.Tasks[1].EntryIndex)
	}

	// Task 1 of session 1: one edit, one read — reads never join editedFiles.
	task1 := taskByIdentity(t, payload.Tasks, fxSession1, 0)
	if want := []string{"internal/a/a.go"}; !reflect.DeepEqual(task1.EditedFiles, want) {
		t.Errorf("task1 EditedFiles = %v, want %v", task1.EditedFiles, want)
	}
	if task1.ReadCount != 1 {
		t.Errorf("task1 ReadCount = %d, want 1", task1.ReadCount)
	}
	if task1.Title != "Add caching to the ingest pipeline" {
		t.Errorf("task1 Title = %q", task1.Title)
	}
	if task1.RetryLoop {
		t.Error("task1 RetryLoop = true, want false")
	}
	if task1.Outcome != schema.OutcomeResolved.String() {
		t.Errorf("task1 Outcome = %q, want %q", task1.Outcome, schema.OutcomeResolved)
	}
	if task1.StartMs == nil || *task1.StartMs != fxBase()+3000 {
		t.Errorf("task1 StartMs = %v, want %d", task1.StartMs, fxBase()+3000)
	}

	// Task 2 of session 1: two edits (deduped, sorted), out-of-repo edit
	// dropped.
	task2 := taskByIdentity(t, payload.Tasks, fxSession1, 4)
	if want := []string{"internal/a/a.go", "internal/b/b.go"}; !reflect.DeepEqual(task2.EditedFiles, want) {
		t.Errorf("task2 EditedFiles = %v, want %v", task2.EditedFiles, want)
	}

	// Session 2's task: retry loop from the consecutive assistant errors.
	task3 := taskByIdentity(t, payload.Tasks, fxSession2, 0)
	if !task3.RetryLoop {
		t.Error("task3 RetryLoop = false, want true (2 consecutive errors)")
	}
	if want := []string{"docs/notes.md", "internal/a/a.go", "internal/b/b.go"}; !reflect.DeepEqual(task3.EditedFiles, want) {
		t.Errorf("task3 EditedFiles = %v, want %v", task3.EditedFiles, want)
	}

	assertNoNullArrays(t, payload)
}

// TestProjectTasks_TitleWordBoundary: titles longer than 80 chars truncate
// at the last word boundary at or before position 80.
func TestProjectTasks_TitleWordBoundary(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.ProjectTasks(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("ProjectTasks: %v", err)
	}

	task := taskByIdentity(t, payload.Tasks, fxSession1, 4)
	if len(task.Title) > 80 {
		t.Errorf("Title length = %d, want <= 80 (%q)", len(task.Title), task.Title)
	}
	if !strings.HasPrefix(fxLongPreview, task.Title) {
		t.Errorf("Title %q is not a prefix of the preview", task.Title)
	}
	if strings.HasSuffix(task.Title, " ") || !strings.HasPrefix(fxLongPreview[len(task.Title):], " ") {
		t.Errorf("Title %q does not end on a word boundary", task.Title)
	}
}

// TestProjectTasks_FileFilter restricts tasks to those editing the given
// file (or any file under a directory path).
func TestProjectTasks_FileFilter(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	// Exact file: b.go was edited by session1@4 and session2@0.
	payload, err := svc.ProjectTasks(context.Background(), fxProjectHash, "internal/b/b.go")
	if err != nil {
		t.Fatalf("ProjectTasks(file): %v", err)
	}
	if payload.FileFilter != "internal/b/b.go" {
		t.Errorf("FileFilter = %q", payload.FileFilter)
	}
	if len(payload.Tasks) != 2 {
		t.Fatalf("len(Tasks) = %d, want 2", len(payload.Tasks))
	}
	taskByIdentity(t, payload.Tasks, fxSession1, 4)
	taskByIdentity(t, payload.Tasks, fxSession2, 0)

	// Directory path: docs/ matches only session2's task.
	payload, err = svc.ProjectTasks(context.Background(), fxProjectHash, "docs")
	if err != nil {
		t.Fatalf("ProjectTasks(dir): %v", err)
	}
	if len(payload.Tasks) != 1 || payload.Tasks[0].SessionID != fxSession2 {
		t.Errorf("docs filter tasks = %+v, want session2 only", payload.Tasks)
	}

	// Reads never match the filter: session1@0 only READ b.go.
	payload, err = svc.ProjectTasks(context.Background(), fxProjectHash, "internal/b/b.go")
	if err != nil {
		t.Fatalf("ProjectTasks(file) #2: %v", err)
	}
	for _, task := range payload.Tasks {
		if task.SessionID == fxSession1 && task.EntryIndex == 0 {
			t.Error("read-only task matched the file filter")
		}
	}
}

// TestProjectTasks_EffectiveLabels: labels are the session's effective
// annotation values — a human annotation outranks a rule annotation of the
// same type — filtered down to informative chips: path-like values, the
// session outcome (already its own chip), and negative-signal detector
// values never reach the wire.
func TestProjectTasks_EffectiveLabels(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	seedSessionAnnotation(t, s, fxSession1, "outcome-classifier", "resolved")
	seedSessionAnnotation(t, s, fxSession1, "human-web", "failed")
	// Noise annotations that must be filtered out of the task chips.
	seedSessionAnnotationOfType(t, s, fxSession1, "outcome-classifier",
		testutil.TestTypeIDSessionScope, "/Users/someone/Documents/Projects/peasant")
	seedSessionAnnotationOfType(t, s, fxSession1, "outcome-classifier",
		testutil.TestTypeIDUserFrustration, "not_detected")
	// Session 2's outcome metric is "partial" — an annotation with the same
	// value duplicates the outcome chip and is filtered.
	seedSessionAnnotation(t, s, fxSession2, "outcome-classifier", "partial")

	payload, err := svc.ProjectTasks(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("ProjectTasks: %v", err)
	}

	task := taskByIdentity(t, payload.Tasks, fxSession1, 0)
	if want := []string{"failed"}; !reflect.DeepEqual(task.Labels, want) {
		t.Errorf("Labels = %v, want %v (human outranks rule; noise filtered)", task.Labels, want)
	}

	// Session 2's only annotation duplicates its outcome: empty (not nil) labels.
	task2 := taskByIdentity(t, payload.Tasks, fxSession2, 0)
	if task2.Labels == nil || len(task2.Labels) != 0 {
		t.Errorf("session2 Labels = %#v, want empty non-nil slice", task2.Labels)
	}
}

// TestProjectTasks_WorktreePathsRemapToRealTree: tool paths recorded inside
// agent worktrees (<cwd>/.claude/worktrees/<name>/rest, also .claire, also
// nested runs) must remap onto the real repo tree instead of minting phantom
// '.claude/worktrees/…' node IDs — and attribution (touch counts, coverage,
// activity edges) must credit the real modules. Non-worktree dotpaths keep
// their own nodes.
func TestProjectTasks_WorktreePathsRemapToRealTree(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	base := fxBase()

	seedSession(t, s, fxSession3, "", base+5000, base+6000)
	seedEntries(t, s, fxSession3, []entrySpec{
		userTurn(base+5000, "Fix attribution for agent worktree edits"),
		toolUse(base+5100, "Edit", fxCwd+"/.claude/worktrees/agent-x/internal/a/a.go"),
		toolUse(base+5200, "Write", fxCwd+"/.claire/worktrees/agent-y/internal/b/b.go"),
		toolUse(base+5300, "Edit", fxCwd+"/.claude/worktrees/agent-x/.claude/worktrees/agent-z/docs/readme.md"),
		toolUse(base+5400, "Edit", fxCwd+"/.claude/settings.json"), // NOT a worktree path
	})

	// The task's edited files land on the real tree (sorted distinct).
	payload, err := svc.ProjectTasks(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("ProjectTasks: %v", err)
	}
	task := taskByIdentity(t, payload.Tasks, fxSession3, 0)
	want := []string{".claude/settings.json", "docs/readme.md", "internal/a/a.go", "internal/b/b.go"}
	if !reflect.DeepEqual(task.EditedFiles, want) {
		t.Errorf("EditedFiles = %v, want %v", task.EditedFiles, want)
	}

	// Map attribution: no phantom worktree nodes; the real modules carry the
	// touch counts, coverage, and activity edges.
	graph, err := svc.MapGraph(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("MapGraph: %v", err)
	}
	for _, n := range graph.Nodes {
		if strings.Contains(n.ID, "worktrees") {
			t.Errorf("phantom worktree node %q on the map", n.ID)
		}
	}
	// internal/a: 4 fixture edits + 1 via the worktree path.
	nodeA := findNode(t, graph.Nodes, "internal/a")
	if nodeA.TouchCount != 5 {
		t.Errorf("internal/a TouchCount = %d, want 5 (worktree edit credited)", nodeA.TouchCount)
	}
	// docs/readme.md (tracked, previously unedited) becomes recorded via the
	// nested worktree edit: docs coverage moves from 0/2 to 1/2.
	docs := findNode(t, graph.Nodes, "docs")
	if docs.RecordedFiles != 1 || docs.TotalFiles != 2 {
		t.Errorf("docs coverage = %d/%d, want 1/2", docs.RecordedFiles, docs.TotalFiles)
	}
	// The co-edit of a.go + b.go through worktree paths raises the
	// internal/a <-> internal/b activity edge from 2 to 3 tasks.
	foundPair := false
	for _, e := range graph.ActivityEdges {
		if (e.From == "internal/a" && e.To == "internal/b") || (e.From == "internal/b" && e.To == "internal/a") {
			foundPair = true
			if e.TaskCount != 3 {
				t.Errorf("a<->b activity edge TaskCount = %d, want 3", e.TaskCount)
			}
		}
	}
	if !foundPair {
		t.Error("internal/a <-> internal/b activity edge missing")
	}
	// The non-worktree dotpath keeps its own (real) node.
	findNode(t, graph.Nodes, ".claude/settings.json")
}

// TestProjectTasks_TitleFallbackChain: a user-turn preview below the signal
// floor (<3 words or <12 chars) never ships as the task title; the chain is
// preview -> session title -> first edited filename -> "task @ <entryIndex>",
// keeping the <=80-char word-boundary truncation on every arm.
func TestProjectTasks_TitleFallbackChain(t *testing.T) {
	t.Parallel()
	base := fxBase()

	cases := []struct {
		name         string
		preview      string
		metricsTitle string
		editFile     string // repo-relative; "" = no edit
		want         string // exact expected title; "" = use check
		check        func(t *testing.T, got string)
	}{
		{
			name:         "preview with signal wins",
			preview:      "Add caching to the ingest pipeline",
			metricsTitle: "Session one title",
			editFile:     "internal/a/a.go",
			want:         "Add caching to the ingest pipeline",
		},
		{
			name:         "fragment falls back to the session title",
			preview:      "The file",
			metricsTitle: "Session one title",
			editFile:     "internal/a/a.go",
			want:         "Session one title",
		},
		{
			name:         "two long words are still a fragment (word floor)",
			preview:      "Reconfiguring deployments",
			metricsTitle: "Session one title",
			want:         "Session one title",
		},
		{
			name:         "short multi-word is still a fragment (char floor)",
			preview:      "do it now",
			metricsTitle: "Session one title",
			want:         "Session one title",
		},
		{
			name:     "no session title falls back to the first edited filename",
			preview:  "The file",
			editFile: "internal/a/a.go",
			want:     "a.go",
		},
		{
			name:    "no signal anywhere falls back to the task identity",
			preview: "The file",
			want:    "task @ 0",
		},
		{
			name: "empty preview falls back to the task identity",
			want: "task @ 0",
		},
		{
			name:         "session-title fallback keeps the word-boundary truncation",
			preview:      "The file",
			metricsTitle: fxLongPreview,
			check: func(t *testing.T, got string) {
				if len(got) > 80 {
					t.Errorf("title length = %d, want <= 80 (%q)", len(got), got)
				}
				if !strings.HasPrefix(fxLongPreview, got) {
					t.Errorf("title %q is not a prefix of the session title", got)
				}
				if strings.HasSuffix(got, " ") || !strings.HasPrefix(fxLongPreview[len(got):], " ") {
					t.Errorf("title %q does not end on a word boundary", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := storetest.Open(t)
			seedSession(t, s, fxSession1, "", base+1000, base+2000)
			specs := []entrySpec{userTurn(base+1000, tc.preview)}
			if tc.editFile != "" {
				specs = append(specs, toolUse(base+1100, "Edit", fxCwd+"/"+tc.editFile))
			}
			seedEntries(t, s, fxSession1, specs)
			if tc.metricsTitle != "" {
				seedMetrics(t, s, fxSession1, tc.metricsTitle, schema.OutcomeResolved, 100, 0, nil)
			}
			svc := codemap.NewService(s, func(string) gitops.Repository { return noRepo() }, codegraph.NewGraphBuilder(), sessionvisibility.All())

			payload, err := svc.ProjectTasks(context.Background(), fxProjectHash, "")
			if err != nil {
				t.Fatalf("ProjectTasks: %v", err)
			}
			if len(payload.Tasks) != 1 {
				t.Fatalf("len(Tasks) = %d, want 1", len(payload.Tasks))
			}
			got := payload.Tasks[0].Title
			if tc.check != nil {
				tc.check(t, got)
				return
			}
			if got != tc.want {
				t.Errorf("Title = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProjectTasks_WorksWithoutRepo: the tasks surface needs no git at all.
func TestProjectTasks_WorksWithoutRepo(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, noRepo())

	payload, err := svc.ProjectTasks(context.Background(), fxProjectHash, "")
	if err != nil {
		t.Fatalf("ProjectTasks: %v", err)
	}
	if len(payload.Tasks) != 3 {
		t.Errorf("len(Tasks) = %d, want 3", len(payload.Tasks))
	}
}
