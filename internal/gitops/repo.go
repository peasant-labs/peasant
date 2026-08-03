// Package gitops provides a read-only local-git query layer for the Map and
// Review surfaces. It is a repo-state concern (branches, merge state, file
// listings, commit metadata) — orthogonal to the session-context git helpers
// in internal/ingest (GitResolver, GitDiffAnalyzer), which it deliberately
// does not import.
package gitops

import (
	"context"
	"fmt"
)

// FileStatus is a typed git diff status code (newtype pattern).
// Only the statuses the Map/Review wire contract models are valid;
// unmodeled codes (copies, typechanges, unmerged) are rejected by the
// constructor and skipped by callers.
type FileStatus string

const (
	// FileStatusModified is a modified file ("M").
	FileStatusModified FileStatus = "M"
	// FileStatusAdded is an added file ("A").
	FileStatusAdded FileStatus = "A"
	// FileStatusDeleted is a deleted file ("D").
	FileStatusDeleted FileStatus = "D"
	// FileStatusRenamed is a renamed file ("R"). Git emits a similarity
	// score suffix ("R100"); NewFileStatus strips it.
	FileStatusRenamed FileStatus = "R"
)

// String returns the single-letter git status code.
func (s FileStatus) String() string { return string(s) }

// IsValid reports whether s is one of the modeled status codes.
func (s FileStatus) IsValid() bool {
	switch s {
	case FileStatusModified, FileStatusAdded, FileStatusDeleted, FileStatusRenamed:
		return true
	}
	return false
}

// NewFileStatus parses a raw `git diff --name-status` code (e.g. "M", "A",
// "D", "R100") into a FileStatus. Rename/copy similarity-score suffixes are
// stripped. Returns an error for empty or unmodeled codes (C, T, U, X, B).
func NewFileStatus(raw string) (FileStatus, error) {
	if raw == "" {
		return "", fmt.Errorf("gitops: empty file status code")
	}
	s := FileStatus(raw[:1])
	if !s.IsValid() {
		return "", fmt.Errorf("gitops: unmodeled file status code %q", raw)
	}
	return s, nil
}

// FileChange is one file-level delta between a branch and its merge-base
// with the default branch.
type FileChange struct {
	// Path is the repo-relative path (the new path for renames).
	Path string
	// Status is the typed diff status.
	Status FileStatus
	// OldPath is the rename source; set only when Status == FileStatusRenamed.
	OldPath *string
}

// FileDiffStat is the per-file line delta from `git diff --numstat`.
type FileDiffStat struct {
	// Path is the repo-relative path (the new path for renames).
	Path string
	// Added is the number of added lines. 0 for binary files.
	Added int
	// Removed is the number of removed lines. 0 for binary files.
	Removed int
}

// DiffStats aggregates line-level diff statistics between two refs.
// LinesAdded/LinesRemoved are the totals over PerFile; binary files
// (numstat "-" placeholders) contribute 0/0 but keep their PerFile row.
type DiffStats struct {
	// LinesAdded is the total added-line count across all files.
	LinesAdded int
	// LinesRemoved is the total removed-line count across all files.
	LinesRemoved int
	// PerFile holds the per-file line deltas.
	PerFile []FileDiffStat
}

// DiffLineKind classifies one line within a unified-diff hunk.
type DiffLineKind string

const (
	// DiffLineContext is an unchanged context line (shown for orientation).
	DiffLineContext DiffLineKind = "context"
	// DiffLineAdded is a line present only in head ("+").
	DiffLineAdded DiffLineKind = "add"
	// DiffLineRemoved is a line present only in base ("-").
	DiffLineRemoved DiffLineKind = "del"
)

// DiffLine is one line within a hunk. Text excludes the leading +/-/space
// marker and the trailing newline. Line numbers are derivable by the renderer
// from the hunk's OldStart/NewStart plus position, so they are not duplicated
// per line (keeps large diffs lean).
type DiffLine struct {
	Kind DiffLineKind
	Text string
}

// Hunk is one "@@ -oldStart,oldLines +newStart,newLines @@" section of a file
// diff, with its lines in order.
type Hunk struct {
	OldStart int
	OldLines int
	NewStart int
	NewLines int
	// Header is the optional section heading git prints after the second "@@"
	// (often the enclosing function); empty when git emits none.
	Header string
	Lines  []DiffLine
}

// FileDiff is the hunk-level diff for one changed file, between a branch and
// its merge-base with the default branch.
type FileDiff struct {
	// Path is the repo-relative path (the new path for renames).
	Path string
	// OldPath is the rename source; set only when Status == FileStatusRenamed.
	OldPath *string
	// Status is the typed diff status.
	Status FileStatus
	// Binary is true for binary files (git emits no textual hunks).
	Binary bool
	// Hunks are the textual change hunks, in file order. Empty for binary files
	// and pure renames with no content change.
	Hunks []Hunk
	// Truncated is true when the file's diff exceeded the line cap and was cut
	// short — the UI says so rather than implying the change is complete.
	Truncated bool
}

