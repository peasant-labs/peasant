package ingest_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// --- CommandParser unit tests ---

// TestCommandParser_HasGitActivity_DetectsCommit verifies that a transcript
// containing "git commit" is recognised as having git activity.
func TestCommandParser_HasGitActivity_DetectsCommit(t *testing.T) {
	t.Parallel()
	transcript := []byte(`{"type":"tool","name":"Bash","input":"git commit -m 'fix: typo'"}`)
	assertHasGitActivity(t, transcript, true)
}

// TestCommandParser_HasGitActivity_DetectsPush verifies that a transcript
// containing "git push" is recognised as having git activity.
func TestCommandParser_HasGitActivity_DetectsPush(t *testing.T) {
	t.Parallel()
	transcript := []byte(`{"type":"tool","name":"Bash","input":"git push origin main"}`)
	assertHasGitActivity(t, transcript, true)
}

// TestCommandParser_HasGitActivity_DetectsRebase verifies that "git rebase" is detected.
func TestCommandParser_HasGitActivity_DetectsRebase(t *testing.T) {
	t.Parallel()
	transcript := []byte(`user ran: git rebase -i HEAD~3`)
	assertHasGitActivity(t, transcript, true)
}

// TestCommandParser_HasGitActivity_DetectsMerge verifies that "git merge" is detected.
func TestCommandParser_HasGitActivity_DetectsMerge(t *testing.T) {
	t.Parallel()
	transcript := []byte(`git merge --no-ff feature-branch`)
	assertHasGitActivity(t, transcript, true)
}

// TestCommandParser_HasGitActivity_NoGitCommands verifies that a transcript
// with only file edits (no git commands) is not treated as having git activity.
func TestCommandParser_HasGitActivity_NoGitCommands(t *testing.T) {
	t.Parallel()
	transcript := []byte(`{"type":"tool","name":"Edit","input":{"path":"main.go","content":"package main"}}`)
	assertHasGitActivity(t, transcript, false)
}

// assertHasGitActivity is a helper that creates a CommitDetector with a stub
// and calls LayeredDetection with the given transcript bytes written to a temp
// file, then verifies whether git activity was detected by comparing result
// commit counts.
//
// It uses ValidateCommits indirectly via LayeredDetection, testing the full
// integration path.
func assertHasGitActivity(t *testing.T, transcript []byte, wantActivity bool) {
	t.Helper()

	now := time.Now()
	candidate := ingest.CommitInfo{
		Hash:        "abc111",
		AuthorEmail: testutil.TestEmail,
		AuthorName:  "Test User",
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}
	stub := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{candidate},
	}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	// Write transcript to a temp file.
	f := writeTempTranscript(t, transcript)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, _ := cd.LayeredDetection(context.Background(), "/repo", sessionStart, sessionEnd, f)

	if wantActivity && len(commits) == 0 {
		t.Error("expected git activity to be detected, but got 0 commits returned")
	}
	if !wantActivity && len(commits) > 0 {
		t.Errorf("expected no git activity, but got %d commits", len(commits))
	}
}

// --- ValidateCommits behaviour ---

// TestCommandParser_ValidateCommits_WithActivity verifies that when a transcript
// contains git activity, all timestamp candidates are returned (deduped).
func TestCommandParser_ValidateCommits_WithActivity(t *testing.T) {
	t.Parallel()
	now := time.Now()
	candidates := []ingest.CommitInfo{
		{Hash: "aaa", AuthorEmail: testutil.TestEmail, AuthorTime: now.UnixMilli(), CommitTime: now.UnixMilli()},
		{Hash: "bbb", AuthorEmail: testutil.TestEmail, AuthorTime: now.UnixMilli(), CommitTime: now.UnixMilli()},
	}
	stub := &testutil.StubGitDiffAnalyzer{CommitInfos: candidates}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	transcript := []byte(`git commit -m "wip: progress"`)
	f := writeTempTranscript(t, transcript)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.LayeredDetection(context.Background(), "/repo", sessionStart, sessionEnd, f)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(commits) != 2 {
		t.Errorf("got %d commits, want 2", len(commits))
	}
}

// TestCommandParser_ValidateCommits_NoActivity verifies that when a transcript
// contains no git commands, an empty slice is returned (no intent detected).
func TestCommandParser_ValidateCommits_NoActivity(t *testing.T) {
	t.Parallel()
	now := time.Now()
	candidates := []ingest.CommitInfo{
		{Hash: "ccc", AuthorEmail: testutil.TestEmail, AuthorTime: now.UnixMilli(), CommitTime: now.UnixMilli()},
	}
	stub := &testutil.StubGitDiffAnalyzer{CommitInfos: candidates}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	transcript := []byte(`{"type":"tool","name":"Read","input":"main.go"}`)
	f := writeTempTranscript(t, transcript)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.LayeredDetection(context.Background(), "/repo", sessionStart, sessionEnd, f)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(commits) != 0 {
		t.Errorf("got %d commits, want 0 (no git activity in transcript)", len(commits))
	}
}

