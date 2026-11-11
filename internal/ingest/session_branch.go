package ingest

import (
	"context"
	"path/filepath"
	"time"
)

// KnownSession is what a previous ingest recorded for one session. A scan
// reuses it instead of walking git again when the source has not changed.
type KnownSession struct {
	GitRemote     string
	Branch        string
	GitWorktree   string
	CanonicalCwd  string
	Title         string
	IngestedMs    int64
	SchemaVersion int
}

// WorkingDirectory returns the most specific project directory already
// recorded. A per-session Git worktree wins over the project-level canonical
// directory. Discovery still wins over both when the current source carries a
// working directory.
func (s KnownSession) WorkingDirectory() string {
	if s.GitWorktree != "" {
		return s.GitWorktree
	}
	return s.CanonicalCwd
}

// KnownSessionIndex answers whether a discovered session can reuse a recorded
// git resolution. A nil index means every session is resolved the full way.
// Implementations must not require this package to import a database type.
//
// The tree a person chooses from and the import filter that follows it read the
// same index, so a stored session is placed on one branch by both.
type KnownSessionIndex interface {
	Reusable(session DiscoveredSession, staleness time.Duration) (KnownSession, bool)
}

// SessionProjectDirectory is the one directory a session's checkout is read
// from: the absolute working directory the session ran in or, for a Claude Code
// session that recorded none, the project directory its transcript folder
// decodes to. It is empty when neither exists.
//
// The working directory is used as recorded, not replaced by its repository
// root. A checkout answers for every folder inside it, so a subfolder needs no
// translation, while a linked worktree has its own checked-out branch that only
// its own directory reports. The harness's storage directory is never used: it
// can sit inside an unrelated repository.
func SessionProjectDirectory(session DiscoveredSession) string {
	if filepath.IsAbs(session.CWD) {
		return filepath.Clean(session.CWD)
	}
	if session.Harness == HarnessClaudeCode {
		slug := filepath.Base(filepath.Dir(string(session.SourcePath)))
		if decoded := DecodeClaudeSlugOnDisk(slug); decoded != "" {
			return decoded
		}
	}
	return ""
}

// SessionBranchEvidence is everything that can say which branch a session
// belongs to. Both the tree and the import filter build it from the same
// discovered session and the same known-session index.
type SessionBranchEvidence struct {
	// Recorded is the branch the session's transcript recorded.
	Recorded string
	// StoredBranch is the branch an earlier import stored for the session. It
	// counts only when StoredKnown is set: the session is stored and its source
	// has not changed since, which is what KnownSessionIndex.Reusable reports.
	StoredBranch string
	StoredKnown  bool
	// Directory is the one directory whose checked-out branch may answer last;
	// SessionProjectDirectory names it.
	Directory string
}

// ResolveSessionBranch is the one precedence for which branch a session belongs
// to, for the tree and for the import alike:
//
//  1. the branch the transcript recorded;
//  2. the branch the store holds for a session that is already stored and whose
//     source has not changed since, even when that import found no branch;
//  3. the branch checked out in Directory now (ResolveGitBranch).
//
// A stored session is not asked about again: the earlier import already looked
// at git for it, and walking git for every such session on every scan is the
// cost the index exists to avoid. A session whose source changed is not stored
// evidence, so it falls through to the checkout.
func ResolveSessionBranch(ctx context.Context, git GitResolver, evidence SessionBranchEvidence) string {
	if evidence.Recorded == "" && evidence.StoredKnown {
		return evidence.StoredBranch
	}
	return ResolveGitBranch(ctx, git, evidence.Directory, evidence.Recorded)
}
