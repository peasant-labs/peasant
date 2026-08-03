package codemap_test

import (
	"context"
	"reflect"
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

// Second-project fixture for the multi-project picker scenarios. Ordering is
// by display name (the canonical cwd), and "/a-repo" < "/repo".
const (
	fxProjectHash2 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	fxCwd2         = "/a-repo"
	fxSession5     = "55555555-5555-5555-5555-555555555555"
)

// seedSecondProject seeds a second project (cwd /a-repo) with one session
// that edits a single file, so it gets its own picker row.
func seedSecondProject(t *testing.T, s *store.Store) {
	t.Helper()
	base := fxBase()
	seedSessionForProject(t, s, fxSession5, fxProjectHash2, fxCwd2, base+8000, base+9000)
	seedEntries(t, s, fxSession5, []entrySpec{
		userTurn(base+8000, "Write the deployment runbook for the new service"),
		assistantTurn(base+8100, false),
		toolUse(base+8200, "Write", fxCwd2+"/docs/runbook.md"),
	})
}

// TestProjectSummaries_RepoFound: the fixture project gets one row with
// session count, last work, coverage at HEAD (universe = tracked ∪ edited,
// recorded = tracked ∧ edited), and the open-branch count (branch list minus
// the merged set).
func TestProjectSummaries_RepoFound(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("ProjectSummaries: %v", err)
	}

	if len(payload.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(payload.Projects))
	}
	row := payload.Projects[0]
	if row.ProjectHash != fxProjectHash {
		t.Errorf("ProjectHash = %q, want %q", row.ProjectHash, fxProjectHash)
	}
	if row.Project != fxCwd {
		t.Errorf("Project = %q, want %q (display name = canonical cwd)", row.Project, fxCwd)
	}
	if row.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", row.Sessions)
	}
	// Last work = the newest session's end (base+4000).
	wantLast := fxBase() + 4000
	if row.LastWorkMs == nil || *row.LastWorkMs != wantLast {
		t.Errorf("LastWorkMs = %v, want %d", row.LastWorkMs, wantLast)
	}
	// Universe: 4 tracked files (go.mod, a.go, b.go, readme.md) + the
	// untracked edited docs/notes.md = 5. Recorded: tracked AND edited =
	// a.go + b.go = 2 (same rule as the map's root roll-up).
	if row.TotalFiles != 5 || row.RecordedFiles != 2 {
		t.Errorf("coverage = %d/%d, want 2/5", row.RecordedFiles, row.TotalFiles)
	}
	// Open changes: BranchList has feat/x; the merged set (feat/done) does
	// not contain it => 1.
	if row.OpenChanges != 1 {
		t.Errorf("OpenChanges = %d, want 1", row.OpenChanges)
	}

	assertNoNullArrays(t, payload)
}

// TestProjectSummaries_RepoMissing: a project whose cwd is not a git repo
// falls back to recorded-edit-only coverage (recorded == total == edited
// files) and reports zero open changes.
func TestProjectSummaries_RepoMissing(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, noRepo())

	payload, err := svc.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("ProjectSummaries: %v", err)
	}

	if len(payload.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(payload.Projects))
	}
	row := payload.Projects[0]
	if row.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", row.Sessions)
	}
	// Edited files: internal/a/a.go, internal/b/b.go, docs/notes.md.
	if row.TotalFiles != 3 || row.RecordedFiles != 3 {
		t.Errorf("fallback coverage = %d/%d, want 3/3", row.RecordedFiles, row.TotalFiles)
	}
	if row.OpenChanges != 0 {
		t.Errorf("OpenChanges = %d, want 0 without a repo", row.OpenChanges)
	}

	assertNoNullArrays(t, payload)
}

// TestProjectSummaries_MergedBranchesSubtracted: a branch present in BOTH the
// local branch list and the merged set is not an open change (fully-merged
// branches keep their local ref).
func TestProjectSummaries_MergedBranchesSubtracted(t *testing.T) {
	t.Parallel()
	repo := fxStubRepo()
	repo.BranchList = []string{fxBranch, fxMergedBranch} // merged ref still local
	svc, _ := newFixtureService(t, repo)

	payload, err := svc.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("ProjectSummaries: %v", err)
	}
	if len(payload.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(payload.Projects))
	}
	if got := payload.Projects[0].OpenChanges; got != 1 {
		t.Errorf("OpenChanges = %d, want 1 (merged branch subtracted)", got)
	}
}