// TestCommandParser_ValidateCommits_Deduplication verifies that when timestamp
// detection returns duplicate hashes (shouldn't happen in practice but must be
// handled), ValidateCommits deduplicates them.
func TestCommandParser_ValidateCommits_Deduplication(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// Two entries with the same hash — only one must appear in the result.
	duplicate := ingest.CommitInfo{
		Hash:        "dup999",
		AuthorEmail: testutil.TestEmail,
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}
	stub := &testutil.StubGitDiffAnalyzer{
		CommitInfos: []ingest.CommitInfo{duplicate, duplicate},
	}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	transcript := []byte(`git push origin main`)
	f := writeTempTranscript(t, transcript)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.LayeredDetection(context.Background(), "/repo", sessionStart, sessionEnd, f)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(commits) != 1 {
		t.Errorf("got %d commits, want 1 (deduplication of dup999)", len(commits))
	}
	if commits[0].Hash != "dup999" {
		t.Errorf("commit hash = %q, want %q", commits[0].Hash, "dup999")
	}
}

// --- LayeredDetection fallback behaviour ---

// TestLayeredDetection_FallbackWhenNoTranscript verifies that an empty
// transcriptPath causes LayeredDetection to return the timestamp results
// unchanged (non-fatal fallback with no extra diagnostics).
func TestLayeredDetection_FallbackWhenNoTranscript(t *testing.T) {
	t.Parallel()
	now := time.Now()
	candidate := ingest.CommitInfo{
		Hash:        "ts001",
		AuthorEmail: testutil.TestEmail,
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}
	stub := &testutil.StubGitDiffAnalyzer{CommitInfos: []ingest.CommitInfo{candidate}}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	// Empty transcriptPath → fallback to timestamp-only.
	commits, diags := cd.LayeredDetection(context.Background(), "/repo", sessionStart, sessionEnd, "")

	if len(diags) > 0 {
		t.Errorf("expected no diagnostics on empty transcript path, got: %v", diags)
	}
	if len(commits) != 1 {
		t.Errorf("got %d commits, want 1 (timestamp results passed through)", len(commits))
	}
}

// TestLayeredDetection_FallbackWhenTranscriptMissing verifies that when the
// transcript file does not exist, LayeredDetection appends a
// "transcript_read_failed" diagnostic and returns the timestamp candidates.
func TestLayeredDetection_FallbackWhenTranscriptMissing(t *testing.T) {
	t.Parallel()
	now := time.Now()
	candidate := ingest.CommitInfo{
		Hash:        "ts002",
		AuthorEmail: testutil.TestEmail,
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}
	stub := &testutil.StubGitDiffAnalyzer{CommitInfos: []ingest.CommitInfo{candidate}}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	// Nonexistent path → os.ReadFile fails.
	commits, diags := cd.LayeredDetection(
		context.Background(), "/repo", sessionStart, sessionEnd,
		"/nonexistent/path/transcript.jsonl",
	)

	// Must include a transcript_read_failed diagnostic.
	found := false
	for _, d := range diags {
		if d.ErrorType == "transcript_read_failed" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected transcript_read_failed diagnostic, got: %v", diags)
	}
	// Timestamp candidates still returned (non-fatal fallback).
	if len(commits) != 1 {
		t.Errorf("got %d commits, want 1 (timestamp fallback on missing transcript)", len(commits))
	}
}

// TestLayeredDetection_FiltersWhenNoGitActivity verifies that when the transcript
// file contains no git commands, LayeredDetection returns an empty commit slice
// (intent not confirmed).
func TestLayeredDetection_FiltersWhenNoGitActivity(t *testing.T) {
	t.Parallel()
	now := time.Now()
	candidate := ingest.CommitInfo{
		Hash:        "ts003",
		AuthorEmail: testutil.TestEmail,
		AuthorTime:  now.UnixMilli(),
		CommitTime:  now.UnixMilli(),
	}
	stub := &testutil.StubGitDiffAnalyzer{CommitInfos: []ingest.CommitInfo{candidate}}
	cd := ingest.NewCommitDetector(stub, testutil.TestEmail)

	transcript := []byte(`{"type":"assistant","content":"I'll help you read that file."}`)
	f := writeTempTranscript(t, transcript)

	sessionStart := now.Add(-time.Hour)
	sessionEnd := now.Add(time.Hour)
	commits, diags := cd.LayeredDetection(context.Background(), "/repo", sessionStart, sessionEnd, f)

	if len(diags) > 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}
	if len(commits) != 0 {
		t.Errorf("got %d commits, want 0 (no git activity → intent not confirmed)", len(commits))
	}
}

// --- helpers ---

// writeTempTranscript writes bytes to a temp file and returns the path.
// The file is automatically cleaned up when the test ends.
func writeTempTranscript(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatalf("writeTempTranscript: %v", err)
	}
	return path
}
