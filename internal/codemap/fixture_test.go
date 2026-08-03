package codemap_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// Fixture constants for the seeded project. The project hash and repo cwd
// are shared by every test; sessions are seeded per scenario.
const (
	fxCwd = "/repo"

	fxSession1 = "11111111-1111-1111-1111-111111111111" // newest, bound to feat/x
	fxSession2 = "22222222-2222-2222-2222-222222222222" // older, retry + re-edit
	fxSession3 = "33333333-3333-3333-3333-333333333333" // commit-arm-only candidate
	fxSession4 = "44444444-4444-4444-4444-444444444444" // git_branch-only candidate

	fxBranch       = "feat/x"
	fxMergedBranch = "feat/done"
	fxMergeBase    = "aaaa000000000000000000000000000000000000"
	fxHashA        = "aaaa111111111111111111111111111111111111" // linked to session 1
	fxHashB        = "aaaa222222222222222222222222222222222222" // linked to session 3
	fxHashC        = "aaaa333333333333333333333333333333333333" // unrecorded
	fxHashM        = "aaaa555555555555555555555555555555555555" // feat/done merge commit
)

var fxProjectHash = schema.ProjectHash(testutil.TestProjectHash)

// fxBase is the fixture time origin (Unix ms).
func fxBase() int64 { return testutil.TestSessionStartTime.UnixMilli() }

// --- store seeding -----------------------------------------------------

// seedSession inserts a session row (plus project/host dimensions) with the
// fixture project hash and cwd.
func seedSession(t *testing.T, s *store.Store, sessionID, gitBranch string, startMs, endMs int64) {
	t.Helper()
	ingested := endMs + 1
	meta := &schema.UnifiedMetadata{
		SessionID:    schema.SessionID(sessionID),
		ModelHarness: ingest.HarnessClaudeCode,
		Model:        testutil.TestModel,
		HostSlug:     schema.HostSlug(testutil.TestHostSlug),
		Project: schema.ProjectContext{
			Hash:     schema.ProjectHash(fxProjectHash),
			Name:     "repo",
			FilePath: fxCwd,
		},
		Timestamp: schema.TimestampInfo{Start: startMs, End: endMs, Ingested: &ingested},
		Source:    schema.SourceInfo{FilePath: "/src.jsonl", Format: schema.SourceFormatJSONL},
	}
	if gitBranch != "" {
		meta.Git.Branch = &gitBranch
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatalf("seedSession(%s): %v", sessionID, err)
	}
}

// entrySpec is a compact session-entry builder for fixtures.
type entrySpec struct {
	role    schema.Role
	typ     schema.EntryType
	depth   int
	ms      int64
	preview string
	tool    string // tool_names_csv
	file    string // absolute file path for tool_input
	isError bool
}

func userTurn(ms int64, preview string) entrySpec {
	return entrySpec{role: schema.RoleUser, typ: schema.EntryTypeText, ms: ms, preview: preview}
}

func assistantTurn(ms int64, isError bool) entrySpec {
	return entrySpec{role: schema.RoleAssistant, typ: schema.EntryTypeText, ms: ms, isError: isError}
}

func toolUse(ms int64, tool, absFile string) entrySpec {
	return entrySpec{role: schema.RoleAssistant, typ: schema.EntryTypeToolUse, depth: 1, ms: ms, tool: tool, file: absFile}
}

// seedEntries indexes the given entries for a session, in order.
func seedEntries(t *testing.T, s *store.Store, sessionID string, specs []entrySpec) {
	t.Helper()
	entries := make([]schema.SessionEntry, len(specs))
	for i, spec := range specs {
		ms := spec.ms
		e := schema.SessionEntry{
			SessionID:   schema.SessionID(sessionID),
			EntryIndex:  i,
			Harness:     ingest.HarnessClaudeCode,
			EntryType:   spec.typ,
			Role:        spec.role,
			Depth:       spec.depth,
			TimestampMs: &ms,
			IsError:     spec.isError,
		}
		if spec.preview != "" {
			preview := spec.preview
			e.ContentPreview = &preview
		}
		if spec.tool != "" {
			tool := spec.tool
			e.ToolNamesCSV = &tool
			e.HasToolUse = true
			input, err := json.Marshal(map[string]string{"file_path": spec.file})
			if err != nil {
				t.Fatalf("seedEntries: marshal tool input: %v", err)
			}
			toolInput := string(input)
			e.ToolInput = &toolInput
		}
		entries[i] = e
	}
	if err := s.IndexSessionEntries(context.Background(), schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("seedEntries(%s): %v", sessionID, err)
	}
}