// TestProjectSummaries_OrderedByName: rows are ordered by display name, and
// per-project numbers don't bleed across projects.
func TestProjectSummaries_OrderedByName(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	seedSecondProject(t, s)

	payload, err := svc.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("ProjectSummaries: %v", err)
	}

	if len(payload.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(payload.Projects))
	}
	gotNames := []string{payload.Projects[0].Project, payload.Projects[1].Project}
	if want := []string{fxCwd2, fxCwd}; !reflect.DeepEqual(gotNames, want) {
		t.Errorf("project order = %v, want %v", gotNames, want)
	}

	second := payload.Projects[0] // /a-repo
	if second.ProjectHash != fxProjectHash2 {
		t.Errorf("second project hash = %q, want %q", second.ProjectHash, fxProjectHash2)
	}
	if second.Sessions != 1 {
		t.Errorf("second project Sessions = %d, want 1", second.Sessions)
	}
	wantLast := fxBase() + 9000
	if second.LastWorkMs == nil || *second.LastWorkMs != wantLast {
		t.Errorf("second project LastWorkMs = %v, want %d", second.LastWorkMs, wantLast)
	}
	// The shared stub repo serves the same tracked files for both cwds; the
	// second project's recorded edit (docs/runbook.md) is untracked there, so
	// it widens the universe without being recorded.
	if second.RecordedFiles != 0 {
		t.Errorf("second project RecordedFiles = %d, want 0", second.RecordedFiles)
	}

	assertNoNullArrays(t, payload)
}

// TestProjectSummaries_Deterministic: identical inputs produce identical
// payloads (there is no clock-derived field on this payload).
func TestProjectSummaries_Deterministic(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	seedSecondProject(t, s)

	first, err := svc.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("ProjectSummaries #1: %v", err)
	}
	second, err := svc.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("ProjectSummaries #2: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("ProjectSummaries not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}
}

// TestProjectSummaries_EmptyStore: a store without projects serves an empty
// (never-nil) row list.
func TestProjectSummaries_EmptyStore(t *testing.T) {
	t.Parallel()
	svc := newEmptyService(t)

	payload, err := svc.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("ProjectSummaries: %v", err)
	}
	if len(payload.Projects) != 0 {
		t.Errorf("projects = %d, want 0", len(payload.Projects))
	}
	assertNoNullArrays(t, payload)
}

// newEmptyService wires a Service around an unseeded store.
func newEmptyService(t *testing.T) *codemap.Service {
	t.Helper()
	s := storetest.Open(t)
	return codemap.NewService(s, func(string) gitops.Repository { return noRepo() }, codegraph.NewGraphBuilder(), sessionvisibility.All())
}

// seedSessionForProject inserts a session row under an arbitrary project
// (hash + cwd) — the multi-project variant of seedSession.
func seedSessionForProject(t *testing.T, s *store.Store, sessionID, projectHash, cwd string, startMs, endMs int64) {
	t.Helper()
	seedSessionForProjectWithRemote(t, s, sessionID, projectHash, cwd, "", startMs, endMs)
}

// seedSessionForProjectWithRemote is seedSessionForProject plus an optional
// git remote URL (as a repo would report it, e.g. "https://github.com/owner/repo.git");
// empty means no remote configured (the path-fallback case).
func seedSessionForProjectWithRemote(t *testing.T, s *store.Store, sessionID, projectHash, cwd, remote string, startMs, endMs int64) {
	t.Helper()
	ingested := endMs + 1
	meta := &schema.UnifiedMetadata{
		SessionID:    schema.SessionID(sessionID),
		ModelHarness: ingest.HarnessClaudeCode,
		Model:        testutil.TestModel,
		HostSlug:     schema.HostSlug(testutil.TestHostSlug),
		Project: schema.ProjectContext{
			Hash:     schema.ProjectHash(projectHash),
			Name:     "second",
			FilePath: cwd,
		},
		Timestamp: schema.TimestampInfo{Start: startMs, End: endMs, Ingested: &ingested},
		Source:    schema.SourceInfo{FilePath: "/src.jsonl", Format: schema.SourceFormatJSONL},
	}
	if remote != "" {
		meta.Git.Remote = &remote
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatalf("seedSessionForProject(%s): %v", sessionID, err)
	}
}

// TestProjectSummaries_DisplayNamePrefersRemote: a project with a configured
// git remote displays as "host:owner/repo", not its canonical_cwd path —
// the confirmed regression where the user saw a
// project literally titled "12", a bare path segment, instead of a
// recognizable remote-derived name).
func TestProjectSummaries_DisplayNamePrefersRemote(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	base := fxBase()
	seedSessionForProjectWithRemote(t, s, fxSession5, fxProjectHash2, fxCwd2, "https://github.com/example-org/garden-app.git", base+8000, base+9000)

	result, err := svc.ProjectSummaries(context.Background())
	if err != nil {
		t.Fatalf("ProjectSummaries: %v", err)
	}
	if len(result.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(result.Projects))
	}
	var remoteRow *schema.ProjectSummary
	for i := range result.Projects {
		if result.Projects[i].ProjectHash == fxProjectHash2 {
			remoteRow = &result.Projects[i]
		}
	}
	if remoteRow == nil {
		t.Fatalf("no row for %s in %+v", fxProjectHash2, result.Projects)
	}
	if remoteRow.Project != "github:example-org/garden-app" {
		t.Errorf("Project = %q, want %q (remote-derived display name)", remoteRow.Project, "github:example-org/garden-app")
	}
}
