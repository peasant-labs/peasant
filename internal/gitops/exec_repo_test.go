package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Test helpers ---
// Local re-implementation of the initTestRepo/mustGit pattern from
// internal/ingest/git_analyzer_test.go (copied, not imported — gitops must
// not depend on ingest test internals).

// testDefaultBranchName is the initial branch for throwaway test repos.
const testDefaultBranchName = "main"

// initTestRepo creates a temp directory with a git repo configured for
// testing: initial branch main, identity set, one empty init commit.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "--initial-branch=" + testDefaultBranchName},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "remote", "add", "origin", "git@github.com:testuser/testrepo.git"},
		{"git", "commit", "--allow-empty", "-m", "init"},
	}
	for _, args := range cmds {
		mustGit(t, dir, args...)
	}
	return dir
}

// initTestRepoWithFile creates a repo with a tracked file.txt already
// committed, so later modifications diff as status M.
func initTestRepoWithFile(t *testing.T) string {
	t.Helper()
	dir := initTestRepo(t)
	writeAndCommit(t, dir, "file.txt", "initial content\n", "add base file")
	return dir
}

// mustGit runs a git command in dir and fails the test on non-zero exit.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// gitOut runs a git command in dir and returns trimmed stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// writeAndCommit writes content to name (relative to dir) and commits it.
func writeAndCommit(t *testing.T, dir, name, content, message string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mustGit(t, dir, "git", "add", "--all")
	mustGit(t, dir, "git", "commit", "-m", message)
}

// headHash returns the current HEAD commit hash.
func headHash(t *testing.T, dir string) string {
	t.Helper()
	return gitOut(t, dir, "git", "rev-parse", "HEAD")
}

// --- DefaultBranch ---

func TestExecGitRepository_DefaultBranch_FallbackMain(t *testing.T) {
	dir := initTestRepo(t)
	repo := NewExecGitRepository(dir)

	got, err := repo.DefaultBranch(context.Background())
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != testDefaultBranchName {
		t.Errorf("DefaultBranch = %q, want %q", got, testDefaultBranchName)
	}
}

