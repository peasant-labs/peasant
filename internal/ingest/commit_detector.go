package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// commitLookback is the window before sessionStart for commit detection.
// Captures commits authored before the session starts (offline work pattern).
// Trade-off: may attribute commits from an earlier nearby session.
const commitLookback = 3 * 24 * time.Hour

// commitLookahead is the window after sessionEnd for commit detection.
// Captures post-session code review merges and CI/CD auto-commits.
const commitLookahead = 3 * 24 * time.Hour

// CommitDetector detects git commits associated with an AI coding session using
// timestamp-based correlation with author email filtering.
//
// Detection is non-fatal: all methods return empty/partial results on git failure
// and record issues as DiagnosticEntry warnings for the pipeline to embed in metadata.
type CommitDetector struct {
	analyzer       GitDiffAnalyzer
	userEmailLower string // normalized to lowercase at construction time
}

// NewCommitDetector creates a CommitDetector for the given analyzer and session user email.
// The email is normalized to lowercase at construction time; comparisons use EqualFold
// for additional safety against locale-dependent case folding.
func NewCommitDetector(analyzer GitDiffAnalyzer, userEmail string) *CommitDetector {
	return &CommitDetector{
		analyzer:       analyzer,
		userEmailLower: strings.ToLower(userEmail),
	}
}

// TimestampDetection returns commits authored within [sessionStart-commitLookback, sessionEnd+commitLookahead]
// that match the session user's email. Non-fatal: git errors are converted to
// DiagnosticEntry warnings and empty/partial commits are returned.
func (cd *CommitDetector) TimestampDetection(ctx context.Context, repoPath string, sessionStart, sessionEnd time.Time) ([]CommitInfo, []DiagnosticEntry) {
	// Commit correlation window: [session.start - commitLookback, session.end + commitLookahead]
	// Lookback: Captures commits authored before session start (offline work pattern).
	//           Trade-off: may attribute commits from an earlier nearby session.
	// Lookahead: Captures post-session code review merges and CI/CD auto-commits.
	since := sessionStart.Add(-commitLookback)
	until := sessionEnd.Add(commitLookahead)

	commits, err := cd.analyzer.GetSessionCommitsWithMetadata(ctx, repoPath, since, until)

	// When user email is unknown, skip filtering and emit a diagnostic warning.
	// Attribution is impossible without an email; all commits in the window are
	// returned unfiltered so the caller has partial data rather than silent zeros.
	if cd.userEmailLower == "" {
		if commits == nil {
			commits = []CommitInfo{}
		}
		return commits, []DiagnosticEntry{{
			ErrorType:   "missing_user_email",
			Location:    "commit_detector.TimestampDetection",
			Message:     "User email is not configured (git config user.email not set). Cannot attribute commits by author.",
			Remediation: "Run 'git config user.email <email>' to set your email, then re-run 'peasant ingest'.",
		}}
	}

	// Apply email filter BEFORE error check so partial results on timeout
	// are also filtered, preventing commits from other authors leaking into DB.
	filtered := make([]CommitInfo, 0, len(commits))
	for _, c := range commits {
		if strings.EqualFold(c.AuthorEmail, cd.userEmailLower) {
			filtered = append(filtered, c)
		}
	}

	if err != nil {
		diag := cd.diagnosticFromError(err, repoPath)
		if filtered == nil {
			filtered = []CommitInfo{}
		}
		return filtered, []DiagnosticEntry{diag}
	}
	return filtered, nil
}

// LayeredDetection returns commits using layered detection (timestamp + command validation).
// If transcriptPath is empty or the transcript cannot be read, it falls back
// non-fatally to timestamp-only results. A DiagnosticEntry is appended only when
// the transcript file exists but cannot be read.
func (cd *CommitDetector) LayeredDetection(ctx context.Context, repoPath string, sessionStart, sessionEnd time.Time, transcriptPath string) ([]CommitInfo, []DiagnosticEntry) {
	// Always run timestamp detection first to get candidates.
	candidates, diags := cd.TimestampDetection(ctx, repoPath, sessionStart, sessionEnd)

	// If no transcript path provided, return timestamp results immediately (non-fatal fallback).
	if transcriptPath == "" {
		return candidates, diags
	}

	// os.ReadFile does not accept a context, so run it in a goroutine with a
	// 10-second timeout. Without this, a slow or network filesystem can block
	// the worker goroutine forever, hanging the entire pipeline.
	// The goroutine may outlive the function if the OS stalls, but the buffered
	// channel lets it exit without blocking when the read eventually returns.
	type readResult struct {
		data []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		d, e := os.ReadFile(transcriptPath)
		readCh <- readResult{d, e}
	}()

	readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readCancel()

	var data []byte
	select {
	case r := <-readCh:
		if r.err != nil {
			diags = append(diags, DiagnosticEntry{
				ErrorType:   "transcript_read_failed",
				Location:    "commit_detector.LayeredDetection",
				Message:     fmt.Sprintf("command parsing skipped: %v", r.err),
				Remediation: "transcript may not exist yet; timestamp-only results returned",
			})
			return candidates, diags
		}
		data = r.data
	case <-readCtx.Done():
		diags = append(diags, DiagnosticEntry{
			ErrorType:   "transcript_read_timeout",
			Location:    "commit_detector.LayeredDetection",
			Message:     fmt.Sprintf("transcript read timed out after 10s: %s", transcriptPath),
			Remediation: "transcript may be on a slow or network filesystem; timestamp-only results returned",
		})
		return candidates, diags
	}

	parser := newCommandParser()
	confirmed := parser.ValidateCommits(data, candidates)
	return confirmed, diags
}

// diagnosticFromError converts a git error into a DiagnosticEntry for metadata warnings.
// Distinguishes between timeout errors (partial results expected) and general failures.
// Uses errors.Is for type-safe deadline detection; also checks error message strings
// for errors that wrap timeout context without %w (e.g., ExecGitDiffAnalyzer).
func (cd *CommitDetector) diagnosticFromError(err error, repoPath string) DiagnosticEntry {
	msg := err.Error()
	isTimeout := errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(msg, "timed out") || strings.Contains(msg, "deadline exceeded")
	if isTimeout {
		return DiagnosticEntry{
			ErrorType: "git_timeout",
			Location:  fmt.Sprintf("commit_detector.TimestampDetection: %s", repoPath),
			Message: fmt.Sprintf(
				"git log timed out while querying commits in %s: %s",
				repoPath, msg,
			),
			Remediation: "Run 'peasant ingest --force' to retry. " +
				"If the problem persists, the repository may be too large for the default 5s timeout.",
		}
	}
	return DiagnosticEntry{
		ErrorType: "git_failure",
		Location:  fmt.Sprintf("commit_detector.TimestampDetection: %s", repoPath),
		Message: fmt.Sprintf(
			"git log failed while querying commits in %s: %s",
			repoPath, msg,
		),
		Remediation: "Verify that git is installed, the repository path is valid, " +
			"and the user has read access to the repository.",
	}
}
