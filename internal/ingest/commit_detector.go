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

// detachedHeadBranch is what a harness records when the session ran on a
// detached checkout. It names no branch, so it cannot anchor reachability.
const detachedHeadBranch = "HEAD"

// branchReachabilityBudget bounds the whole reachability pass for one session.
// Each IsAncestor call is bounded by the analyzer; this bounds their sum, so a
// session with many candidates on a slow repository degrades to the timestamp
// window instead of stalling an ingest worker.
const branchReachabilityBudget = 10 * time.Second

// CommitDetector detects git commits associated with an AI coding session using
// timestamp-based correlation with author email filtering.
//
// Detection is non-fatal: all methods return empty/partial results on git failure
// and record issues as DiagnosticEntry warnings for the pipeline to embed in metadata.
type CommitDetector struct {
	analyzer       GitDiffAnalyzer
	userEmailLower string // normalized to lowercase at construction time
	transcripts    CommitTranscriptReader
	sessionBranch  string // branch the session recorded; "" or "HEAD" leaves the window unrestricted
}

// CommitTranscriptReader is the bounded content boundary used only for real
// transcript files during command validation.
type CommitTranscriptReader interface {
	ReadTranscript(context.Context, string) ([]byte, error)
}

type osCommitTranscriptReader struct{}

func (osCommitTranscriptReader) ReadTranscript(ctx context.Context, path string) ([]byte, error) {
	type readResult struct {
		data []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		data, err := os.ReadFile(path)
		readCh <- readResult{data: data, err: err}
	}()
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case result := <-readCh:
		return result.data, result.err
	case <-readCtx.Done():
		return nil, fmt.Errorf("transcript read timed out after 10s: %s: %w", path, readCtx.Err())
	}
}

// CommitDetectorOption configures optional detector behaviour.
type CommitDetectorOption func(*CommitDetector)

// WithSessionBranch restricts association to commits reachable from the branch
// the session recorded, checked against refs/heads/<branch>. An empty branch,
// or the symbolic "HEAD" a detached checkout reports, leaves the timestamp
// window unrestricted, which is the behaviour for sessions without a branch.
func WithSessionBranch(branch string) CommitDetectorOption {
	return func(cd *CommitDetector) { cd.sessionBranch = branch }
}

// NewCommitDetector creates a CommitDetector for the given analyzer and session user email.
// The email is normalized to lowercase at construction time; comparisons use EqualFold
// for additional safety against locale-dependent case folding.
func NewCommitDetector(analyzer GitDiffAnalyzer, userEmail string, opts ...CommitDetectorOption) *CommitDetector {
	cd := &CommitDetector{
		analyzer:       analyzer,
		userEmailLower: strings.ToLower(userEmail),
		transcripts:    osCommitTranscriptReader{},
	}
	for _, opt := range opts {
		opt(cd)
	}
	return cd
}

func newCommitDetectorWithReader(analyzer GitDiffAnalyzer, userEmail string, reader CommitTranscriptReader, opts ...CommitDetectorOption) *CommitDetector {
	detector := NewCommitDetector(analyzer, userEmail, opts...)
	if reader != nil {
		detector.transcripts = reader
	}
	return detector
}

