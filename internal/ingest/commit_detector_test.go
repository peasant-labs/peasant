package ingest_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// --- Constructor ---

// TestNewCommitDetector_EmailNormalization verifies that the constructor normalizes
// the session user email to lowercase. Tested via behavior: a commit with the
// expected lowercase email must match after construction with various input forms.
func TestNewCommitDetector_EmailNormalization(t *testing.T) {
	cases := []struct {
		name       string
		inputEmail string
		wantMatch  bool
	}{
		{"lowercase input", "alice@company.com", true},
		{"all-caps input", "ALICE@COMPANY.COM", true},
		{"mixed-case input", "Alice@Company.Com", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			commit := ingest.CommitInfo{
				Hash:        "abc123",
				AuthorEmail: "alice@company.com", // always lowercase in commit
				AuthorName:  "Alice",
				AuthorTime:  now.UnixMilli(),
				CommitTime:  now.UnixMilli(),
			}
			stub := &testutil.StubGitDiffAnalyzer{
				CommitInfos: []ingest.CommitInfo{commit},
			}
			cd := ingest.NewCommitDetector(stub, tc.inputEmail)

			sessionStart := now.Add(-time.Hour)
			sessionEnd := now.Add(time.Hour)
			commits, diags := cd.TimestampDetection(context.Background(), "/repo", sessionStart, sessionEnd)

			if len(diags) > 0 {
				t.Errorf("unexpected diagnostics: %v", diags)
			}
			if tc.wantMatch && len(commits) == 0 {
				t.Errorf("inputEmail=%q: expected match with alice@company.com, got 0 commits", tc.inputEmail)
			}
			if !tc.wantMatch && len(commits) > 0 {
				t.Errorf("inputEmail=%q: expected no match, got %d commits", tc.inputEmail, len(commits))
			}
		})
	}
}

// --- Timestamp window ---

// TestTimestampDetection_WithinWindow verifies that commits within [sessionStart-3d, sessionEnd+3d]
// are returned when the author matches the session email.
func TestTimestampDetection_WithinWindow(t *testing.T) {
	now := time.Now()
	commit := ingest.CommitInfo{
		Hash:        "def456",
		AuthorEmail: testutil.TestEmail,
		AuthorName:  "Test User",
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
		Message:     "feat: add feature",
	}

	stub := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{commit},
	}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.TimestampDetection(context.Background(), "/repo", sessionStart, sessionEnd)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(commits) != 1 {
		t.Fatalf("got %d commits, want 1", len(commits))
	}
	if commits[0].Hash != "def456" {
		t.Errorf("commit hash = %q, want %q", commits[0].Hash, "def456")
	}
}

// TestTimestampDetection_WithinWindow_AuthorMatch verifies that only commits
// authored by the session user are returned when multiple authors contributed.
func TestTimestampDetection_WithinWindow_AuthorMatch(t *testing.T) {
	now := time.Now()
	aliceCommit := ingest.CommitInfo{
		Hash:        "aaa111",
		AuthorEmail: "alice@company.com",
		AuthorName:  "Alice",
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}
	bobCommit := ingest.CommitInfo{
		Hash:        "bbb222",
		AuthorEmail: "bob@company.com",
		AuthorName:  "Bob",
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}

	stub := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{aliceCommit, bobCommit},
	}
	cd := ingest.NewCommitDetector(stub, "alice@company.com")

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.TimestampDetection(context.Background(), "/repo", sessionStart, sessionEnd)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(commits) != 1 {
		t.Fatalf("got %d commits, want 1 (only alice's commit)", len(commits))
	}
	if commits[0].Hash != "aaa111" {
		t.Errorf("commit hash = %q, want alice's commit %q", commits[0].Hash, "aaa111")
	}
}

