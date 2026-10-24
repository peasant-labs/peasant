package ingest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/proc"
)

// Default streaming parameters for ExecGitDiffAnalyzer.
const (
	// DefaultCommitBatchSize is the number of commits processed per scanner iteration.
	// O(BatchSize) memory footprint regardless of total commit count.
	DefaultCommitBatchSize = 500

	// DefaultMaxCommitsPerSession caps the number of commits returned per session.
	// Prevents pathological cases in monorepos with many commits per day.
	DefaultMaxCommitsPerSession = 1000

	// DefaultGitLogTimeout is the per-call timeout for git log operations.
	DefaultGitLogTimeout = 5 * time.Second
)

// ExecGitDiffAnalyzer implements GitDiffAnalyzer by shelling out to git.
// All methods use argument-list form (exec.Command), never sh -c.
//
// Streaming configuration:
//   - BatchSize controls scanner memory: O(BatchSize) per call, not O(TotalCommits).
//   - MaxCommits caps results per session; cap hit is silent (non-fatal).
//   - LogTimeout guards against pathologically slow git in monorepos.
//     Timeout returns partial results and a diagnostic error.
type ExecGitDiffAnalyzer struct {
	// BatchSize is the scanner batch size (number of commits buffered at a time).
	// Defaults to DefaultCommitBatchSize when zero.
	BatchSize int

	// MaxCommits is the per-session commit cap. Defaults to DefaultMaxCommitsPerSession when zero.
	MaxCommits int

	// LogTimeout is the per-call timeout for git log. Defaults to DefaultGitLogTimeout when zero.
	LogTimeout time.Duration
}

var _ GitDiffAnalyzer = (*ExecGitDiffAnalyzer)(nil)

// NewExecGitDiffAnalyzer returns an ExecGitDiffAnalyzer with default parameters.
func NewExecGitDiffAnalyzer() *ExecGitDiffAnalyzer {
	return &ExecGitDiffAnalyzer{
		BatchSize:  DefaultCommitBatchSize,
		MaxCommits: DefaultMaxCommitsPerSession,
		LogTimeout: DefaultGitLogTimeout,
	}
}

func (g *ExecGitDiffAnalyzer) logTimeout() time.Duration {
	if g.LogTimeout > 0 {
		return g.LogTimeout
	}
	return DefaultGitLogTimeout
}

func (g *ExecGitDiffAnalyzer) maxCommits() int {
	if g.MaxCommits > 0 {
		return g.MaxCommits
	}
	return DefaultMaxCommitsPerSession
}

// GetFileAtCommit returns the contents of a file at a specific git commit.
// Returns an error if the file does not exist at that commit or the command fails.
func (g *ExecGitDiffAnalyzer) GetFileAtCommit(ctx context.Context, repoPath, file, commit string) ([]byte, error) {
	ref := commit + ":" + file
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "show", ref)
	proc.HideConsoleWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s in %s: %w", ref, repoPath, err)
	}
	return out, nil
}

// GetSessionCommits returns commit hashes authored within [since, until).
// Hashes are returned in chronological order (oldest first).
// Non-fatal: callers must treat errors as "no commits" and continue.
func (g *ExecGitDiffAnalyzer) GetSessionCommits(ctx context.Context, repoPath string, since, until time.Time) ([]string, error) {
	logTimeout := g.logTimeout()
	ctx, cancel := context.WithTimeout(ctx, logTimeout)
	defer cancel()

	listCmd := exec.CommandContext(ctx,
		"git", "-C", repoPath, "log",
		"--since="+since.Format(time.RFC3339),
		"--until="+until.Format(time.RFC3339),
		"--diff-filter=M",
		"--pretty=format:%H",
		"--reverse",
	)
	proc.HideConsoleWindow(listCmd)
	out, err := listCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log (hashes) in %s: %w", repoPath, err)
	}

	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	return strings.Split(string(trimmed), "\n"), nil
}

// GetSessionCommitsWithMetadata returns full CommitInfo records for commits
// authored within [since, until) in the session's repository. Commits are
// returned in chronological order.
//
// A session starts in one checkout but may commit in a linked worktree it
// creates (the per-branch worktree workflow the project uses). Those commits are
// on a different branch, so reading only the checkout this session started in
// would miss the commit that opened the pull request. The read therefore covers
// every worktree git reports for the repository, and deduplicates by hash.
// Author email and the session window still bound the result; CommitDetector's
// transcript-activity gate remains the "did this session touch git" check.
//
// Streaming behaviour:
//   - Reads git log output via bufio.Scanner (O(BatchSize) memory, not O(TotalCommits)).
//   - Caps results at MaxCommits; hitting the cap is silent (no error).
//   - Times out once after LogTimeout across all worktrees; partial results are
//     returned with a diagnostic error.
//   - Git command failures return partial results with a diagnostic error.
//
// Non-fatal: callers must record the returned error as a DiagnosticEntry warning
// and continue session ingestion with the partial (or empty) commits list.
func (g *ExecGitDiffAnalyzer) GetSessionCommitsWithMetadata(ctx context.Context, repoPath string, since, until time.Time) ([]CommitInfo, error) {
	logTimeout := g.logTimeout()
	maxCommits := g.maxCommits()

	// Wrap context with per-call timeout.
	ctx, cancel := context.WithTimeout(ctx, logTimeout)
	defer cancel()

	seen := make(map[string]bool)
	commits := make([]CommitInfo, 0, min(maxCommits, 64))
	var failures []error
	for i, path := range g.sessionCommitLogPaths(ctx, repoPath) {
		// The session's own checkout is always read, even after the deadline,
		// so a timeout is reported instead of silently returning no commits.
		if i > 0 && ctx.Err() != nil {
			break
		}
		found, err := g.logSessionCommits(ctx, path, since, until, maxCommits)
		if err != nil {
			failures = append(failures, err)
		}
		for _, commit := range found {
			if seen[commit.Hash] {
				continue
			}
			seen[commit.Hash] = true
			commits = append(commits, commit)
		}
		if len(commits) >= maxCommits {
			break
		}
	}

	// Each checkout's log is chronological. A stable sort keeps that order for
	// commits made within the same instant, which test repositories and busy
	// sessions commonly do.
	sort.SliceStable(commits, func(i, j int) bool {
		return commits[i].CommitTime < commits[j].CommitTime
	})
	if len(commits) > maxCommits {
		commits = commits[:maxCommits]
	}
	return commits, errors.Join(failures...)
}

