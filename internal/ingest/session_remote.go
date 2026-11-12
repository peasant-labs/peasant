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
	// Directory is the one directory whose remote answers otherwise;
	// SessionProjectDirectory names it.
	Directory string
}

// SessionRemoteFor is the git remote a discovered session belongs to. It builds
// the evidence for the session from what the known-session index holds for it
// (record and stored are the result of KnownSessionIndex.Reusable; the zero
// record and false when there is none) and lets resolveSessionRemote decide.
// The tree and the import filter both call it, as they call SessionBranchFor, so
// a session listed under one repository is judged by that repository.
func SessionRemoteFor(ctx context.Context, git GitResolver, session DiscoveredSession, record KnownSession, stored bool) string {
	return resolveSessionRemote(ctx, git, sessionRemoteEvidence{
		StoredRemote: record.GitRemote,
		StoredKnown:  stored,
		Directory:    SessionProjectDirectory(session),
	})
}

// resolveSessionRemote is the precedence SessionRemoteFor applies, the same
// family as the branch's:
//
//  1. the remote the store holds for a session that is already stored and whose
//     source has not changed since, even when that import found no remote;
//  2. the remote of the one directory SessionProjectDirectory names, the same
//     directory the branch is read from.
//
// A stored session is not asked about again: the earlier import already looked
// at git for it. A session whose source changed is not stored evidence, so it
// falls through to the directory. A session with no project directory has no
// remote: the harness's storage directory is never asked, because it can sit
// inside an unrelated repository such as a dotfiles checkout. Such a session is
// still selected by its name, its path or its session id.
func resolveSessionRemote(ctx context.Context, git GitResolver, evidence sessionRemoteEvidence) string {
	if evidence.StoredKnown {
		return evidence.StoredRemote
	}
	if git == nil || evidence.Directory == "" {
		return ""
	}
	remote, err := git.RemoteURL(ctx, evidence.Directory)
	if err != nil {
		return ""
	}
	return remote
}
