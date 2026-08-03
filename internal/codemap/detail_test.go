package codemap_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// TestMapNodeDetail_Package: counts, shaped-by ordering, footnotes, and the
// session-linked recent commits for a package node.
func TestMapNodeDetail_Package(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	seedCommit(t, s, fxSession1, fxHashA, fxBase()+3500)

	payload, err := svc.MapNodeDetail(context.Background(), fxProjectHash, "internal/a")
	if err != nil {
		t.Fatalf("MapNodeDetail: %v", err)
	}

	if payload.Path != "internal/a" {
		t.Errorf("Path = %q", payload.Path)
	}

	// Structural role (5.6): internal/a imports internal/b at HEAD and nothing
	// imports it. Language carries the parsed language for the descriptor.
	if payload.Language != "go" {
		t.Errorf("Language = %q, want go", payload.Language)
	}
	if len(payload.DependsOn) != 1 || payload.DependsOn[0] != "internal/b" {
		t.Errorf("DependsOn = %v, want [internal/b]", payload.DependsOn)
	}
	if len(payload.UsedBy) != 0 {
		t.Errorf("UsedBy = %v, want [] (nothing imports internal/a)", payload.UsedBy)
	}
	// All three tasks edit a.go; two distinct sessions.
	if payload.TaskCount != 3 {
		t.Errorf("TaskCount = %d, want 3", payload.TaskCount)
	}
	if payload.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", payload.SessionCount)
	}
	if payload.TotalFiles != 1 || payload.RecordedFiles != 1 {
		t.Errorf("coverage = %d/%d, want 1/1", payload.RecordedFiles, payload.TotalFiles)
	}

	// Last touch under internal/a: session1 task2's edit at base+3700.
	if payload.LastTouchMs == nil || *payload.LastTouchMs != fxBase()+3700 {
		t.Errorf("LastTouchMs = %v, want %d", payload.LastTouchMs, fxBase()+3700)
	}

	// Shaped by: most recent first.
	if len(payload.ShapedBy) != 3 {
		t.Fatalf("len(ShapedBy) = %d, want 3", len(payload.ShapedBy))
	}
	if payload.ShapedBy[0].SessionID != fxSession1 || payload.ShapedBy[0].EntryIndex != 4 {
		t.Errorf("ShapedBy[0] = %s@%d, want %s@4",
			payload.ShapedBy[0].SessionID, payload.ShapedBy[0].EntryIndex, fxSession1)
	}
	if payload.ShapedBy[2].SessionID != fxSession2 {
		t.Errorf("ShapedBy[2].SessionID = %s, want %s", payload.ShapedBy[2].SessionID, fxSession2)
	}

	// Recent commits: session 1 touches the node and links fxHashA.
	if len(payload.RecentCommits) != 1 {
		t.Fatalf("len(RecentCommits) = %d, want 1", len(payload.RecentCommits))
	}
	if payload.RecentCommits[0].Hash != fxHashA || !payload.RecentCommits[0].HasSession {
		t.Errorf("RecentCommits[0] = %+v, want hash %s with HasSession", payload.RecentCommits[0], fxHashA)
	}

	// Footnotes: retry loops summed over touching sessions (0 + 2); re-edits
	// = files under the node re-edited within one session (a.go by session
	// 2); cost = the known costs summed (session 2's is unknown).
	if payload.RetryLoops != 2 {
		t.Errorf("RetryLoops = %d, want 2", payload.RetryLoops)
	}
	if payload.ReEdits != 1 {
		t.Errorf("ReEdits = %d, want 1", payload.ReEdits)
	}
	if payload.CostUsd == nil || *payload.CostUsd != 1.5 {
		t.Errorf("CostUsd = %v, want 1.5", payload.CostUsd)
	}

	assertNoNullArrays(t, payload)
}

// TestMapNodeDetail_Connections covers the structural-role direction (5.6):
// internal/b is imported by internal/a and imports nothing — the mirror of the
// package test's internal/a.
func TestMapNodeDetail_Connections(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.MapNodeDetail(context.Background(), fxProjectHash, "internal/b")
	if err != nil {
		t.Fatalf("MapNodeDetail(internal/b): %v", err)
	}
	if len(payload.DependsOn) != 0 {
		t.Errorf("DependsOn = %v, want [] (internal/b imports nothing)", payload.DependsOn)
	}
	if len(payload.UsedBy) != 1 || payload.UsedBy[0] != "internal/a" {
		t.Errorf("UsedBy = %v, want [internal/a]", payload.UsedBy)
	}
	assertNoNullArrays(t, payload)
}

