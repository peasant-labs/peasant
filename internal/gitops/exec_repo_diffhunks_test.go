package gitops

import (
	"context"
	"strings"
	"testing"
)

// diffFileByPath finds the FileDiff for path, or fails the test.
func diffFileByPath(t *testing.T, files []FileDiff, path string) FileDiff {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no FileDiff for %q in %d files", path, len(files))
	return FileDiff{}
}

// countKind sums the lines of a kind across a file's hunks.
func countKind(f FileDiff, kind DiffLineKind) int {
	n := 0
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == kind {
				n++
			}
		}
	}
	return n
}

// TestExecGitRepository_DiffHunks covers the four modeled statuses (M/A/D/R),
// hunk structure, three-dot (merge-base) scoping, the paths filter and the
// per-file line cap — all against a real throwaway repo.
func TestExecGitRepository_DiffHunks(t *testing.T) {
	dir := initTestRepo(t)
	// Base files on main, before the branch forks.
	writeAndCommit(t, dir, "keep.go", "package keep\n\nfunc A() int { return 1 }\n", "add keep")
	writeAndCommit(t, dir, "gone.go", "package gone\n", "add gone")
	writeAndCommit(t, dir, "old.go", "package old\n", "add old")

	// Fork a feature branch and make all four kinds of change.
	mustGit(t, dir, "git", "checkout", "-b", "feat/diff")
	writeAndCommit(t, dir, "keep.go", "package keep\n\nfunc A() int { return 2 }\n", "modify keep")
	writeAndCommit(t, dir, "added.go", "package added\n\nfunc New() {}\n", "add new file")
	mustGit(t, dir, "git", "rm", "gone.go")
	mustGit(t, dir, "git", "commit", "-m", "delete gone")
	mustGit(t, dir, "git", "mv", "old.go", "renamed.go")
	mustGit(t, dir, "git", "commit", "-m", "rename old")

	// A main-only change AFTER the fork must NOT appear in base...head (three-dot).
	mustGit(t, dir, "git", "checkout", testDefaultBranchName)
	writeAndCommit(t, dir, "mainonly.go", "package mainonly\n", "main only")

	repo := NewExecGitRepository(dir)
	files, err := repo.DiffHunks(context.Background(), testDefaultBranchName, "feat/diff", nil, 0)
	if err != nil {
		t.Fatalf("DiffHunks: %v", err)
	}

	// Three-dot scoping: the main-only file must be absent.
	for _, f := range files {
		if f.Path == "mainonly.go" {
			t.Fatalf("main-only change leaked into base...head diff")
		}
	}

	// Modified.
	mod := diffFileByPath(t, files, "keep.go")
	if mod.Status != FileStatusModified {
		t.Errorf("keep.go status = %q, want M", mod.Status)
	}
	if countKind(mod, DiffLineAdded) != 1 || countKind(mod, DiffLineRemoved) != 1 {
		t.Errorf("keep.go: want 1 add + 1 del, got +%d/-%d", countKind(mod, DiffLineAdded), countKind(mod, DiffLineRemoved))
	}
	if len(mod.Hunks) == 0 || mod.Hunks[0].NewStart == 0 {
		t.Errorf("keep.go: expected a hunk with a NewStart, got %+v", mod.Hunks)
	}

	// Added.
	add := diffFileByPath(t, files, "added.go")
	if add.Status != FileStatusAdded {
		t.Errorf("added.go status = %q, want A", add.Status)
	}
	if countKind(add, DiffLineRemoved) != 0 || countKind(add, DiffLineAdded) == 0 {
		t.Errorf("added.go: want only additions, got +%d/-%d", countKind(add, DiffLineAdded), countKind(add, DiffLineRemoved))
	}

	// Deleted (path resolved from the --- header since +++ is /dev/null).
	del := diffFileByPath(t, files, "gone.go")
	if del.Status != FileStatusDeleted {
		t.Errorf("gone.go status = %q, want D", del.Status)
	}
	if countKind(del, DiffLineAdded) != 0 || countKind(del, DiffLineRemoved) == 0 {
		t.Errorf("gone.go: want only removals, got +%d/-%d", countKind(del, DiffLineAdded), countKind(del, DiffLineRemoved))
	}

	// Renamed.
	ren := diffFileByPath(t, files, "renamed.go")
	if ren.Status != FileStatusRenamed {
		t.Errorf("renamed.go status = %q, want R", ren.Status)
	}
	if ren.OldPath == nil || *ren.OldPath != "old.go" {
		t.Errorf("renamed.go OldPath = %v, want old.go", ren.OldPath)
	}
}

// TestExecGitRepository_DiffHunks_DashAndPlusContent guards the parser against
// in-hunk content lines whose text starts with "-- " or "++ " (e.g. SQL/Lua
// comments) — once the diff marker is prepended they read as "--- "/"+++ ",
// which must NOT be mistaken for file headers and dropped.
func TestExecGitRepository_DiffHunks_DashAndPlusContent(t *testing.T) {
	dir := initTestRepo(t)
	writeAndCommit(t, dir, "q.sql", "SELECT 1;\n", "base")
	mustGit(t, dir, "git", "checkout", "-b", "feat/sql")
	// New content: a "-- comment" line and a "++ marker" line, plus a kept line.
	writeAndCommit(t, dir, "q.sql", "SELECT 1;\n-- a comment line\n++ a plus line\n", "edit")

	repo := NewExecGitRepository(dir)
	files, err := repo.DiffHunks(context.Background(), testDefaultBranchName, "feat/sql", nil, 0)
	if err != nil {
		t.Fatalf("DiffHunks: %v", err)
	}
	f := diffFileByPath(t, files, "q.sql")
	added := 0
	var addedTexts []string
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == DiffLineAdded {
				added++
				addedTexts = append(addedTexts, l.Text)
			}
		}
	}
	// Both the "-- comment" and "++ marker" lines must survive as added lines.
	if added != 2 {
		t.Fatalf("added lines = %d (%q), want 2 (the -- and ++ content lines)", added, addedTexts)
	}
	joined := strings.Join(addedTexts, "|")
	if !strings.Contains(joined, "-- a comment line") || !strings.Contains(joined, "++ a plus line") {
		t.Errorf("added texts = %q, want both the -- and ++ content lines", addedTexts)
	}
}

