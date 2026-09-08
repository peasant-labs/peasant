package ingest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Test helpers ---

// initTestRepoWithModifiableFile creates a temp repo with a tracked file.txt
// already committed. Subsequent commits that modify file.txt will have status M
// (Modified), which is required for --diff-filter=M to include them.
func initTestRepoWithModifiableFile(t *testing.T) string {
	t.Helper()
	dir := initTestRepo(t) // git init + allow-empty init commit

	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("initial content\n"), 0644); err != nil {
		t.Fatalf("write file.txt: %v", err)
	}
	mustGit(t, dir, "git", "add", "file.txt")
	mustGit(t, dir, "git", "commit", "-m", "add base file")
	return dir
}

// mustGit runs a git command and fails the test on non-zero exit.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// addModifyCommit writes new content to file.txt and commits it.
// Because file.txt already exists, git records this as status M (Modified).
func addModifyCommit(t *testing.T, dir, content, message string) {
	t.Helper()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write file.txt: %v", err)
	}
	mustGit(t, dir, "git", "add", "file.txt")
	mustGit(t, dir, "git", "commit", "-m", message)
}

// addNModifyCommits creates n commits each modifying file.txt.
// Returns the approximate time window that encompasses them.
func addNModifyCommits(t *testing.T, dir string, n int) (since, until time.Time) {
	t.Helper()
	since = time.Now().Add(-2 * time.Second)
	for i := range n {
		addModifyCommit(t, dir, fmt.Sprintf("content %d\n", i), fmt.Sprintf("commit %d", i))
	}
	until = time.Now().Add(2 * time.Second)
	return since, until
}

// defaultAnalyzer returns an ExecGitDiffAnalyzer with sensible test defaults.
func defaultAnalyzer() *ExecGitDiffAnalyzer {
	return &ExecGitDiffAnalyzer{
		BatchSize:  DefaultCommitBatchSize,
		MaxCommits: DefaultMaxCommitsPerSession,
		LogTimeout: DefaultGitLogTimeout,
	}
}

// --- Tests ---

// TestStreamingBehavior verifies that GetSessionCommitsWithMetadata returns all
// commits from a small repository with file-modifying commits.
func TestStreamingBehavior(t *testing.T) {
	dir := initTestRepoWithModifiableFile(t)
	const nCommits = 3
	since, until := addNModifyCommits(t, dir, nCommits)

	analyzer := defaultAnalyzer()
	commits, err := analyzer.GetSessionCommitsWithMetadata(context.Background(), dir, since, until)
	if err != nil {
		t.Fatalf("GetSessionCommitsWithMetadata: unexpected error: %v", err)
	}
	if len(commits) != nCommits {
		t.Errorf("got %d commits, want %d", len(commits), nCommits)
	}

	// Verify each commit has the expected fields populated.
	for i, c := range commits {
		if c.Hash == "" {
			t.Errorf("commit[%d].Hash is empty", i)
		}
		if c.AuthorEmail == "" {
			t.Errorf("commit[%d].AuthorEmail is empty", i)
		}
		if c.AuthorName == "" {
			t.Errorf("commit[%d].AuthorName is empty", i)
		}
		if c.AuthorTime == 0 {
			t.Errorf("commit[%d].AuthorTime is zero", i)
		}
		if c.CommitTime == 0 {
			t.Errorf("commit[%d].CommitTime is zero", i)
		}
		if !strings.HasPrefix(c.Message, fmt.Sprintf("commit %d", i)) {
			t.Errorf("commit[%d].Message = %q, want prefix %q", i, c.Message, fmt.Sprintf("commit %d", i))
		}
	}
}

// TestBatchProcessing verifies that a small BatchSize does not drop commits —
// the batch size controls memory, not the number of results.
func TestBatchProcessing(t *testing.T) {
	dir := initTestRepoWithModifiableFile(t)
	const nCommits = 15
	since, until := addNModifyCommits(t, dir, nCommits)

	// Small BatchSize to exercise multi-batch iteration logic.
	analyzer := &ExecGitDiffAnalyzer{
		BatchSize:  5,
		MaxCommits: DefaultMaxCommitsPerSession,
		LogTimeout: DefaultGitLogTimeout,
	}

	commits, err := analyzer.GetSessionCommitsWithMetadata(context.Background(), dir, since, until)
	if err != nil {
		t.Fatalf("GetSessionCommitsWithMetadata: unexpected error: %v", err)
	}
	if len(commits) != nCommits {
		t.Errorf("got %d commits with BatchSize=5, want %d", len(commits), nCommits)
	}
}