// TestTimestampDetection_LookaheadWindow verifies that commits made AFTER sessionEnd
// but within the 3-day lookahead are detected (PR/CI/CD scenario), and that commits
// beyond the 3-day window are excluded.
//
// Regression guard: commits at sessionEnd+5d must be EXCLUDED. In the prior 7-day
// window these were included, causing over-attribution across sessions.
//
// This is an integration test using real git and ExecGitDiffAnalyzer with backdated
// commit timestamps to allow controlled placement inside and outside the window.
func TestTimestampDetection_LookaheadWindow(t *testing.T) {
	dir := initCommitDetectorTestRepo(t)
	now := time.Now()

	// Session ended 8 days ago; window = [now-13d, now-5d].
	sessionStart := now.Add(-10 * 24 * time.Hour)
	sessionEnd := now.Add(-8 * 24 * time.Hour)

	// C1: sessionEnd+2d (now-6d) — within 3-day lookahead — must be INCLUDED.
	addGitCommitAt(t, dir, "lookahead 2d\n", "post: 2d after session end", now.Add(-6*24*time.Hour))

	// C2: sessionEnd+5d (now-3d) — beyond 3-day lookahead — must be EXCLUDED.
	// Regression guard: was included in the old 7-day window.
	addGitCommitAt(t, dir, "lookahead 5d\n", "post: 5d after session end", now.Add(-3*24*time.Hour))

	analyzer := ingest.NewExecGitDiffAnalyzer()
	cd := ingest.NewCommitDetector(analyzer, testutil.TestEmail)

	commits, diags := cd.TimestampDetection(context.Background(), dir, sessionStart, sessionEnd)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	foundC1 := false
	for _, c := range commits {
		if strings.Contains(c.Message, "2d after session end") {
			foundC1 = true
		}
		if strings.Contains(c.Message, "5d after session end") {
			t.Errorf("regression: commit at sessionEnd+5d was included; 3-day lookahead must apply (not old 7-day)")
		}
	}
	if !foundC1 {
		t.Error("expected commit at sessionEnd+2d to be detected within 3-day lookahead window")
	}
}

// TestCommitDetector_TimestampDetection_LookbackWindow verifies that commits made BEFORE
// sessionStart but within the 3-day lookback window are included (offline work pattern),
// and that commits before the lookback window are excluded.
//
// This is an integration test using real git with backdated commit timestamps.
func TestCommitDetector_TimestampDetection_LookbackWindow(t *testing.T) {
	dir := initCommitDetectorTestRepo(t)
	now := time.Now()

	// Session started 10 days ago; window = [now-13d, now-5d].
	sessionStart := now.Add(-10 * 24 * time.Hour)
	sessionEnd := now.Add(-8 * 24 * time.Hour)

	// Commits must be added in chronological order (oldest first = HEAD is newest) so
	// git log's --since traversal doesn't stop prematurely on a backdated parent.

	// sessionStart-4d (now-14d) is before the 3-day lookback and must be excluded.
	addGitCommitAt(t, dir, "lookback 4d\n", "pre: 4d before session start", now.Add(-14*24*time.Hour))

	// sessionStart-2d (now-12d) is within the 3-day lookback and must be included.
	addGitCommitAt(t, dir, "lookback 2d\n", "pre: 2d before session start", now.Add(-12*24*time.Hour))

	analyzer := ingest.NewExecGitDiffAnalyzer()
	cd := ingest.NewCommitDetector(analyzer, testutil.TestEmail)

	commits, diags := cd.TimestampDetection(context.Background(), dir, sessionStart, sessionEnd)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	foundInLookback := false
	for _, c := range commits {
		if strings.Contains(c.Message, "2d before session start") {
			foundInLookback = true
		}
		if strings.Contains(c.Message, "4d before session start") {
			t.Errorf("commit at sessionStart-4d was included; 3-day lookback window must apply")
		}
	}
	if !foundInLookback {
		t.Error("expected commit at sessionStart-2d to be detected within 3-day lookback window")
	}
}

