package ingest

import (
	"context"
	"time"
)

// GitDiffAnalyzer provides git operations needed for M6 output survival computation.
// The production implementation invokes git commands; tests use StubGitDiffAnalyzer.
type GitDiffAnalyzer interface {
	// GetFileAtCommit returns the contents of a file at a specific git commit.
	// Returns an error if the file does not exist at that commit.
	GetFileAtCommit(ctx context.Context, repoPath, file, commit string) ([]byte, error)

	// GetSessionCommits returns commit hashes authored within [since, until).
	GetSessionCommits(ctx context.Context, repoPath string, since, until time.Time) ([]string, error)

	// GetSessionCommitsWithMetadata returns full CommitInfo records for commits
	// authored within [since, until). Used by the commit detector.
	// Non-fatal: callers must treat errors as "no commits" and continue.
	GetSessionCommitsWithMetadata(ctx context.Context, repoPath string, since, until time.Time) ([]CommitInfo, error)

	// IsAncestor reports whether commit is reachable from ref, the question
	// `git merge-base --is-ancestor <commit> <ref>` answers. (false, nil) means
	// the commit is not an ancestor of ref. A non-nil error means the question
	// could not be answered: unknown ref, git failure, or timeout. Callers must
	// treat an error as "unknown", never as "not reachable".
	IsAncestor(ctx context.Context, repoPath, commit, ref string) (bool, error)
}
