package ingest

import (
	"context"
	"path/filepath"
)

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
	// sessionRemoteDirectory names it.
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
		Directory:    sessionRemoteDirectory(session),
	})
}

// resolveSessionRemote is the precedence SessionRemoteFor applies, the same
// family as the branch's:
//
//  1. the remote the store holds for a session that is already stored and whose
//     source has not changed since, even when that import found no remote;
//  2. the remote of the one directory sessionRemoteDirectory names.
//
// A stored session is not asked about again: the earlier import already looked
// at git for it. A session whose source changed is not stored evidence, so it
// falls through to the directory.
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

// sessionRemoteDirectory is the one directory a session's remote is read from:
// its project directory (SessionProjectDirectory) or, for a session with none,
// the directory the harness found the recording in.
func sessionRemoteDirectory(session DiscoveredSession) string {
	if dir := SessionProjectDirectory(session); dir != "" {
		return dir
	}
	if root := string(session.OriginalRoot); root != "" {
		return root
	}
	if session.SourcePath != "" {
		return filepath.Dir(string(session.SourcePath))
	}
	return ""
}