// TimestampDetection returns commits authored within [sessionStart-commitLookback, sessionEnd+commitLookahead]
// that match the session user's email. When the detector was given a session
// branch, only commits reachable from that branch are returned. Non-fatal: git
// errors are converted to DiagnosticEntry warnings and empty/partial commits
// are returned.
func (cd *CommitDetector) TimestampDetection(ctx context.Context, repoPath string, sessionStart, sessionEnd time.Time) ([]CommitInfo, []DiagnosticEntry) {
	// Commit correlation window: [session.start - commitLookback, session.end + commitLookahead]
	// Lookback: Captures commits authored before session start (offline work pattern).
	//           Trade-off: may attribute commits from an earlier nearby session.
	// Lookahead: Captures post-session code review merges and CI/CD auto-commits.
	since := sessionStart.Add(-commitLookback)
	until := sessionEnd.Add(commitLookahead)

	commits, err := cd.analyzer.GetSessionCommitsWithMetadata(ctx, repoPath, since, until)

	// When user email is unknown, skip the author filter and emit a diagnostic
	// warning. Attribution is impossible without an email; all commits in the
	// window are returned unfiltered by author so the caller has partial data
	// rather than silent zeros. The branch filter still applies: it does not
	// depend on the author.
	if cd.userEmailLower == "" {
		if commits == nil {
			commits = []CommitInfo{}
		}
		var branchDiags []DiagnosticEntry
		commits, branchDiags = cd.restrictToSessionBranch(ctx, repoPath, commits)
		diags := []DiagnosticEntry{{
			ErrorType:   "missing_user_email",
			Location:    "commit_detector.TimestampDetection",
			Message:     "User email is not configured (git config user.email not set). Cannot attribute commits by author.",
			Remediation: "Run 'git config user.email <email>' to set your email, then re-run 'peasant ingest'.",
		}}
		return commits, append(diags, branchDiags...)
	}

	// Apply email filter BEFORE error check so partial results on timeout
	// are also filtered, preventing commits from other authors leaking into DB.
	filtered := make([]CommitInfo, 0, len(commits))
	for _, c := range commits {
		if strings.EqualFold(c.AuthorEmail, cd.userEmailLower) {
			filtered = append(filtered, c)
		}
	}

	// The branch filter runs on the same partial results for the same reason.
	filtered, branchDiags := cd.restrictToSessionBranch(ctx, repoPath, filtered)

	var diags []DiagnosticEntry
	if err != nil {
		diags = append(diags, cd.diagnosticFromError(err, repoPath))
	}
	diags = append(diags, branchDiags...)
	return filtered, diags
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

	data, readErr := cd.transcripts.ReadTranscript(ctx, transcriptPath)
	if readErr != nil {
		errorType := "transcript_read_failed"
		if errors.Is(readErr, context.DeadlineExceeded) {
			errorType = "transcript_read_timeout"
		}
		diags = append(diags, DiagnosticEntry{
			ErrorType:   errorType,
			Location:    "commit_detector.LayeredDetection",
			Message:     fmt.Sprintf("command parsing skipped: %v", readErr),
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

// sessionBranchRef returns the fully qualified ref for the recorded branch, or
// "" when the session recorded no branch that can anchor reachability.
func (cd *CommitDetector) sessionBranchRef() string {
	if cd.sessionBranch == "" || cd.sessionBranch == detachedHeadBranch {
		return ""
	}
	return "refs/heads/" + cd.sessionBranch
}

// restrictToSessionBranch drops candidates that are not reachable from the
// recorded branch. It is all-or-nothing per session: when any reachability
// question cannot be answered, the unfiltered candidates are returned with one
// diagnostic, so a branch deleted after merge or a slow repository degrades to
// the timestamp-window behaviour instead of dropping real associations.
func (cd *CommitDetector) restrictToSessionBranch(ctx context.Context, repoPath string, candidates []CommitInfo) ([]CommitInfo, []DiagnosticEntry) {
	ref := cd.sessionBranchRef()
	if ref == "" || len(candidates) == 0 {
		return candidates, nil
	}

	ctx, cancel := context.WithTimeout(ctx, branchReachabilityBudget)
	defer cancel()

	kept := make([]CommitInfo, 0, len(candidates))
	for _, c := range candidates {
		reachable, err := cd.analyzer.IsAncestor(ctx, repoPath, c.Hash, ref)
		if err != nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("%w (the %v reachability budget for this session was exhausted)", err, branchReachabilityBudget)
			}
			return candidates, []DiagnosticEntry{cd.branchDiagnostic(err, repoPath, ref)}
		}
		if reachable {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// branchDiagnostic explains why the branch filter was skipped for this session.
func (cd *CommitDetector) branchDiagnostic(err error, repoPath, ref string) DiagnosticEntry {
	return DiagnosticEntry{
		ErrorType: "branch_reachability_unavailable",
		Location:  fmt.Sprintf("commit_detector.TimestampDetection: %s", repoPath),
		Message: fmt.Sprintf(
			"could not check whether commits are reachable from %s in %s: %v; "+
				"the timestamp window was applied without the branch filter",
			ref, repoPath, err,
		),
		Remediation: "If the branch was deleted after it merged, this is expected. " +
			"Otherwise verify the repository is readable and run 'peasant ingest --force' to retry.",
	}
}