// TestCommitDetector_TimestampDetection_WindowBoundaries verifies the exact boundary
// behaviour of the [sessionStart-commitLookback, sessionEnd+commitLookahead] window.
// Commits AT the boundary are included; commits 1 second outside are excluded.
//
// Note: boundary inclusion at the exact since/until second depends on git's
// --since/--until semantics (both are inclusive in standard git implementations).
func TestCommitDetector_TimestampDetection_WindowBoundaries(t *testing.T) {
	dir := initCommitDetectorTestRepo(t)
	now := time.Now()

	sessionStart := now.Add(-10 * 24 * time.Hour)
	sessionEnd := now.Add(-8 * 24 * time.Hour)

	// Exact boundaries: since = sessionStart-3d, until = sessionEnd+3d.
	sinceBoundary := sessionStart.Add(-3 * 24 * time.Hour) // now-13d
	untilBoundary := sessionEnd.Add(3 * 24 * time.Hour)    // now-5d

	// Commits must be added in chronological order (oldest first = HEAD is newest) so
	// git log's --since traversal doesn't stop prematurely on a backdated parent.

	// C6: 1s before since boundary — must be EXCLUDED (older than window).
	addGitCommitAt(t, dir, "before since boundary\n", "boundary: start-3d-1s", sinceBoundary.Add(-time.Second))
	// C5: exactly at since boundary — must be INCLUDED.
	addGitCommitAt(t, dir, "at since boundary\n", "boundary: at start-3d", sinceBoundary)
	// C7: exactly at until boundary — must be INCLUDED.
	addGitCommitAt(t, dir, "at until boundary\n", "boundary: at end+3d", untilBoundary)
	// C8: 1s after until boundary — must be EXCLUDED (newer than window).
	addGitCommitAt(t, dir, "after until boundary\n", "boundary: end+3d+1s", untilBoundary.Add(time.Second))

	analyzer := ingest.NewExecGitDiffAnalyzer()
	cd := ingest.NewCommitDetector(analyzer, testutil.TestEmail)

	commits, diags := cd.TimestampDetection(context.Background(), dir, sessionStart, sessionEnd)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	messages := make(map[string]bool, len(commits))
	for _, c := range commits {
		messages[c.Message] = true
	}

	if !messages["boundary: at start-3d"] {
		t.Error("commit exactly at sessionStart-3d (since boundary) must be included")
	}
	if messages["boundary: start-3d-1s"] {
		t.Error("commit 1s before sessionStart-3d must be excluded (outside since boundary)")
	}
	if !messages["boundary: at end+3d"] {
		t.Error("commit exactly at sessionEnd+3d (until boundary) must be included")
	}
	if messages["boundary: end+3d+1s"] {
		t.Error("commit 1s after sessionEnd+3d must be excluded (outside until boundary)")
	}
}

// --- Case-insensitive email matching ---

// TestCaseInsensitiveEmail_ExactMatch verifies exact lowercase email match.
func TestCaseInsensitiveEmail_ExactMatch(t *testing.T) {
	testCaseInsensitiveEmail(t, "alice@company.com", "alice@company.com", true)
}

// TestCaseInsensitiveEmail_UpperCase verifies that a commit email in ALL CAPS
// matches a lowercase session email.
func TestCaseInsensitiveEmail_UpperCase(t *testing.T) {
	testCaseInsensitiveEmail(t, "ALICE@COMPANY.COM", "alice@company.com", true)
}

// TestCaseInsensitiveEmail_MixedCase verifies that a mixed-case commit email
// (as commonly emitted by some git clients) matches the session email.
func TestCaseInsensitiveEmail_MixedCase(t *testing.T) {
	testCaseInsensitiveEmail(t, "Alice.Smith@Company.Com", "alice.smith@company.com", true)
}

// TestCaseInsensitiveEmail_NonMatching verifies that a different user's email
// does not match the session email.
func TestCaseInsensitiveEmail_NonMatching(t *testing.T) {
	testCaseInsensitiveEmail(t, "bob@company.com", "alice@company.com", false)
}

// testCaseInsensitiveEmail is a shared helper for email matching tests.
// commitEmail is the AuthorEmail on the fake commit; sessionEmail is passed
// to NewCommitDetector. wantMatch controls whether a result is expected.
func testCaseInsensitiveEmail(t *testing.T, commitEmail, sessionEmail string, wantMatch bool) {
	t.Helper()
	now := time.Now()
	commit := ingest.CommitInfo{
		Hash:        "abc999",
		AuthorEmail: commitEmail,
		AuthorName:  "Test User",
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}

	stub := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{commit},
	}
	cd := ingest.NewCommitDetector(stub, sessionEmail)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.TimestampDetection(context.Background(), "/repo", sessionStart, sessionEnd)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if wantMatch && len(commits) == 0 {
		t.Errorf("commitEmail=%q, sessionEmail=%q: expected match, got 0 commits", commitEmail, sessionEmail)
	}
	if !wantMatch && len(commits) > 0 {
		t.Errorf("commitEmail=%q, sessionEmail=%q: expected no match, got %d commits", commitEmail, sessionEmail, len(commits))
	}
}

// --- Empty user email ---