func TestExecGitRepository_DefaultBranch_OriginHEADWins(t *testing.T) {
	dir := initTestRepo(t)
	// origin/HEAD names "develop" even though only main exists locally —
	// the symbolic-ref path must win over the candidate fallback.
	mustGit(t, dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	repo := NewExecGitRepository(dir)

	got, err := repo.DefaultBranch(context.Background())
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != "develop" {
		t.Errorf("DefaultBranch = %q, want %q (origin/HEAD must win)", got, "develop")
	}
}

func TestExecGitRepository_DefaultBranch_FallbackTrunk(t *testing.T) {
	dir := initTestRepo(t)
	// Rename main → trunk: only the later fallback candidate exists.
	mustGit(t, dir, "git", "branch", "-m", testDefaultBranchName, "trunk")
	repo := NewExecGitRepository(dir)

	got, err := repo.DefaultBranch(context.Background())
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != "trunk" {
		t.Errorf("DefaultBranch = %q, want %q", got, "trunk")
	}
}

func TestExecGitRepository_DefaultBranch_Undeterminable(t *testing.T) {
	dir := initTestRepo(t)
	mustGit(t, dir, "git", "branch", "-m", testDefaultBranchName, "weird-name")
	repo := NewExecGitRepository(dir)

	if _, err := repo.DefaultBranch(context.Background()); err == nil {
		t.Fatal("DefaultBranch: expected error when no candidate branch exists")
	}
}

// --- Branches ---

func TestExecGitRepository_Branches_ExcludesDefault(t *testing.T) {
	dir := initTestRepo(t)
	mustGit(t, dir, "git", "branch", "feat/a")
	mustGit(t, dir, "git", "branch", "feat/b")
	repo := NewExecGitRepository(dir)

	got, err := repo.Branches(context.Background())
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	want := []string{"feat/a", "feat/b"}
	if len(got) != len(want) {
		t.Fatalf("Branches = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Branches[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExecGitRepository_Branches_OnlyDefault(t *testing.T) {
	dir := initTestRepo(t)
	repo := NewExecGitRepository(dir)

	got, err := repo.Branches(context.Background())
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Branches = %v, want empty", got)
	}
}

// --- BranchState ---

func TestExecGitRepository_BranchState(t *testing.T) {
	dir := initTestRepoWithFile(t)
	writeAndCommit(t, dir, "keep.txt", "to be deleted\n", "add keep.txt")
	writeAndCommit(t, dir, "old.go", "package old\n\nfunc Old() {}\n", "add old.go")
	baseHash := headHash(t, dir)

	// Feature branch: modify, add, delete, rename — one commit.
	mustGit(t, dir, "git", "checkout", "-b", "feat/state")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed content\n"), 0o644); err != nil {
		t.Fatalf("write file.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatalf("write new.txt: %v", err)
	}
	mustGit(t, dir, "git", "rm", "--quiet", "keep.txt")
	mustGit(t, dir, "git", "mv", "old.go", "renamed.go")
	mustGit(t, dir, "git", "add", "--all")
	mustGit(t, dir, "git", "commit", "-m", "feature work")

	// Advance main by one commit (behind = 1).
	mustGit(t, dir, "git", "checkout", testDefaultBranchName)
	mustGit(t, dir, "git", "commit", "--allow-empty", "-m", "main moves on")

	repo := NewExecGitRepository(dir)
	state, err := repo.BranchState(context.Background(), "feat/state")
	if err != nil {
		t.Fatalf("BranchState: %v", err)
	}

	if state.Name != "feat/state" {
		t.Errorf("Name = %q, want %q", state.Name, "feat/state")
	}
	if state.MergeBase != baseHash {
		t.Errorf("MergeBase = %q, want %q", state.MergeBase, baseHash)
	}
	if state.AheadCount != 1 {
		t.Errorf("AheadCount = %d, want 1", state.AheadCount)
	}
	if state.BehindCount != 1 {
		t.Errorf("BehindCount = %d, want 1", state.BehindCount)
	}

	byPath := map[string]FileChange{}
	for _, fc := range state.ChangedFiles {
		byPath[fc.Path] = fc
	}
	if len(byPath) != 4 {
		t.Fatalf("ChangedFiles = %+v, want 4 entries", state.ChangedFiles)
	}
	if fc := byPath["file.txt"]; fc.Status != FileStatusModified {
		t.Errorf("file.txt status = %q, want %q", fc.Status, FileStatusModified)
	}
	if fc := byPath["new.txt"]; fc.Status != FileStatusAdded {
		t.Errorf("new.txt status = %q, want %q", fc.Status, FileStatusAdded)
	}
	if fc := byPath["keep.txt"]; fc.Status != FileStatusDeleted {
		t.Errorf("keep.txt status = %q, want %q", fc.Status, FileStatusDeleted)
	}
	ren, ok := byPath["renamed.go"]
	if !ok {
		t.Fatalf("ChangedFiles missing rename target renamed.go: %+v", state.ChangedFiles)
	}
	if ren.Status != FileStatusRenamed {
		t.Errorf("renamed.go status = %q, want %q", ren.Status, FileStatusRenamed)
	}
	if ren.OldPath == nil || *ren.OldPath != "old.go" {
		t.Errorf("renamed.go OldPath = %v, want old.go", ren.OldPath)
	}
}

func TestExecGitRepository_BranchState_UnknownBranch(t *testing.T) {
	dir := initTestRepo(t)
	repo := NewExecGitRepository(dir)

	if _, err := repo.BranchState(context.Background(), "no/such/branch"); err == nil {
		t.Fatal("BranchState: expected error for unknown branch")
	}
}

// --- DiffStats ---

func TestExecGitRepository_DiffStats(t *testing.T) {
	dir := initTestRepoWithFile(t)
	writeAndCommit(t, dir, "keep.txt", "line1\nline2\n", "add keep.txt")

	// Feature branch: modify (+2/-1), add (+3/-0), delete (+0/-2) — one commit.
	mustGit(t, dir, "git", "checkout", "-b", "feat/stats")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed one\nchanged two\n"), 0o644); err != nil {
		t.Fatalf("write file.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatalf("write new.txt: %v", err)
	}
	mustGit(t, dir, "git", "rm", "--quiet", "keep.txt")
	mustGit(t, dir, "git", "add", "--all")
	mustGit(t, dir, "git", "commit", "-m", "feature work")

	// Advance main with its own file. Three-dot (merge-base) semantics must
	// exclude it — a two-tree diff would report it as 10 removed lines.
	mustGit(t, dir, "git", "checkout", testDefaultBranchName)
	writeAndCommit(t, dir, "main-only.txt", strings.Repeat("m\n", 10), "main moves on")

	repo := NewExecGitRepository(dir)
	stats, err := repo.DiffStats(context.Background(), testDefaultBranchName, "feat/stats")
	if err != nil {
		t.Fatalf("DiffStats: %v", err)
	}

	if stats.LinesAdded != 5 {
		t.Errorf("LinesAdded = %d, want 5", stats.LinesAdded)
	}
	if stats.LinesRemoved != 3 {
		t.Errorf("LinesRemoved = %d, want 3", stats.LinesRemoved)
	}

	byPath := map[string]FileDiffStat{}
	for _, fs := range stats.PerFile {
		byPath[fs.Path] = fs
	}
	if len(byPath) != 3 {
		t.Fatalf("PerFile = %+v, want 3 entries", stats.PerFile)
	}
	if fs := byPath["file.txt"]; fs.Added != 2 || fs.Removed != 1 {
		t.Errorf("file.txt = +%d/-%d, want +2/-1", fs.Added, fs.Removed)
	}
	if fs := byPath["new.txt"]; fs.Added != 3 || fs.Removed != 0 {
		t.Errorf("new.txt = +%d/-%d, want +3/-0", fs.Added, fs.Removed)
	}
	if fs := byPath["keep.txt"]; fs.Added != 0 || fs.Removed != 2 {
		t.Errorf("keep.txt = +%d/-%d, want +0/-2", fs.Added, fs.Removed)
	}
	if _, leaked := byPath["main-only.txt"]; leaked {
		t.Error("PerFile includes main-only.txt: diff is not merge-base (three-dot) scoped")
	}
}

func TestExecGitRepository_DiffStats_Rename(t *testing.T) {
	dir := initTestRepoWithFile(t)
	writeAndCommit(t, dir, "pkg/old.go", "package pkg\n", "add pkg/old.go")
	writeAndCommit(t, dir, "top_old.go", "package top\n", "add top_old.go")

	// Pure renames: git emits brace notation for the shared directory
	// ("pkg/{old.go => new.go}") and plain notation at the top level
	// ("top_old.go => top_new.go"). Both must normalize to the new path.
	mustGit(t, dir, "git", "checkout", "-b", "feat/rename")
	mustGit(t, dir, "git", "mv", "pkg/old.go", "pkg/new.go")
	mustGit(t, dir, "git", "mv", "top_old.go", "top_new.go")
	mustGit(t, dir, "git", "commit", "-m", "rename files")

	repo := NewExecGitRepository(dir)
	stats, err := repo.DiffStats(context.Background(), testDefaultBranchName, "feat/rename")
	if err != nil {
		t.Fatalf("DiffStats: %v", err)
	}

	if stats.LinesAdded != 0 || stats.LinesRemoved != 0 {
		t.Errorf("totals = +%d/-%d, want +0/-0 for pure renames", stats.LinesAdded, stats.LinesRemoved)
	}
	byPath := map[string]FileDiffStat{}
	for _, fs := range stats.PerFile {
		byPath[fs.Path] = fs
	}
	if len(byPath) != 2 {
		t.Fatalf("PerFile = %+v, want 2 entries", stats.PerFile)
	}
	if _, ok := byPath["pkg/new.go"]; !ok {
		t.Errorf("PerFile missing brace-form rename target pkg/new.go: %+v", stats.PerFile)
	}
	if _, ok := byPath["top_new.go"]; !ok {
		t.Errorf("PerFile missing plain-form rename target top_new.go: %+v", stats.PerFile)
	}
}

func TestExecGitRepository_DiffStats_BinaryFile(t *testing.T) {
	dir := initTestRepoWithFile(t)

	// Binary file (NUL bytes) added alongside a text change: numstat emits
	// "-\t-" for the binary — its row is kept with 0/0 and totals only count
	// the text file.
	mustGit(t, dir, "git", "checkout", "-b", "feat/binary")
	writeAndCommit(t, dir, "blob.bin", "\x00\x01\x02\x03binary", "add binary blob")
	writeAndCommit(t, dir, "file.txt", "changed one\nchanged two\n", "modify text file")

	repo := NewExecGitRepository(dir)
	stats, err := repo.DiffStats(context.Background(), testDefaultBranchName, "feat/binary")
	if err != nil {
		t.Fatalf("DiffStats: %v", err)
	}

	if stats.LinesAdded != 2 || stats.LinesRemoved != 1 {
		t.Errorf("totals = +%d/-%d, want +2/-1 (binary must not count)", stats.LinesAdded, stats.LinesRemoved)
	}
	byPath := map[string]FileDiffStat{}
	for _, fs := range stats.PerFile {
		byPath[fs.Path] = fs
	}
	if len(byPath) != 2 {
		t.Fatalf("PerFile = %+v, want 2 entries (binary row kept)", stats.PerFile)
	}
	bin, ok := byPath["blob.bin"]
	if !ok {
		t.Fatalf("PerFile missing binary row blob.bin: %+v", stats.PerFile)
	}
	if bin.Added != 0 || bin.Removed != 0 {
		t.Errorf("blob.bin = +%d/-%d, want +0/-0", bin.Added, bin.Removed)
	}
}

func TestExecGitRepository_DiffStats_IdenticalRefs(t *testing.T) {
	dir := initTestRepoWithFile(t)
	repo := NewExecGitRepository(dir)

	stats, err := repo.DiffStats(context.Background(), testDefaultBranchName, testDefaultBranchName)
	if err != nil {
		t.Fatalf("DiffStats: %v", err)
	}
	if stats.LinesAdded != 0 || stats.LinesRemoved != 0 || len(stats.PerFile) != 0 {
		t.Errorf("DiffStats(identical refs) = %+v, want empty", stats)
	}
}

func TestNumstatPath(t *testing.T) {
	cases := []struct {
		field string
		want  string
	}{
		{field: "plain.go", want: "plain.go"},
		{field: "dir/sub/plain.go", want: "dir/sub/plain.go"},
		{field: "old.go => new.go", want: "new.go"},
		{field: "pkg/{old.go => new.go}", want: "pkg/new.go"},
		{field: "a/{b => c}/file.go", want: "a/c/file.go"},
		{field: "a/{b => }/file.go", want: "a/file.go"}, // vanished component collapsed
	}
	for _, tc := range cases {
		if got := numstatPath(tc.field); got != tc.want {
			t.Errorf("numstatPath(%q) = %q, want %q", tc.field, got, tc.want)
		}
	}
}

func TestParseNumstat_MalformedCount(t *testing.T) {
	if _, err := parseNumstat([]string{"x\t1\tfile.go"}); err == nil {
		t.Error("parseNumstat: expected error for non-numeric added count")
	}
	if _, err := parseNumstat([]string{"1\tx\tfile.go"}); err == nil {
		t.Error("parseNumstat: expected error for non-numeric removed count")
	}
}

// --- FileAtCommit ---

func TestExecGitRepository_FileAtCommit(t *testing.T) {
	dir := initTestRepoWithFile(t)
	firstHash := headHash(t, dir)
	writeAndCommit(t, dir, "file.txt", "second version\n", "modify file")

	repo := NewExecGitRepository(dir)
	got, err := repo.FileAtCommit(context.Background(), firstHash, "file.txt")
	if err != nil {
		t.Fatalf("FileAtCommit: %v", err)
	}
	if string(got) != "initial content\n" {
		t.Errorf("FileAtCommit = %q, want %q", got, "initial content\n")
	}

	if _, err := repo.FileAtCommit(context.Background(), firstHash, "nonexistent.txt"); err == nil {
		t.Error("FileAtCommit: expected error for file missing at commit")
	}
}

// --- FilesAtCommit ---

func TestExecGitRepository_FilesAtCommit(t *testing.T) {
	dir := initTestRepoWithFile(t)
	firstHash := headHash(t, dir)
	writeAndCommit(t, dir, "sub/nested.txt", "nested\n", "add nested file")
	writeAndCommit(t, dir, "file.txt", "second version\n", "modify file")

	repo := NewExecGitRepository(dir)

	// Batch at HEAD: both files come back; missing paths are absent, not errors.
	got, err := repo.FilesAtCommit(context.Background(), "HEAD",
		[]string{"file.txt", "sub/nested.txt", "nonexistent.txt"})
	if err != nil {
		t.Fatalf("FilesAtCommit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FilesAtCommit returned %d entries, want 2: %v", len(got), got)
	}
	if string(got["file.txt"]) != "second version\n" {
		t.Errorf("file.txt = %q, want %q", got["file.txt"], "second version\n")
	}
	if string(got["sub/nested.txt"]) != "nested\n" {
		t.Errorf("sub/nested.txt = %q, want %q", got["sub/nested.txt"], "nested\n")
	}

	// Historical commit: contents at that commit, not the worktree.
	old, err := repo.FilesAtCommit(context.Background(), firstHash, []string{"file.txt", "sub/nested.txt"})
	if err != nil {
		t.Fatalf("FilesAtCommit(first): %v", err)
	}
	if string(old["file.txt"]) != "initial content\n" {
		t.Errorf("file.txt@first = %q, want %q", old["file.txt"], "initial content\n")
	}
	if _, ok := old["sub/nested.txt"]; ok {
		t.Error("sub/nested.txt should be absent at the first commit")
	}

	// Empty request: empty result, no error.
	empty, err := repo.FilesAtCommit(context.Background(), "HEAD", nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("FilesAtCommit(none) = %v, %v; want empty, nil", empty, err)
	}
}

func TestExecGitRepository_FilesAtCommit_MatchesFileAtCommit(t *testing.T) {
	dir := initTestRepoWithFile(t)
	repo := NewExecGitRepository(dir)
	ctx := context.Background()

	single, err := repo.FileAtCommit(ctx, "HEAD", "file.txt")
	if err != nil {
		t.Fatalf("FileAtCommit: %v", err)
	}
	batch, err := repo.FilesAtCommit(ctx, "HEAD", []string{"file.txt"})
	if err != nil {
		t.Fatalf("FilesAtCommit: %v", err)
	}
	if string(batch["file.txt"]) != string(single) {
		t.Errorf("batch content %q != single content %q", batch["file.txt"], single)
	}
}

// --- ListFiles ---

func TestExecGitRepository_ListFiles(t *testing.T) {
	dir := initTestRepoWithFile(t)
	writeAndCommit(t, dir, "sub/nested.txt", "nested\n", "add nested file")

	repo := NewExecGitRepository(dir)
	got, err := repo.ListFiles(context.Background(), "HEAD")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	want := map[string]bool{"file.txt": true, "sub/nested.txt": true}
	if len(got) != len(want) {
		t.Fatalf("ListFiles = %v, want keys of %v", got, want)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("ListFiles: unexpected file %q", f)
		}
	}

	if _, err := repo.ListFiles(context.Background(), "no-such-ref"); err == nil {
		t.Error("ListFiles: expected error for unknown ref")
	}
}

// --- Commits ---

func TestExecGitRepository_Commits(t *testing.T) {
	dir := initTestRepoWithFile(t)
	writeAndCommit(t, dir, "file.txt", "v2\n", "second commit")
	writeAndCommit(t, dir, "file.txt", "v3\n", "third commit")
	tipHash := headHash(t, dir)

	repo := NewExecGitRepository(dir)
	got, err := repo.Commits(context.Background(), testDefaultBranchName, 2)
	if err != nil {
		t.Fatalf("Commits: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Commits returned %d, want 2 (limit)", len(got))
	}
	head := got[0]
	if head.Hash != tipHash {
		t.Errorf("Commits[0].Hash = %q, want tip %q (newest first)", head.Hash, tipHash)
	}
	if head.Subject != "third commit" {
		t.Errorf("Commits[0].Subject = %q, want %q", head.Subject, "third commit")
	}
	if head.AuthorEmail != "test@example.com" {
		t.Errorf("Commits[0].AuthorEmail = %q, want %q", head.AuthorEmail, "test@example.com")
	}
	if head.TimeMs <= 0 {
		t.Errorf("Commits[0].TimeMs = %d, want > 0", head.TimeMs)
	}
	if got[1].Subject != "second commit" {
		t.Errorf("Commits[1].Subject = %q, want %q", got[1].Subject, "second commit")
	}

	// No limit returns full history (init + base file + 2 modifications).
	all, err := repo.Commits(context.Background(), testDefaultBranchName, 0)
	if err != nil {
		t.Fatalf("Commits (no limit): %v", err)
	}
	if len(all) != 4 {
		t.Errorf("Commits (no limit) returned %d, want 4", len(all))
	}
}

// --- CommitsInRange ---

func TestExecGitRepository_CommitsInRange(t *testing.T) {
	dir := initTestRepoWithFile(t)

	mustGit(t, dir, "git", "checkout", "-b", "feat/range")
	writeAndCommit(t, dir, "file.txt", "r1\n", "range commit 1")
	hash1 := headHash(t, dir)
	writeAndCommit(t, dir, "file.txt", "r2\n", "range commit 2")
	hash2 := headHash(t, dir)

	// Advance main past the branch point — must NOT appear in the range.
	mustGit(t, dir, "git", "checkout", testDefaultBranchName)
	mustGit(t, dir, "git", "commit", "--allow-empty", "-m", "main only")

	repo := NewExecGitRepository(dir)
	got, err := repo.CommitsInRange(context.Background(), testDefaultBranchName, "feat/range")
	if err != nil {
		t.Fatalf("CommitsInRange: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("CommitsInRange = %v, want 2 hashes", got)
	}
	if got[0] != hash2 || got[1] != hash1 {
		t.Errorf("CommitsInRange = %v, want [%s %s] (newest first)", got, hash2, hash1)
	}

	// Identical refs → empty range, no error.
	empty, err := repo.CommitsInRange(context.Background(), testDefaultBranchName, testDefaultBranchName)
	if err != nil {
		t.Fatalf("CommitsInRange (empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("CommitsInRange (empty) = %v, want none", empty)
	}
}

// --- MergedBranches ---

func TestExecGitRepository_MergedBranches(t *testing.T) {
	dir := initTestRepoWithFile(t)

	// Branch merged with a true merge commit.
	mustGit(t, dir, "git", "checkout", "-b", "feat/done")
	writeAndCommit(t, dir, "done.txt", "done\n", "feature done")
	mustGit(t, dir, "git", "checkout", testDefaultBranchName)
	mustGit(t, dir, "git", "merge", "--no-ff", "feat/done", "-m", "Merge branch 'feat/done'")
	mergeHash := headHash(t, dir)

	// Branch merged by fast-forward (no merge commit).
	mustGit(t, dir, "git", "checkout", "-b", "feat/ff")
	writeAndCommit(t, dir, "ff.txt", "ff\n", "ff work")
	mustGit(t, dir, "git", "checkout", testDefaultBranchName)
	mustGit(t, dir, "git", "merge", "--ff-only", "feat/ff")

	// Open branch — must NOT be listed.
	mustGit(t, dir, "git", "checkout", "-b", "feat/open")
	writeAndCommit(t, dir, "open.txt", "open\n", "open work")
	mustGit(t, dir, "git", "checkout", testDefaultBranchName)
	mustGit(t, dir, "git", "commit", "--allow-empty", "-m", "diverge from feat/open")

	repo := NewExecGitRepository(dir)
	got, err := repo.MergedBranches(context.Background(), 10)
	if err != nil {
		t.Fatalf("MergedBranches: %v", err)
	}

	byName := map[string]MergedBranch{}
	for _, mb := range got {
		byName[mb.Name] = mb
	}
	if len(byName) != 2 {
		t.Fatalf("MergedBranches = %+v, want exactly feat/done and feat/ff", got)
	}
	if _, open := byName["feat/open"]; open {
		t.Error("MergedBranches includes open branch feat/open")
	}

	done, ok := byName["feat/done"]
	if !ok {
		t.Fatalf("MergedBranches missing feat/done: %+v", got)
	}
	if done.MergeCommit != mergeHash {
		t.Errorf("feat/done MergeCommit = %q, want %q", done.MergeCommit, mergeHash)
	}
	if done.MergedAtMs <= 0 {
		t.Errorf("feat/done MergedAtMs = %d, want > 0", done.MergedAtMs)
	}

	ff, ok := byName["feat/ff"]
	if !ok {
		t.Fatalf("MergedBranches missing feat/ff: %+v", got)
	}
	if ff.MergeCommit != "" {
		t.Errorf("feat/ff MergeCommit = %q, want empty (fast-forward)", ff.MergeCommit)
	}
	if ff.MergedAtMs <= 0 {
		t.Errorf("feat/ff MergedAtMs = %d, want > 0 (tip fallback)", ff.MergedAtMs)
	}

	// Limit caps the result.
	limited, err := repo.MergedBranches(context.Background(), 1)
	if err != nil {
		t.Fatalf("MergedBranches (limit 1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("MergedBranches (limit 1) returned %d, want 1", len(limited))
	}
}

func TestExecGitRepository_MergedBranches_NoneMerged(t *testing.T) {
	dir := initTestRepoWithFile(t)
	mustGit(t, dir, "git", "checkout", "-b", "feat/open")
	writeAndCommit(t, dir, "open.txt", "open\n", "open work")
	mustGit(t, dir, "git", "checkout", testDefaultBranchName)
	mustGit(t, dir, "git", "commit", "--allow-empty", "-m", "diverge")

	repo := NewExecGitRepository(dir)
	got, err := repo.MergedBranches(context.Background(), 10)
	if err != nil {
		t.Fatalf("MergedBranches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("MergedBranches = %+v, want empty", got)
	}
}

// --- Missing repo / timeout ---

func TestExecGitRepository_MissingRepo(t *testing.T) {
	repo := NewExecGitRepository(t.TempDir()) // empty dir, not a git repo
	ctx := context.Background()

	if _, err := repo.DefaultBranch(ctx); err == nil {
		t.Error("DefaultBranch: expected error for non-repo")
	}
	if _, err := repo.Branches(ctx); err == nil {
		t.Error("Branches: expected error for non-repo")
	}
	if _, err := repo.BranchState(ctx, testDefaultBranchName); err == nil {
		t.Error("BranchState: expected error for non-repo")
	}
	if _, err := repo.DiffStats(ctx, "a", "b"); err == nil {
		t.Error("DiffStats: expected error for non-repo")
	}
	if _, err := repo.MergedBranches(ctx, 10); err == nil {
		t.Error("MergedBranches: expected error for non-repo")
	}
	if _, err := repo.FileAtCommit(ctx, "HEAD", "file.txt"); err == nil {
		t.Error("FileAtCommit: expected error for non-repo")
	}
	if _, err := repo.FilesAtCommit(ctx, "HEAD", []string{"file.txt"}); err == nil {
		t.Error("FilesAtCommit: expected error for non-repo")
	}
	if _, err := repo.ListFiles(ctx, "HEAD"); err == nil {
		t.Error("ListFiles: expected error for non-repo")
	}
	if _, err := repo.Commits(ctx, "HEAD", 1); err == nil {
		t.Error("Commits: expected error for non-repo")
	}
	if _, err := repo.CommitsInRange(ctx, "a", "b"); err == nil {
		t.Error("CommitsInRange: expected error for non-repo")
	}
}

func TestExecGitRepository_TimeoutConfigurable(t *testing.T) {
	dir := initTestRepo(t)
	repo := NewExecGitRepository(dir)
	repo.Timeout = time.Nanosecond // expires before git can run

	if _, err := repo.DefaultBranch(context.Background()); err == nil {
		t.Fatal("DefaultBranch: expected error with nanosecond timeout")
	}
}

// --- FileStatus ---

func TestNewFileStatus(t *testing.T) {
	cases := []struct {
		raw     string
		want    FileStatus
		wantErr bool
	}{
		{raw: "M", want: FileStatusModified},
		{raw: "A", want: FileStatusAdded},
		{raw: "D", want: FileStatusDeleted},
		{raw: "R", want: FileStatusRenamed},
		{raw: "R100", want: FileStatusRenamed}, // similarity score stripped
		{raw: "R087", want: FileStatusRenamed},
		{raw: "", wantErr: true},
		{raw: "C75", wantErr: true}, // copy: unmodeled
		{raw: "T", wantErr: true},   // typechange: unmodeled
		{raw: "X", wantErr: true},
	}
	for _, tc := range cases {
		got, err := NewFileStatus(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NewFileStatus(%q): expected error, got %q", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NewFileStatus(%q): unexpected error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NewFileStatus(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestFileStatus_IsValid(t *testing.T) {
	for _, s := range []FileStatus{FileStatusModified, FileStatusAdded, FileStatusDeleted, FileStatusRenamed} {
		if !s.IsValid() {
			t.Errorf("FileStatus(%q).IsValid() = false, want true", s)
		}
	}
	for _, s := range []FileStatus{"", "Z", "R100", "MM"} {
		if s.IsValid() {
			t.Errorf("FileStatus(%q).IsValid() = true, want false", s)
		}
	}
}