// TestCommitCapHandling verifies that MaxCommits is respected: more than MaxCommits
// commits exist in the window, but only MaxCommits are returned, with no error.
func TestCommitCapHandling(t *testing.T) {
	dir := initTestRepoWithModifiableFile(t)
	// Create more commits than the cap.
	const nCommits = 10
	const cap = 5
	since, until := addNModifyCommits(t, dir, nCommits)

	analyzer := &ExecGitDiffAnalyzer{
		BatchSize:  DefaultCommitBatchSize,
		MaxCommits: cap,
		LogTimeout: DefaultGitLogTimeout,
	}

	commits, err := analyzer.GetSessionCommitsWithMetadata(context.Background(), dir, since, until)
	// Cap hit is silent — no error.
	if err != nil {
		t.Fatalf("GetSessionCommitsWithMetadata: unexpected error on cap hit: %v", err)
	}
	if len(commits) != cap {
		t.Errorf("got %d commits, want exactly %d (cap)", len(commits), cap)
	}
	// Commits should be the oldest ones (--reverse order).
	if commits[0].Message != "commit 0" {
		t.Errorf("first commit message = %q, want %q (oldest first)", commits[0].Message, "commit 0")
	}
}

// TestTimeoutHandling verifies that when LogTimeout expires, GetSessionCommitsWithMetadata
// returns partial commits (possibly empty) and a diagnostic error describing the timeout.
func TestTimeoutHandling(t *testing.T) {
	dir := initTestRepoWithModifiableFile(t)
	since, until := addNModifyCommits(t, dir, 3)

	// A 1ns timeout is effectively instant — the git process cannot output
	// anything before the context deadline fires, making this deterministic.
	analyzer := &ExecGitDiffAnalyzer{
		BatchSize:  DefaultCommitBatchSize,
		MaxCommits: DefaultMaxCommitsPerSession,
		LogTimeout: 1 * time.Nanosecond,
	}

	commits, err := analyzer.GetSessionCommitsWithMetadata(context.Background(), dir, since, until)

	// Must return a diagnostic error on timeout.
	// The error may manifest as "timed out" (from our explicit check after scan)
	// or "context deadline exceeded" (when the timeout fires during cmd.Start).
	// Both indicate the same timeout condition.
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "timed out") && !strings.Contains(msg, "deadline exceeded") {
		t.Errorf("expected timeout error (containing 'timed out' or 'deadline exceeded'), got: %v", err)
	}

	// Partial results: may be empty or partial, but never nil.
	if commits == nil {
		t.Error("expected non-nil commits slice on timeout; got nil")
	}
}

// TestEmptyRepository verifies that no commits in the query window returns an
// empty slice with no error.
func TestEmptyRepository(t *testing.T) {
	dir := initTestRepoWithModifiableFile(t)

	// Query a window in the far past — no commits exist there.
	since := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)

	analyzer := defaultAnalyzer()
	commits, err := analyzer.GetSessionCommitsWithMetadata(context.Background(), dir, since, until)
	if err != nil {
		t.Fatalf("GetSessionCommitsWithMetadata: unexpected error: %v", err)
	}
	if len(commits) != 0 {
		t.Errorf("got %d commits in empty window, want 0", len(commits))
	}
}

// TestGetSessionCommits_HashesOnly verifies that GetSessionCommits returns the
// same commit hashes as the Hash fields from GetSessionCommitsWithMetadata.
func TestGetSessionCommits_HashesOnly(t *testing.T) {
	dir := initTestRepoWithModifiableFile(t)
	const nCommits = 4
	since, until := addNModifyCommits(t, dir, nCommits)

	analyzer := defaultAnalyzer()
	ctx := context.Background()

	hashes, err := analyzer.GetSessionCommits(ctx, dir, since, until)
	if err != nil {
		t.Fatalf("GetSessionCommits: %v", err)
	}
	full, err := analyzer.GetSessionCommitsWithMetadata(ctx, dir, since, until)
	if err != nil {
		t.Fatalf("GetSessionCommitsWithMetadata: %v", err)
	}

	if len(hashes) != len(full) {
		t.Fatalf("GetSessionCommits returned %d hashes, GetSessionCommitsWithMetadata returned %d commits", len(hashes), len(full))
	}
	for i := range hashes {
		if hashes[i] != full[i].Hash {
			t.Errorf("hashes[%d] = %q, full[%d].Hash = %q: mismatch", i, hashes[i], i, full[i].Hash)
		}
	}
}

// TestGetFileAtCommit verifies that GetFileAtCommit retrieves file contents at
// a specific commit hash.
func TestGetFileAtCommit(t *testing.T) {
	dir := initTestRepoWithModifiableFile(t)

	// Write a known file and commit it.
	const wantContent = "hello from commit\n"
	addModifyCommit(t, dir, wantContent, "add test content")

	// Get the commit hash.
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	commitHash := strings.TrimSpace(string(out))

	analyzer := defaultAnalyzer()
	got, err := analyzer.GetFileAtCommit(context.Background(), dir, "file.txt", commitHash)
	if err != nil {
		t.Fatalf("GetFileAtCommit: %v", err)
	}
	if string(got) != wantContent {
		t.Errorf("GetFileAtCommit content = %q, want %q", string(got), wantContent)
	}
}