// TestEmptyUserEmail verifies that when the session user email is not configured
// (empty string), TimestampDetection returns unfiltered commits and a
// missing_user_email diagnostic instead of silently returning zero commits.
func TestEmptyUserEmail(t *testing.T) {
	now := time.Now()
	commit := ingest.CommitInfo{
		Hash:        "any123",
		AuthorEmail: "someone@company.com",
		AuthorName:  "Someone",
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}
	stub := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{commit},
	}
	cd := ingest.NewCommitDetector(stub, "") // empty email

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.TimestampDetection(context.Background(), "/repo", sessionStart, sessionEnd)

	// Must emit missing_user_email diagnostic.
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1 (missing_user_email)", len(diags))
	}
	if diags[0].ErrorType != "missing_user_email" {
		t.Errorf("diagnostic ErrorType = %q, want %q", diags[0].ErrorType, "missing_user_email")
	}
	if diags[0].Remediation == "" {
		t.Error("diagnostic must have non-empty Remediation")
	}
	// Commits returned unfiltered (not zero) so caller has partial data.
	if commits == nil {
		t.Error("commits must be non-nil on empty email (should return unfiltered)")
	}
}

// --- Diagnostic warnings ---

// TestGitTimeoutDiagnostic verifies that when the analyzer returns a timeout error
// (context.DeadlineExceeded), TimestampDetection returns a non-nil commits slice
// and a DiagnosticEntry with ErrorType "git_timeout".
func TestGitTimeoutDiagnostic(t *testing.T) {
	stub := &testutil.StubGitDiffAnalyzer{
		GetCommitsWithMetaErr: context.DeadlineExceeded,
	}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	now := time.Now()
	commits, diags := cd.TimestampDetection(context.Background(), "/repo", now.Add(-time.Hour), now)

	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(diags))
	}
	if diags[0].ErrorType != "git_timeout" {
		t.Errorf("diagnostic ErrorType = %q, want %q", diags[0].ErrorType, "git_timeout")
	}
	// Non-nil: partial results returned even on timeout.
	if commits == nil {
		t.Error("commits must be non-nil on timeout (empty slice, not nil)")
	}
}

// TestGitFailureDiagnostic verifies that when the analyzer returns a non-timeout
// error (e.g. git not installed), TimestampDetection returns an empty commits slice
// and a DiagnosticEntry with ErrorType "git_failure".
func TestGitFailureDiagnostic(t *testing.T) {
	failErr := errors.New("git log in /repo: Start: exec: git: executable not found in $PATH")

	stub := &testutil.StubGitDiffAnalyzer{
		GetCommitsWithMetaErr: failErr,
	}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	now := time.Now()
	commits, diags := cd.TimestampDetection(context.Background(), "/repo", now.Add(-time.Hour), now)

	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(diags))
	}
	if diags[0].ErrorType != "git_failure" {
		t.Errorf("diagnostic ErrorType = %q, want %q", diags[0].ErrorType, "git_failure")
	}
	if len(commits) > 0 {
		t.Errorf("got %d commits on git failure, want 0", len(commits))
	}
}

// TestTimestampDetection_EmailFilterOnErrorPath verifies that when the analyzer
// returns partial commits alongside a timeout error, the email filter is still
// applied — commits from other authors must not leak into the result.
func TestTimestampDetection_EmailFilterOnErrorPath(t *testing.T) {
	now := time.Now()
	matchingCommit := ingest.CommitInfo{
		Hash:        "match111",
		AuthorEmail: testutil.TestEmail,
		AuthorName:  "Test User",
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}
	otherCommit := ingest.CommitInfo{
		Hash:        "other222",
		AuthorEmail: "other@example.com",
		AuthorName:  "Other User",
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}

	// partialOnTimeout returns both commits AND a deadline error (simulates
	// ExecGitDiffAnalyzer returning partial results before timeout fires).
	analyzer := &partialOnTimeoutAnalyzer{
		commits: []ingest.CommitInfo{matchingCommit, otherCommit},
		err:     context.DeadlineExceeded,
	}
	cd := ingest.NewCommitDetector(analyzer, testutil.TestEmail)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.TimestampDetection(context.Background(), "/repo", sessionStart, sessionEnd)

	// Diagnostic must be present (timeout).
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(diags))
	}
	if diags[0].ErrorType != "git_timeout" {
		t.Errorf("diagnostic ErrorType = %q, want %q", diags[0].ErrorType, "git_timeout")
	}

	// Email filter must have been applied: only the matching commit returned.
	if len(commits) != 1 {
		t.Fatalf("got %d commits, want 1 (email filter must apply on error path)", len(commits))
	}
	if commits[0].Hash != "match111" {
		t.Errorf("commit Hash = %q, want %q (only matching author)", commits[0].Hash, "match111")
	}
}