// BranchState describes a branch measured against the default branch.
type BranchState struct {
	// Name is the branch name (no refs/ prefix).
	Name string
	// MergeBase is the merge-base commit hash with the default branch.
	// Additive to the contract field list: consumers (codemap) need it for
	// ChangeDetailPayload.BaseRef and historical structure reads.
	MergeBase string
	// AheadCount is the number of commits on the branch not on the default branch.
	AheadCount int
	// BehindCount is the number of commits on the default branch not on the branch.
	BehindCount int
	// ChangedFiles is the merge-base..branch diff with rename detection (-M).
	ChangedFiles []FileChange
}

// MergedBranch is a local branch whose tip is reachable from the default branch.
type MergedBranch struct {
	// Name is the branch name (no refs/ prefix).
	Name string
	// MergedAtMs is the merge-commit time in Unix ms. For fast-forward merges
	// (no merge commit) it falls back to the branch tip's committer time.
	// 0 when neither could be determined.
	MergedAtMs int64
	// MergeCommit is the hash of the merge commit on the default branch.
	// Empty for fast-forward merges (no merge commit exists).
	MergeCommit string
}

// Commit is lightweight commit metadata for time strips and rail panels.
type Commit struct {
	// Hash is the full commit hash.
	Hash string
	// Subject is the first line of the commit message.
	Subject string
	// TimeMs is the committer time in Unix ms.
	TimeMs int64
	// AuthorEmail is the commit author's email.
	AuthorEmail string
}

// Repository is the read-only local-git query interface used by the codemap
// service. Production: ExecGitRepository. Tests: testutil.StubGitRepository.
type Repository interface {
	// DefaultBranch returns the default branch name: origin/HEAD when set,
	// otherwise the first of main/master/develop/trunk that exists locally.
	// Errors when no default branch can be determined.
	DefaultBranch(ctx context.Context) (string, error)
	// Branches lists local branch names, excluding the default branch.
	Branches(ctx context.Context) ([]string, error)
	// BranchState compares branch to its merge-base with the default branch:
	// ahead/behind counts and changed files with rename detection (-M).
	BranchState(ctx context.Context, branch string) (*BranchState, error)
	// DiffStats returns line-level diff statistics for head measured against
	// its merge-base with base (git diff --numstat base...head, three-dot
	// merge-base semantics — the same baseline BranchState diffs against).
	// Binary files appear in PerFile with 0/0 counts.
	DiffStats(ctx context.Context, base, head string) (*DiffStats, error)
	// DiffHunks returns the per-file unified-diff hunks for head measured
	// against its merge-base with base (git diff base...head, the same baseline
	// as DiffStats). When paths is non-empty only those files are diffed (the
	// lazy per-file path the Review detail uses). maxLinesPerFile caps the lines
	// kept per file (<= 0 uses the package default); over-cap files come back
	// with Truncated=true. Binary files come back with Binary=true and no hunks.
	DiffHunks(ctx context.Context, base, head string, paths []string, maxLinesPerFile int) ([]FileDiff, error)
	// MergedBranches lists local branches merged into the default branch,
	// most recently merged first, capped at limit (limit <= 0 means no cap).
	MergedBranches(ctx context.Context, limit int) ([]MergedBranch, error)
	// FileAtCommit returns the file content at a given commit (git show commit:path).
	FileAtCommit(ctx context.Context, commit, path string) ([]byte, error)
	// FilesAtCommit returns the contents of many paths at a commit in one
	// bounded git call (cat-file --batch) — one process for N files instead
	// of one `git show` per file. Paths missing at the commit (and non-blob
	// entries like submodules) are absent from the result map, not errors.
	FilesAtCommit(ctx context.Context, commit string, paths []string) (map[string][]byte, error)
	// ListFiles lists all tracked files at ref (git ls-tree -r --name-only ref).
	ListFiles(ctx context.Context, ref string) ([]string, error)
	// Commits returns commit metadata for ref, newest first, capped at limit
	// (limit <= 0 means no cap).
	Commits(ctx context.Context, ref string, limit int) ([]Commit, error)
	// CommitsInRange returns the hashes reachable from head but not from base
	// (i.e. commits ahead of the merge-base), newest first.
	CommitsInRange(ctx context.Context, base, head string) ([]string, error)
	// BlameCommits returns, for each 1-based line of path at ref, the full
	// commit hash that last modified that line (git blame). out[0] is line 1.
	// Used to attribute a diff hunk's new lines to the commit — and thence the
	// recorded conversation — that wrote them. A path absent at ref yields an
	// empty slice, not an error.
	BlameCommits(ctx context.Context, ref, path string) ([]string, error)
	// RevertedCommits returns the set of commit hashes that a "git revert" on
	// ref undid — parsed from the "This reverts commit <hash>" trailer that
	// git writes into revert-commit messages. Hashes may be abbreviated; callers
	// match by prefix. Used to flag a merged change that was later reverted.
	RevertedCommits(ctx context.Context, ref string) (map[string]bool, error)
}