// TestMapNodeDetail_ActivityOnlyNode: nodes outside the parsed graph (e.g.
// docs) are addressable too.
func TestMapNodeDetail_ActivityOnlyNode(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	payload, err := svc.MapNodeDetail(context.Background(), fxProjectHash, "docs/notes.md")
	if err != nil {
		t.Fatalf("MapNodeDetail(docs/notes.md): %v", err)
	}
	if payload.TaskCount != 1 || payload.SessionCount != 1 {
		t.Errorf("tasks/sessions = %d/%d, want 1/1", payload.TaskCount, payload.SessionCount)
	}
	if len(payload.ShapedBy) != 1 || payload.ShapedBy[0].SessionID != fxSession2 {
		t.Errorf("ShapedBy = %+v, want session2's task", payload.ShapedBy)
	}
}

// TestMapNodeDetail_RecentCommitsCoherence guards commitsForSessions'
// HasSession/SessionIDs invariant (detail.go:171) on real MapNodeDetail
// output — the node-detail rail's analog of TestReviewChanges_TimelineBindings
// for the main Review timeline's review.go producer (internal/codemap#TestReviewChanges_TimelineBindings
// asserts the same invariant, via payload.Validate(), for the sibling producer).
//
// internal/a is touched by both session 1 and session 2 (seedActivityFixture).
// Linking a session-1-only commit, a session-2-only commit, and a commit
// shared by both exercises the shape a silent sessionsByHash/byHash desync
// would break: a session attributed to the wrong commit, or a shared commit
// collapsing to only one contributing session, surfaces as a wrong SessionIDs
// set below even though the HasSession/SessionIDs coherence check alone would
// still read true for either case.
//
// commitsForSessions has no off-window analog to review.go's bounded
// default-branch commit window (see detail.go:56-60): every commit it can
// return was discovered by walking pd.commitsByID for a session already in
// sessionSet, so on real output SessionIDs can never be empty and HasSession
// is always true by construction — there is no reachable "commit outside the
// window" case to fixture here. assertCommitRefCoherent still guards the
// computation itself (see its doc comment + the negative-control commit
// message for the mutation proof that it is not vacuous despite that).
func TestMapNodeDetail_RecentCommitsCoherence(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	const (
		fxHashSession2Only = "bbbb111111111111111111111111111111111111"
		fxHashShared       = "cccc111111111111111111111111111111111111"
	)
	// UpsertSessionCommits atomically replaces a session's whole commit set,
	// so each session's full link list is seeded in one call (unlike
	// seedCommit, which would clobber an earlier call for the same session).
	if err := s.UpsertSessionCommits(context.Background(), schema.SessionID(fxSession1), []ingest.CommitInfo{
		{Hash: fxHashA, Message: "subject for " + fxHashA[:8], CommitTime: fxBase() + 3500},
		{Hash: fxHashShared, Message: "subject for " + fxHashShared[:8], CommitTime: fxBase() + 2800},
	}); err != nil {
		t.Fatalf("seed session-one commits: %v", err)
	}
	if err := s.UpsertSessionCommits(context.Background(), schema.SessionID(fxSession2), []ingest.CommitInfo{
		{Hash: fxHashSession2Only, Message: "subject for " + fxHashSession2Only[:8], CommitTime: fxBase() + 3000},
		{Hash: fxHashShared, Message: "subject for " + fxHashShared[:8], CommitTime: fxBase() + 2800},
	}); err != nil {
		t.Fatalf("seed session-two commits: %v", err)
	}

	payload, err := svc.MapNodeDetail(context.Background(), fxProjectHash, "internal/a")
	if err != nil {
		t.Fatalf("MapNodeDetail: %v", err)
	}
	assertCommitRefCoherent(t, payload.RecentCommits)

	if len(payload.RecentCommits) != 3 {
		t.Fatalf("len(RecentCommits) = %d, want 3", len(payload.RecentCommits))
	}
	byHash := make(map[string][]string, len(payload.RecentCommits))
	for _, ref := range payload.RecentCommits {
		ids := make([]string, len(ref.SessionIDs))
		for i, id := range ref.SessionIDs {
			ids[i] = string(id)
		}
		byHash[ref.Hash] = ids
	}
	if got := byHash[fxHashA]; !reflect.DeepEqual(got, []string{fxSession1}) {
		t.Errorf("%s sessions = %v, want [%s]", fxHashA, got, fxSession1)
	}
	if got := byHash[fxHashSession2Only]; !reflect.DeepEqual(got, []string{fxSession2}) {
		t.Errorf("%s sessions = %v, want [%s]", fxHashSession2Only, got, fxSession2)
	}
	if got := byHash[fxHashShared]; !reflect.DeepEqual(got, []string{fxSession1, fxSession2}) {
		t.Errorf("%s sessions = %v, want [%s %s]", fxHashShared, got, fxSession1, fxSession2)
	}
}

// TestMapNodeDetail_NotFound: unknown node paths error with ErrNodeNotFound.
func TestMapNodeDetail_NotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())

	_, err := svc.MapNodeDetail(context.Background(), fxProjectHash, "no/such/node.go")
	if !errors.Is(err, codemap.ErrNodeNotFound) {
		t.Errorf("MapNodeDetail(unknown) error = %v, want ErrNodeNotFound", err)
	}
}