// sessionCommitLogPaths lists the repository's worktrees, the one the session
// started in first. A failed listing still yields that checkout: the session's
// own worktree is the one read that must never be skipped.
func (g *ExecGitDiffAnalyzer) sessionCommitLogPaths(ctx context.Context, repoPath string) []string {
	paths := []string{repoPath}
	seen := map[string]bool{repoPath: true}
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "list", "--porcelain")
	proc.HideConsoleWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return paths
	}
	for _, line := range strings.Split(string(out), "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok || path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

// logSessionCommits reads one checkout's log for the session window.
func (g *ExecGitDiffAnalyzer) logSessionCommits(ctx context.Context, repoPath string, since, until time.Time, maxCommits int) ([]CommitInfo, error) {
	// git log with strict ISO-8601 dates (%aI, %cI) for RFC3339 parsing.
	// --diff-filter=M: only commits that modified files (reduces output in large repos).
	// --reverse: oldest first, allows early termination when cap is hit.
	cmd := exec.CommandContext(ctx,
		"git", "-C", repoPath, "log",
		"--since="+since.Format(time.RFC3339),
		"--until="+until.Format(time.RFC3339),
		"--diff-filter=M",
		"--pretty=format:%H%n%an%n%ae%n%aI%n%cI%n%s%n---",
		"--reverse",
	)
	proc.HideConsoleWindow(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return []CommitInfo{}, fmt.Errorf(
			"git log in %s: StdoutPipe: %w (check git is installed and repository exists)",
			repoPath, err,
		)
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close() //nolint:errcheck
		return []CommitInfo{}, fmt.Errorf(
			"git log in %s: Start: %w (check git is installed and repository exists)",
			repoPath, err,
		)
	}

	commits := make([]CommitInfo, 0, min(maxCommits, 64))
	var current CommitInfo
	lineIdx := 0

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() && len(commits) < maxCommits {
		line := scanner.Text()

		if line == "---" {
			// End of commit record: flush to results.
			commits = append(commits, current)
			current = CommitInfo{}
			lineIdx = 0
			continue
		}

		// Parse fields by line position within each commit record.
		switch lineIdx {
		case 0: // full commit hash
			current.Hash = line
		case 1: // author display name
			current.AuthorName = line
		case 2: // author email
			current.AuthorEmail = line
		case 3: // author date (strict ISO 8601 via %aI → parses as RFC3339)
			if t, err := time.Parse(time.RFC3339, line); err == nil {
				current.AuthorTime = t.UnixMilli()
			}
		case 4: // commit date (strict ISO 8601 via %cI → parses as RFC3339)
			if t, err := time.Parse(time.RFC3339, line); err == nil {
				current.CommitTime = t.UnixMilli()
			}
		case 5: // commit subject (first line of message via %s)
			current.Message = line
		}
		lineIdx++
	}

	// Close our read end of the pipe before waiting for git to exit.
	// If we stopped reading early (cap hit), git may be blocked writing to a full
	// pipe buffer. Closing our end delivers SIGPIPE to git, causing it to exit
	// immediately rather than waiting for the 5s context deadline.
	// cmd.Wait() also closes this via closeAfterWait; the second close is a no-op.
	_ = stdout.Close() //nolint:errcheck

	// Wait for process exit. Ignore non-zero exits caused by broken pipe (cap hit)
	// or context cancellation — those are expected and handled below.
	_ = cmd.Wait()

	// Timeout: return partial commits with diagnostic error.
	if ctx.Err() == context.DeadlineExceeded {
		return commits, fmt.Errorf(
			"git log in %s: operation timed out after %v; returning %d commits collected before timeout "+
				"(large repository — run 'peasant ingest --force' to retry)",
			repoPath, g.logTimeout(), len(commits),
		)
	}

	// Scanner error (e.g. broken pipe from cap hit): not a caller-visible error.
	// We intentionally stopped reading; any scan error here is our own doing.
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return commits, fmt.Errorf(
			"git log in %s: reading output: %w",
			repoPath, err,
		)
	}

	return commits, nil
}