// seedMetrics saves a session_metrics row with the codemap-relevant fields.
func seedMetrics(t *testing.T, s *store.Store, sessionID, title string, outcome schema.SessionOutcome, outputTokens, retryLoops int, cost *float64) {
	t.Helper()
	computeVersion := 1
	m := &ingest.SessionMetrics{SessionID: schema.SessionID(sessionID)}
	m.ComputeVersion = &computeVersion
	if title != "" {
		m.TitleGenerated = &title
	}
	if outcome != "" {
		m.Outcome = &outcome
	}
	m.OutputTokens = &outputTokens
	m.RetryLoops = &retryLoops
	m.CostTotalUSD = cost
	if err := s.SaveMetrics(context.Background(), m); err != nil {
		t.Fatalf("seedMetrics(%s): %v", sessionID, err)
	}
}

// seedCommit links a commit to a session.
func seedCommit(t *testing.T, s *store.Store, sessionID, hash string, timeMs int64) {
	t.Helper()
	err := s.UpsertSessionCommits(context.Background(), schema.SessionID(sessionID), []ingest.CommitInfo{
		{Hash: hash, Message: "subject for " + hash[:8], AuthorEmail: testutil.TestEmail, CommitTime: timeMs},
	})
	if err != nil {
		t.Fatalf("seedCommit(%s, %s): %v", sessionID, hash, err)
	}
}

// seedSessionAnnotation creates a session-level annotation through one of
// the migration-seeded annotators ("outcome-classifier" rule / "human-web"
// human) using the quality.session_outcome seed type.
func seedSessionAnnotation(t *testing.T, s *store.Store, sessionID, annotatorName, value string) {
	t.Helper()
	seedSessionAnnotationOfType(t, s, sessionID, annotatorName, testutil.TestTypeIDSessionOutcome, value)
}

// seedSessionAnnotationOfType is seedSessionAnnotation with an explicit
// migration-seeded annotation type (e.g. metadata.session_scope).
func seedSessionAnnotationOfType(t *testing.T, s *store.Store, sessionID, annotatorName, typeID, value string) {
	t.Helper()
	ctx := context.Background()
	annotator, err := s.GetAnnotator(ctx, annotatorName)
	if err != nil || annotator == nil {
		t.Fatalf("seedSessionAnnotation: annotator %q: %v", annotatorName, err)
	}
	typeRow, err := s.GetAnnotationTypeByTypeID(ctx, typeID)
	if err != nil || typeRow == nil {
		t.Fatalf("seedSessionAnnotation: type %q: %v", typeID, err)
	}
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotator.ID,
		AnnotationTypeID: typeRow.ID,
		Value:            value,
	}); err != nil {
		t.Fatalf("seedSessionAnnotation(%s, %s=%s): %v", sessionID, annotatorName, value, err)
	}
}

// --- standard activity fixture -----------------------------------------

// Long user preview to exercise word-boundary title truncation: 100+ chars,
// with a space straddling position 80.
const fxLongPreview = "Refactor the ingest pipeline so the diff classifier stops marking unchanged sessions as updated forever"