// TestNonFatalHandling verifies that git failure does not propagate as a fatal error.
// The caller receives (empty, diagnostics) and can continue session ingestion normally.
func TestNonFatalHandling(t *testing.T) {
	stub := &testutil.StubGitDiffAnalyzer{
		GetCommitsWithMetaErr: errors.New("git log failed: repository not found"),
	}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	now := time.Now()
	// Must not panic and must return usable (non-nil) results.
	commits, diags := cd.TimestampDetection(context.Background(), "/repo", now.Add(-time.Hour), now)

	// Non-fatal: empty commits, not nil.
	if commits == nil {
		t.Error("commits must be non-nil on error (empty slice, not nil)")
	}
	// At least one diagnostic warning to inform the pipeline.
	if len(diags) == 0 {
		t.Error("expected at least one diagnostic entry on git failure")
	}
	// Diagnostic must have an actionable remediation message.
	if len(diags) > 0 && diags[0].Remediation == "" {
		t.Error("diagnostic entry must have a non-empty Remediation message")
	}
}

// --- Local test helpers ---

// partialOnTimeoutAnalyzer returns both commits AND the configured error,
// simulating ExecGitDiffAnalyzer returning partial results before a timeout fires.
type partialOnTimeoutAnalyzer struct {
	commits []ingest.CommitInfo
	err     error
}

var _ ingest.GitDiffAnalyzer = (*partialOnTimeoutAnalyzer)(nil)

func (p *partialOnTimeoutAnalyzer) GetFileAtCommit(_ context.Context, _, _, _ string) ([]byte, error) {
	return nil, nil
}

func (p *partialOnTimeoutAnalyzer) GetSessionCommits(_ context.Context, _ string, _, _ time.Time) ([]string, error) {
	return nil, p.err
}

func (p *partialOnTimeoutAnalyzer) GetSessionCommitsWithMetadata(_ context.Context, _ string, _, _ time.Time) ([]ingest.CommitInfo, error) {
	return p.commits, p.err
}

// --- Integration test helpers (real git) ---

// initCommitDetectorTestRepo creates a temporary git repository with an initial
// tracked file, ready to accept further commits.
func initCommitDetectorTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGitCmd(t, dir, "git", "init", "-b", "main")
	mustGitCmd(t, dir, "git", "config", "user.email", testutil.TestEmail)
	mustGitCmd(t, dir, "git", "config", "user.name", "Test User")
	mustGitCmd(t, dir, "git", "config", "commit.gpgsign", "false")
	mustGitCmd(t, dir, "git", "commit", "--allow-empty", "-m", "initial")

	// Add a base file so subsequent commits have status M (Modified).
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("base content\n"), 0644); err != nil {
		t.Fatalf("write file.txt: %v", err)
	}
	mustGitCmd(t, dir, "git", "add", "file.txt")
	mustGitCmd(t, dir, "git", "commit", "-m", "add base file")
	return dir
}

// addGitCommitAt writes new content to file.txt and commits it with a backdated
// timestamp. Both GIT_AUTHOR_DATE and GIT_COMMITTER_DATE are set so that git log
// --since/--until (which filters by committer date) produces predictable results.
// The repo's configured user.email is used as the author; call initCommitDetectorTestRepo
// first to set up the repo with testutil.TestEmail.
func addGitCommitAt(t *testing.T, dir, content, message string, at time.Time) {
	t.Helper()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write file.txt: %v", err)
	}
	mustGitCmd(t, dir, "git", "add", "file.txt")
	dateStr := at.UTC().Format(time.RFC3339)
	mustGitCmdEnv(t, dir, []string{
		"GIT_AUTHOR_DATE=" + dateStr,
		"GIT_COMMITTER_DATE=" + dateStr,
	}, "git", "commit", "-m", message)
}

// mustGitCmd runs a git command in dir and fails the test on non-zero exit.
func mustGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	mustGitCmdEnv(t, dir, nil, args...)
}

// mustGitCmdEnv runs a git command in dir with optional extra environment variables
// appended to the current process environment. Fails the test on non-zero exit.
func mustGitCmdEnv(t *testing.T, dir string, extraEnv []string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}
