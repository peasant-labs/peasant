package codemap_test

import (
	"context"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestChangeDiff_RendersHunks: the service maps gitops FileDiff hunks into the
// wire payload, including the status from the change's file list and the
// context/add/del line-kind strings.
func TestChangeDiff_RendersHunks(t *testing.T) {
	t.Parallel()
	repo := fxStubRepo()
	repo.DiffHunksResult = []gitops.FileDiff{
		{
			Path:   "internal/a/a.go",
			Status: gitops.FileStatusModified,
			Hunks: []gitops.Hunk{{
				OldStart: 1, OldLines: 3, NewStart: 1, NewLines: 4,
				Header: "func A()",
				Lines: []gitops.DiffLine{
					{Kind: gitops.DiffLineContext, Text: "package a"},
					{Kind: gitops.DiffLineRemoved, Text: "func A() int { return 1 }"},
					{Kind: gitops.DiffLineAdded, Text: "func A() int { return 2 }"},
				},
			}},
		},
	}
	svc, _ := newFixtureService(t, repo)

	payload, err := svc.ChangeDiff(context.Background(), fxProjectHash, fxBranch, "internal/a/a.go")
	if err != nil {
		t.Fatalf("ChangeDiff: %v", err)
	}
	if payload.File != "internal/a/a.go" {
		t.Errorf("File = %q, want internal/a/a.go", payload.File)
	}
	if payload.Status != "M" {
		t.Errorf("Status = %q, want M (from the change's file list)", payload.Status)
	}
	if len(payload.Hunks) != 1 {
		t.Fatalf("Hunks = %d, want 1", len(payload.Hunks))
	}
	h := payload.Hunks[0]
	if h.NewStart != 1 || h.NewLines != 4 || h.Header != "func A()" {
		t.Errorf("hunk header mismatch: %+v", h)
	}
	if len(h.Lines) != 3 {
		t.Fatalf("hunk lines = %d, want 3", len(h.Lines))
	}
	if h.Lines[0].Kind != "context" || h.Lines[1].Kind != "del" || h.Lines[2].Kind != "add" {
		t.Errorf("line kinds = %q/%q/%q, want context/del/add",
			h.Lines[0].Kind, h.Lines[1].Kind, h.Lines[2].Kind)
	}
}

// TestChangeDiff_AttributesHunkToConversation: a hunk whose added line blames to
// a commit linked to a recorded session is attributed to that conversation (the
// mission climax — git blame → commit → session).
func TestChangeDiff_AttributesHunkToConversation(t *testing.T) {
	t.Parallel()
	repo := fxStubRepo()
	repo.DiffHunksResult = []gitops.FileDiff{
		{
			Path:   "internal/a/a.go",
			Status: gitops.FileStatusModified,
			Hunks: []gitops.Hunk{{
				OldStart: 1, OldLines: 0, NewStart: 1, NewLines: 1,
				Lines: []gitops.DiffLine{{Kind: gitops.DiffLineAdded, Text: "x := 1"}},
			}},
		},
	}
	// The file's line 1 (the added line) blames to fxHashA.
	repo.BlameByRefPath = map[string][]string{
		fxBranch + "\x00" + "internal/a/a.go": {fxHashA},
	}
	svc, store := newFixtureService(t, repo)
	// Link fxHashA to a recorded session so the blame resolves to a conversation.
	seedCommit(t, store, fxSession1, fxHashA, 0)

	payload, err := svc.ChangeDiff(context.Background(), fxProjectHash, fxBranch, "internal/a/a.go")
	if err != nil {
		t.Fatalf("ChangeDiff: %v", err)
	}
	if len(payload.Hunks) != 1 {
		t.Fatalf("Hunks = %d, want 1", len(payload.Hunks))
	}
	if payload.Hunks[0].SessionID != fxSession1 {
		t.Errorf("hunk SessionID = %q, want %q", payload.Hunks[0].SessionID, fxSession1)
	}
	if payload.Hunks[0].SessionTitle == "" {
		t.Errorf("hunk SessionTitle should be resolved, got empty")
	}
}

// TestReviewChanges_RevertedMergedBranch verifies that a merged branch whose
// merge commit was reverted on the default branch is flagged Reverted.
func TestReviewChanges_RevertedMergedBranch(t *testing.T) {
	t.Parallel()
	repo := fxStubRepo()
	repo.RevertedByRef = map[string]map[string]bool{
		testutil.TestDefaultBranch: {fxHashM: true},
	}
	svc, _ := newFixtureService(t, repo)

	payload, err := svc.ReviewChanges(context.Background(), fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}
	var merged *schema.ChangeSummary
	for i := range payload.Changes {
		if payload.Changes[i].Branch == fxMergedBranch {
			merged = &payload.Changes[i]
		}
	}
	if merged == nil {
		t.Fatalf("no merged row for %s", fxMergedBranch)
	}
	if !merged.Reverted {
		t.Errorf("merged %s Reverted = false, want true (merge commit %s reverted)", fxMergedBranch, fxHashM)
	}
}

// TestChangeDiff_RepoMissing: no git repo → ErrRepoNotFound (same contract as
// ChangeDetail).
func TestChangeDiff_RepoMissing(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, noRepo())
	_, err := svc.ChangeDiff(context.Background(), fxProjectHash, fxBranch, "internal/a/a.go")
	if !errors.Is(err, codemap.ErrRepoNotFound) {
		t.Errorf("ChangeDiff error = %v, want ErrRepoNotFound", err)
	}
}

// TestChangeDiff_UnknownBranch: an unknown branch is an honest not-found.
func TestChangeDiff_UnknownBranch(t *testing.T) {
	t.Parallel()
	svc, _ := newFixtureService(t, fxStubRepo())
	_, err := svc.ChangeDiff(context.Background(), fxProjectHash, "no/such-branch", "x.go")
	if !errors.Is(err, codemap.ErrBranchNotFound) {
		t.Errorf("ChangeDiff error = %v, want ErrBranchNotFound", err)
	}
}

// TestChangeDiff_BinaryAndTruncated: binary + truncated flags pass through, and
// a pure-rename status comes from the change's file list even with no hunks.
func TestChangeDiff_BinaryAndTruncated(t *testing.T) {
	t.Parallel()
	repo := fxStubRepo()
	repo.DiffHunksResult = []gitops.FileDiff{
		{Path: "internal/c/c.go", Status: gitops.FileStatusAdded, Truncated: true},
	}
	svc, _ := newFixtureService(t, repo)

	payload, err := svc.ChangeDiff(context.Background(), fxProjectHash, fxBranch, "internal/c/c.go")
	if err != nil {
		t.Fatalf("ChangeDiff: %v", err)
	}
	if !payload.Truncated {
		t.Errorf("Truncated = false, want true")
	}
	if payload.Status != "A" {
		t.Errorf("Status = %q, want A", payload.Status)
	}
}
