package ingest

import "context"

// sessionRemoteEvidence is everything that can say which git remote a session
// belongs to. SessionRemoteFor builds it from the discovered session and the
// known-session index, so the tree and the import filter cannot build it two
// ways.
type sessionRemoteEvidence struct {
	// StoredRemote is the remote an earlier import stored for the session. It
	// counts only when StoredKnown is set: the session is stored and its source
	// has not changed since, which is what KnownSessionIndex.Reusable reports.
	StoredRemote string
	StoredKnown  bool
	// Recorded is the branch the session's transcript recorded, and Explicit the
	// remote the harness's own recording names (DiscoveredSession.RecordedRemote).
	Recorded string
	Explicit string
	// Directory is the one directory whose repository answers otherwise;
	// SessionProjectDirectory names it.
	Directory string
}

// SessionRemoteFor is the git remote a discovered session belongs to. It builds
// the evidence for the session from what its recording names and from what the
// known-session index holds for it (record and stored are the result of
// KnownSessionIndex.Reusable; the zero record and false when there is none) and
// lets resolveSessionRemote decide. The tree and the import filter both call it,
// as they call SessionBranchFor, so a session listed under one repository is
// judged by that repository, and the extractors store the remote it names.
func SessionRemoteFor(ctx context.Context, git GitResolver, session DiscoveredSession, record KnownSession, stored bool) string {
	return resolveSessionRemote(ctx, git, sessionRemoteEvidence{
		StoredRemote: record.GitRemote,
		StoredKnown:  stored,
		Recorded:     session.Branch,
		Explicit:     session.RecordedRemote,
		Directory:    SessionProjectDirectory(session),
	})
}

// resolveSessionRemote is the precedence SessionRemoteFor applies, the same
// family as the branch's:
//
//  1. the remote the store holds for a session that is already stored and whose
//     source has not changed since, even when that import found no remote;
//  2. the remote ResolveGitRemote names for the session's recording and its
//     project directory: an explicit remote the recording carries, else the
//     upstream of the branch it recorded, else origin, else (no recorded
//     branch) the checkout's upstream. The extractors store the answer of the
//     same function over the same inputs, so the remote a session is listed
//     under before its import is the remote the import stores.
//
// A stored session is not asked about again: the earlier import already looked
// at git for it. A session whose source changed is not stored evidence, so it
// falls through to git. A session with no project directory has no remote from
// git: the harness's storage directory is never asked, because it can sit
// inside an unrelated repository such as a dotfiles checkout. Such a session is
// still selected by its name, its path or its session id. Neither the tree nor
// the extractors walk up to a parent directory's repository.
func resolveSessionRemote(ctx context.Context, git GitResolver, evidence sessionRemoteEvidence) string {
	if evidence.StoredKnown {
		return evidence.StoredRemote
	}
	return ResolveGitRemoteURL(ctx, git, evidence.Directory, evidence.Recorded, evidence.Explicit)
}