// seedActivityFixture seeds the canonical two-session activity scenario:
//
//	session 1 (newest, base+3000..base+4000):
//	  task@0  "Add caching to the ingest pipeline" — edits internal/a/a.go,
//	          reads internal/b/b.go
//	  task@4  fxLongPreview — edits internal/a/a.go AND internal/b/b.go
//	          (co-edit pair occurrence #1); one edit outside the repo is
//	          skipped
//	session 2 (older, base+1000..base+2000):
//	  task@0  "fix the flaky b tests" — edits a.go (error-adjacent), two
//	          consecutive depth-0 assistant errors (retry loop), edits a.go
//	          again (re-edit), edits b.go (co-edit pair occurrence #2), edits
//	          docs/notes.md (activity-only file)
func seedActivityFixture(t *testing.T, s *store.Store) {
	t.Helper()
	base := fxBase()

	seedSession(t, s, fxSession1, "", base+3000, base+4000)
	seedEntries(t, s, fxSession1, []entrySpec{
		userTurn(base+3000, "Add caching to the ingest pipeline"),
		assistantTurn(base+3100, false),
		toolUse(base+3200, "Edit", fxCwd+"/internal/a/a.go"),
		toolUse(base+3300, "Read", fxCwd+"/internal/b/b.go"),
		userTurn(base+3500, fxLongPreview),
		assistantTurn(base+3600, false),
		toolUse(base+3700, "Edit", fxCwd+"/internal/a/a.go"),
		toolUse(base+3800, "Write", fxCwd+"/internal/b/b.go"),
		toolUse(base+3900, "Edit", "/elsewhere/x.go"), // outside the repo: skipped
	})
	seedMetrics(t, s, fxSession1, "Session one title", schema.OutcomeResolved, 1000, 0, ptrFloat(1.5))

	seedSession(t, s, fxSession2, "", base+1000, base+2000)
	seedEntries(t, s, fxSession2, []entrySpec{
		userTurn(base+1000, "fix the flaky b tests"),
		assistantTurn(base+1100, false),
		toolUse(base+1200, "Edit", fxCwd+"/internal/a/a.go"), // error-adjacent (errors at 3,4)
		assistantTurn(base+1300, true),
		assistantTurn(base+1400, true),                       // consecutive errors => retry loop
		toolUse(base+1500, "Edit", fxCwd+"/internal/a/a.go"), // re-edit within session
		toolUse(base+1600, "Write", fxCwd+"/internal/b/b.go"),
		toolUse(base+1700, "Edit", fxCwd+"/docs/notes.md"), // activity-only node
	})
	seedMetrics(t, s, fxSession2, "", schema.OutcomePartial, 500, 2, nil)
}

// --- stub repository ---------------------------------------------------

// Fixture file contents per ref. The head (feat/x) adds internal/c and a new
// import a -> c; the merge-base matches HEAD.
const (
	fxGoMod   = "module example.com/proj\n"
	fxFileA   = "package a\n\nimport _ \"example.com/proj/internal/b\"\n"
	fxFileAv2 = "package a\n\nimport (\n\t_ \"example.com/proj/internal/b\"\n\t_ \"example.com/proj/internal/c\"\n)\n"
	fxFileB   = "package b\n"
	fxFileC   = "package c\n"
)

