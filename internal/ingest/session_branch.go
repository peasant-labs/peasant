package ingest

import (
	"context"
	"maps"
	"path/filepath"
	"time"
)

// BranchSource says where a harness's sessions can get a branch from.
type BranchSource uint8

const (
	// BranchSourceUnknown is the zero value; no harness declares it.
	BranchSourceUnknown BranchSource = iota
	// BranchSourceRecordingOrCheckout is the general rule: the branch the
	// session recorded, and for a session that recorded none the branch checked
	// out in its directory.
	BranchSourceRecordingOrCheckout
	// BranchSourceRecordingOnly is for a harness that takes a session's branch
	// only from its own recording. A session that recorded none has no branch;
	// the checkout never answers for it.
	BranchSourceRecordingOnly
)

// String returns the declaration token, or "unknown".
func (s BranchSource) String() string {
	switch s {
	case BranchSourceRecordingOrCheckout:
		return "recording-or-checkout"
	case BranchSourceRecordingOnly:
		return "recording-only"
	default:
		return "unknown"
	}
}

// AsksCheckout reports whether a session that recorded no branch may be placed
// by the branch checked out in its directory.
func (s BranchSource) AsksCheckout() bool { return s != BranchSourceRecordingOnly }

// harnessBranchSources declares, once per registered harness, where its
// sessions get a branch from. It is keyed like DefaultAdapterRegistry and
// HarvesterVersionRegistry, and a test holds the three key sets equal.
// SessionBranchFor reads it, so the tree, the import filter and the store
// cannot disagree about a harness. It is unexported and read through
// HarnessBranchSources, which hands out a copy, so no caller can change a
// declaration.
//
// Codex records its git context once in session_meta and Strike keeps its branch
// in a sidecar. Neither extractor ever asks the checkout, so a session of theirs
// that recorded no branch has none, in the store and everywhere else.
var harnessBranchSources = map[Harness]BranchSource{
	HarnessClaudeCode: BranchSourceRecordingOrCheckout,
	HarnessCursor:     BranchSourceRecordingOrCheckout,
	HarnessOpenCode:   BranchSourceRecordingOrCheckout,
	HarnessPi:         BranchSourceRecordingOrCheckout,
	HarnessCodex:      BranchSourceRecordingOnly,
	HarnessStrike:     BranchSourceRecordingOnly,
}

// HarnessBranchSources returns a copy of the declarations, one per registered
// harness.
func HarnessBranchSources() map[Harness]BranchSource {
	return maps.Clone(harnessBranchSources)
}

// BranchSourceOf returns where a harness's sessions get a branch from. A
// harness with no declaration gets the general rule, which is what every
// harness had before the declarations existed.
func BranchSourceOf(harness Harness) BranchSource {
	if source, declared := harnessBranchSources[harness]; declared {
		return source
	}
	return BranchSourceRecordingOrCheckout
}

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
	return SessionProjectDirectoryIn(session, osDirectoryExists)
}

// SessionProjectDirectoryIn is SessionProjectDirectory over any filesystem: an
// extractor that reads through its adapter's filesystem decodes a Claude
// transcript folder against that filesystem, and the tree decodes it against the
// disk. dirExists says whether a path is a directory.
func SessionProjectDirectoryIn(session DiscoveredSession, dirExists func(string) bool) string {
	if filepath.IsAbs(session.CWD) {
		return filepath.Clean(session.CWD)
	}
	if session.Harness == HarnessClaudeCode {
		slug := filepath.Base(filepath.Dir(string(session.SourcePath)))
		if decoded := DecodeClaudeSlug(slug, dirExists); decoded != "" {
			return decoded
		}
	}
	return ""
}

// ExtractionDirectory is the directory an extractor asks git about for a session
// it is reading: the session's project directory, where recordedCWD stands in
// for a working directory the discovery did not carry. It is the directory the
// tree and the import filter ask about before the session is stored, so the
// branch and the remote an extractor stores are the ones the tree showed. It is
// empty when the session has no project directory, and git is then asked
// nothing: the harness's storage folder is never used.
func ExtractionDirectory(session DiscoveredSession, recordedCWD string, dirExists func(string) bool) string {
	if session.CWD == "" {
		session.CWD = recordedCWD
	}
	return SessionProjectDirectoryIn(session, dirExists)
}

// sessionBranchEvidence is everything that can say which branch a session
// belongs to. SessionBranchFor builds it from the discovered session and the
// known-session index, so the tree and the import filter cannot build it two ways.
type sessionBranchEvidence struct {
	// Harness is the harness that recorded the session; HarnessBranchSources
	// says whether the checkout may answer for it.
	Harness Harness
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

// SessionBranchFor is the branch a discovered session belongs to. It builds the
// evidence for the session from what its transcript recorded and from what the
// known-session index holds for it (record and stored are the result of
// KnownSessionIndex.Reusable; the zero record and false when there is none), and
// lets resolveSessionBranch decide. The tree and the import filter both call
// it, so neither can build the evidence another way.
func SessionBranchFor(ctx context.Context, git GitResolver, session DiscoveredSession, record KnownSession, stored bool) string {
	return resolveSessionBranch(ctx, git, sessionBranchEvidence{
		Harness:      session.Harness,
		Recorded:     session.Branch,
		StoredBranch: record.Branch,
		StoredKnown:  stored,
		Directory:    SessionProjectDirectory(session),
	})
}

// resolveSessionBranch is the precedence SessionBranchFor applies, for the tree
// and for the import alike:
//
//  1. the branch the transcript recorded;
//  2. the branch the store holds for a session that is already stored and whose
//     source has not changed since, even when that import found no branch;
//  3. the branch checked out in Directory now (ResolveGitBranch), unless the
//     harness takes its branch only from its own recording
//     (BranchSourceRecordingOnly), in which case the session has no branch.
//
// A stored session is not asked about again: the earlier import already looked
// at git for it, and walking git for every such session on every scan is the
// cost the index exists to avoid. A session whose source changed is not stored
// evidence, so it falls through to the checkout.
func resolveSessionBranch(ctx context.Context, git GitResolver, evidence sessionBranchEvidence) string {
	if evidence.Recorded == "" && evidence.StoredKnown {
		return NamedBranch(evidence.StoredBranch)
	}
	if evidence.Recorded == "" && !BranchSourceOf(evidence.Harness).AsksCheckout() {
		return ""
	}
	return ResolveGitBranch(ctx, git, evidence.Directory, evidence.Recorded)
}