// TestExecGitRepository_RevertedCommits detects the commit a `git revert` undid
// via the "This reverts commit <hash>" trailer.
func TestExecGitRepository_RevertedCommits(t *testing.T) {
	dir := initTestRepo(t)
	writeAndCommit(t, dir, "f.txt", "v1\n", "feature")
	target := headHash(t, dir)
	writeAndCommit(t, dir, "g.txt", "other\n", "unrelated")
	mustGit(t, dir, "git", "revert", "--no-edit", target)

	repo := NewExecGitRepository(dir)
	reverted, err := repo.RevertedCommits(context.Background(), testDefaultBranchName)
	if err != nil {
		t.Fatalf("RevertedCommits: %v", err)
	}
	// The revert trailer names the target (possibly abbreviated) → prefix match.
	found := false
	for h := range reverted {
		if strings.HasPrefix(target, h) || strings.HasPrefix(h, target) {
			found = true
		}
	}
	if !found {
		t.Errorf("reverted set %v does not reference target %s", reverted, target)
	}
}

func TestExecGitRepository_DiffHunks_PathsFilter(t *testing.T) {
	dir := initTestRepo(t)
	writeAndCommit(t, dir, "a.go", "package a\n", "add a")
	writeAndCommit(t, dir, "b.go", "package b\n", "add b")
	base := headHash(t, dir)
	mustGit(t, dir, "git", "checkout", "-b", "feat/two")
	writeAndCommit(t, dir, "a.go", "package a\n\nvar X = 1\n", "edit a")
	writeAndCommit(t, dir, "b.go", "package b\n\nvar Y = 1\n", "edit b")

	repo := NewExecGitRepository(dir)
	files, err := repo.DiffHunks(context.Background(), base, "feat/two", []string{"a.go"}, 0)
	if err != nil {
		t.Fatalf("DiffHunks: %v", err)
	}
	if len(files) != 1 || files[0].Path != "a.go" {
		t.Fatalf("paths filter: want only a.go, got %+v", files)
	}
}

// TestExecGitRepository_BlameCommits attributes each line to the commit that
// last touched it (the basis for per-hunk → conversation attribution).
func TestExecGitRepository_BlameCommits(t *testing.T) {
	dir := initTestRepo(t)
	writeAndCommit(t, dir, "f.go", "line1\nline2\nline3\n", "c1")
	c1 := headHash(t, dir)
	writeAndCommit(t, dir, "f.go", "line1\nCHANGED\nline3\nline4\n", "c2")
	c2 := headHash(t, dir)

	repo := NewExecGitRepository(dir)
	commits, err := repo.BlameCommits(context.Background(), "HEAD", "f.go")
	if err != nil {
		t.Fatalf("BlameCommits: %v", err)
	}
	if len(commits) != 4 {
		t.Fatalf("want 4 blamed lines, got %d (%v)", len(commits), commits)
	}
	if commits[0] != c1 {
		t.Errorf("line 1 (unchanged) blame = %s, want c1 %s", commits[0], c1)
	}
	if commits[1] != c2 {
		t.Errorf("line 2 (changed) blame = %s, want c2 %s", commits[1], c2)
	}
	if commits[3] != c2 {
		t.Errorf("line 4 (added) blame = %s, want c2 %s", commits[3], c2)
	}

	// Absent path → empty slice, not an error.
	empty, err := repo.BlameCommits(context.Background(), "HEAD", "does/not/exist.go")
	if err != nil {
		t.Errorf("absent path BlameCommits errored: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("absent path: want empty, got %v", empty)
	}
}

func TestExecGitRepository_DiffHunks_Truncates(t *testing.T) {
	dir := initTestRepo(t)
	writeAndCommit(t, dir, "big.txt", "", "add big")
	base := headHash(t, dir)
	mustGit(t, dir, "git", "checkout", "-b", "feat/big")
	// 50 added lines; cap at 10.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("line\n")
	}
	writeAndCommit(t, dir, "big.txt", b.String(), "grow big")

	repo := NewExecGitRepository(dir)
	files, err := repo.DiffHunks(context.Background(), base, "feat/big", nil, 10)
	if err != nil {
		t.Fatalf("DiffHunks: %v", err)
	}
	big := diffFileByPath(t, files, "big.txt")
	if !big.Truncated {
		t.Errorf("big.txt: expected Truncated=true at cap 10")
	}
	total := countKind(big, DiffLineAdded) + countKind(big, DiffLineRemoved) + countKind(big, DiffLineContext)
	if total > 10 {
		t.Errorf("big.txt: kept %d lines, want <= 10", total)
	}
}