// fxStubRepo builds the canonical stub repository:
//
//	HEAD / merge-base: go.mod, internal/a/a.go (imports b), internal/b/b.go,
//	                   docs/readme.md (tracked, unparsed)
//	feat/x:            + internal/c/c.go, a.go also imports c
func fxStubRepo() *testutil.StubGitRepository {
	base := fxBase()
	baseFiles := []string{"go.mod", "internal/a/a.go", "internal/b/b.go", "docs/readme.md"}
	headFiles := []string{"go.mod", "internal/a/a.go", "internal/b/b.go", "internal/c/c.go", "docs/readme.md"}

	contentsAt := func(ref string, aGo string, withC bool) map[string][]byte {
		m := map[string][]byte{
			ref + ":go.mod":          []byte(fxGoMod),
			ref + ":internal/a/a.go": []byte(aGo),
			ref + ":internal/b/b.go": []byte(fxFileB),
		}
		if withC {
			m[ref+":internal/c/c.go"] = []byte(fxFileC)
		}
		return m
	}

	contents := map[string][]byte{}
	for k, v := range contentsAt("HEAD", fxFileA, false) {
		contents[k] = v
	}
	for k, v := range contentsAt(fxMergeBase, fxFileA, false) {
		contents[k] = v
	}
	for k, v := range contentsAt(fxBranch, fxFileAv2, true) {
		contents[k] = v
	}

	return &testutil.StubGitRepository{
		DefaultBranchName: testutil.TestDefaultBranch,
		BranchList:        []string{fxBranch},
		BranchStates: map[string]*gitops.BranchState{
			fxBranch: {
				Name:        fxBranch,
				MergeBase:   fxMergeBase,
				AheadCount:  3,
				BehindCount: 0,
				ChangedFiles: []gitops.FileChange{
					{Path: "internal/a/a.go", Status: gitops.FileStatusModified},
					{Path: "internal/c/c.go", Status: gitops.FileStatusAdded},
				},
			},
		},
		Merged: []gitops.MergedBranch{
			{Name: fxMergedBranch, MergedAtMs: base + 9000, MergeCommit: fxHashM},
		},
		FileContents: contents,
		FilesByRef: map[string][]string{
			"HEAD":                     baseFiles,
			fxMergeBase:                baseFiles,
			fxBranch:                   headFiles,
			testutil.TestDefaultBranch: baseFiles,
		},
		CommitsByRef: map[string][]gitops.Commit{
			testutil.TestDefaultBranch: {
				{Hash: fxMergeBase, Subject: "base commit", TimeMs: base, AuthorEmail: testutil.TestEmail},
			},
			fxBranch: {
				{Hash: fxHashC, Subject: "unrecorded tweak", TimeMs: base + 5000, AuthorEmail: testutil.TestEmail},
				{Hash: fxHashB, Subject: "more feature work", TimeMs: base + 2500, AuthorEmail: testutil.TestEmail},
				{Hash: fxHashA, Subject: "feature work", TimeMs: base + 3500, AuthorEmail: testutil.TestEmail},
				{Hash: fxMergeBase, Subject: "base commit", TimeMs: base, AuthorEmail: testutil.TestEmail},
			},
		},
		RangeHashes: map[string][]string{
			fxMergeBase + ".." + fxBranch: {fxHashC, fxHashB, fxHashA},
		},
		// One row per ChangedFiles entry; totals are the column sums.
		DiffStatsResult: &gitops.DiffStats{
			LinesAdded:   12,
			LinesRemoved: 3,
			PerFile: []gitops.FileDiffStat{
				{Path: "internal/a/a.go", Added: 4, Removed: 3},
				{Path: "internal/c/c.go", Added: 8},
			},
		},
	}
}

// noRepo returns a stub where every git call errors (canonical_cwd is not a
// git repository).
func noRepo() *testutil.StubGitRepository { return testutil.NoGitRepository() }

// newFixtureService seeds a store with the activity fixture and wires a
// Service around the given repository stub.
func newFixtureService(t *testing.T, repo gitops.Repository) (*codemap.Service, *store.Store) {
	t.Helper()
	s := storetest.Open(t)
	seedActivityFixture(t, s)
	svc := codemap.NewService(s, func(string) gitops.Repository { return repo }, codegraph.NewGraphBuilder(), sessionvisibility.All())
	return svc, s
}

// --- assertion helpers --------------------------------------------------

func ptrFloat(v float64) *float64 { return &v }

// findNode returns the node with the given ID, failing the test if absent.
func findNode(t *testing.T, nodes []schema.MapNode, id string) schema.MapNode {
	t.Helper()
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not found in %d nodes", id, len(nodes))
	return schema.MapNode{}
}

// assertNoNullArrays marshals v and asserts the JSON never contains a null —
// the contract's "slices never nil on marshal" guarantee (pointer fields are
// all omitempty, so any null is a nil slice).
func assertNoNullArrays(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("payload JSON contains null (nil slice?): %s", b)
	}
}

// assertCommitRefCoherent asserts the HasSession/SessionIDs coherence
// invariant (HasSession true iff SessionIDs is non-empty) for every ref.
// schema.ReviewListPayload.Validate() enforces this for review.go's main
// timeline producer (see TestReviewChanges_TimelineBindings); MapNodeDetailPayload
// has no equivalent Validate() method, so detail.go's commitsForSessions needs
// this explicit check instead. Shared by timeline_test.go and detail_test.go
// so the two independent CommitRef producers can't drift on what "coherent"
// means.
func assertCommitRefCoherent(t *testing.T, refs []schema.CommitRef) {
	t.Helper()
	for _, ref := range refs {
		if want := len(ref.SessionIDs) > 0; ref.HasSession != want {
			t.Errorf("commit %s: HasSession = %t, want %t (len(SessionIDs) = %d)",
				ref.Hash, ref.HasSession, want, len(ref.SessionIDs))
		}
	}
}
