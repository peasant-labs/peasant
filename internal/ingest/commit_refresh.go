package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

// commitRefreshEmailTimeout matches the extraction path's cap on the git
// config read. A locked config file must not stall the run.
const commitRefreshEmailTimeout = 2 * time.Second

// refreshesUnchangedCommits reports whether this run should detect commits
// for a session the diff left unchanged. It is opt-in: a plain scoped harvest
// does not, so it emits no detector warning. A full harvest does not, even
// when the flag is set. A dry run writes nothing. Detection is absent when no
// analyzer is attached.
func (p *Pipeline) refreshesUnchangedCommits() bool {
	asked := p.config.RefreshCommits
	named := p.config.Scope != nil
	writing := !p.config.DryRun && p.store != nil
	detecting := p.gitAnalyzer != nil
	return asked && named && writing && detecting
}

// refreshUnchangedCommits runs the extraction path's commit detector for each
// unchanged session a scoped run will not re-extract, and writes only the
// commit projection. It does not extract entries, rewrite the index, or write
// a transcript. A detection or write failure is a diagnostic; it does not
// fail the run.
func (p *Pipeline) refreshUnchangedCommits(ctx context.Context, entries []DiffEntry, reprocessing map[SessionID]bool) {
	if !p.refreshesUnchangedCommits() || len(entries) == 0 {
		return
	}
	for _, entry := range entries {
		if reprocessing[entry.Session.SessionID] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		p.refreshOneUnchangedCommit(ctx, entry.Session, nil, nil)
	}
}

// refreshCapturedUnchangedCommit detects commits for a session the worker has
// already proved unchanged from captured bytes. The capture is not written.
// A changed projection is written on the store lane so it cannot overlap
// another session's write.
func (p *Pipeline) refreshCapturedUnchangedCommit(ctx context.Context, session DiscoveredSession, transcript []byte, lane *storeWriteLane) {
	if !p.refreshesUnchangedCommits() {
		return
	}
	p.refreshOneUnchangedCommit(ctx, session, transcript, lane)
}

func (p *Pipeline) refreshOneUnchangedCommit(ctx context.Context, session DiscoveredSession, transcript []byte, lane *storeWriteLane) {
	meta, ok := p.storedCommitMetadata(ctx, session)
	if !ok {
		return
	}
	repoPath := storedCommitRepoPath(meta)
	if repoPath == "" || meta.Timestamp.End == 0 {
		// The extraction path skips detection in the same cases: no repository
		// to query, or a session that is still running.
		return
	}
	transcriptPath, transcriptKnown := p.commitRefreshTranscript(session)
	if !transcriptKnown {
		p.reportSessionDiagnostic(session.SessionID, DiagnosticEntry{
			ErrorType:   "commit_refresh_skipped",
			Location:    string(session.SessionID),
			Message:     fmt.Sprintf("commit refresh for session %s skipped command validation: the stored managed transcript path is not recorded", session.SessionID),
			Remediation: "Run peasant harvest --session " + string(session.SessionID) + " --force once so the managed transcript is recorded, then open the session again. Stored entries and the index were not changed.",
		})
		return
	}

	userEmail := ""
	if p.git != nil {
		emailCtx, cancel := context.WithTimeout(ctx, commitRefreshEmailTimeout)
		userEmail, _ = p.git.UserEmail(emailCtx)
		cancel()
	}
	reader := p.commitTranscriptReader
	if len(transcript) > 0 {
		// The worker already read this transcript to prove the session
		// unchanged. Reuse those bytes instead of reading the file again.
		reader = capturedTranscriptReader{data: transcript}
		if transcriptPath == "" {
			transcriptPath = string(session.SessionID)
		}
	}
	detector := newCommitDetectorWithReader(p.gitAnalyzer, userEmail, reader)
	commits, diags := detector.LayeredDetection(ctx, repoPath, time.UnixMilli(meta.Timestamp.Start), time.UnixMilli(meta.Timestamp.End), transcriptPath)
	for _, diagnostic := range diags {
		p.reportSessionDiagnostic(session.SessionID, diagnostic)
	}
	preserve := len(diags) > 0
	write := func() { p.persistRefreshedCommits(ctx, session.SessionID, commits, preserve) }
	if lane != nil {
		p.runStoreWrite(lane, write)
		return
	}
	write()
}

func (p *Pipeline) storedCommitMetadata(ctx context.Context, session DiscoveredSession) (UnifiedMetadata, bool) {
	reader, ok := p.store.(StoredMetadataReader)
	if !ok {
		p.reportSessionDiagnostic(session.SessionID, DiagnosticEntry{
			ErrorType:   "commit_refresh_skipped",
			Location:    string(session.SessionID),
			Message:     fmt.Sprintf("commit refresh for session %s skipped: the configured store cannot read stored metadata", session.SessionID),
			Remediation: "Use the production store. Stored entries and the index were not changed.",
		})
		return UnifiedMetadata{}, false
	}
	data, err := reader.ReadStoredMetadata(ctx, session.SessionID)
	if err != nil {
		p.reportSessionDiagnostic(session.SessionID, DiagnosticEntry{
			ErrorType:   "commit_refresh_failed",
			Location:    string(session.SessionID),
			Message:     fmt.Sprintf("commit refresh for session %s could not read stored metadata: %s", session.SessionID, err),
			Remediation: "Restore database access and run peasant open --session " + string(session.SessionID) + " again. Stored entries and the index were not changed.",
		})
		return UnifiedMetadata{}, false
	}
	if len(data) == 0 {
		p.reportSessionDiagnostic(session.SessionID, DiagnosticEntry{
			ErrorType:   "commit_refresh_skipped",
			Location:    string(session.SessionID),
			Message:     fmt.Sprintf("commit refresh for session %s skipped: the store has no recorded metadata for it", session.SessionID),
			Remediation: "Record the session with peasant harvest --session " + string(session.SessionID) + " --force, then open it again. Stored entries and the index were not changed.",
		})
		return UnifiedMetadata{}, false
	}
	var meta UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		p.reportSessionDiagnostic(session.SessionID, DiagnosticEntry{
			ErrorType:   "commit_refresh_failed",
			Location:    string(session.SessionID),
			Message:     fmt.Sprintf("commit refresh for session %s could not read stored metadata: %s", session.SessionID, err),
			Remediation: "Record the session again with peasant harvest --session " + string(session.SessionID) + " --force. Stored entries and the index were not changed.",
		})
		return UnifiedMetadata{}, false
	}
	return meta, true
}

func storedCommitRepoPath(meta UnifiedMetadata) string {
	if meta.Git.Worktree != nil && *meta.Git.Worktree != "" {
		return *meta.Git.Worktree
	}
	return meta.Project.FilePath
}

// commitRefreshTranscript is the transcript the extraction path would hand the
// detector. A file origin uses the source file. The current SQLite origin uses
// the already installed managed transcript, never a newly written one. Other
// origins are timestamp-only, as extraction is.
func (p *Pipeline) commitRefreshTranscript(session DiscoveredSession) (path string, known bool) {
	switch session.TranscriptOrigin {
	case TranscriptOriginFile:
		path = session.SourcePath.String()
		return path, path != ""
	case TranscriptOriginOpenCodeCurrentSQLite:
		return p.installedTranscriptPath(session)
	default:
		return "", true
	}
}

func (p *Pipeline) installedTranscriptPath(session DiscoveredSession) (string, bool) {
	loc, ok := p.locationCache[session.SessionID]
	if !ok || loc.HostSlug == "" {
		return "", false
	}
	ext := string(session.SourceFormat)
	if ext == "" {
		ext = string(SourceFormatJSON)
	}
	dir := SessionDir(string(p.config.OutputDir), loc.HostSlug, string(session.SessionID), loc.ParentID)
	return filepath.Join(dir, string(session.SessionID)+"--transcript."+ext), true
}

func (p *Pipeline) persistRefreshedCommits(ctx context.Context, id SessionID, commits []CommitInfo, preserveExisting bool) {
	writer, ok := p.store.(SessionCommitProjectionStore)
	if !ok {
		p.reportSessionDiagnostic(id, DiagnosticEntry{
			ErrorType:   "commit_refresh_skipped",
			Location:    string(id),
			Message:     fmt.Sprintf("commit refresh for session %s skipped: the configured store cannot write the commit projection without clearing the saved pair identity", id),
			Remediation: "Use the production store. Stored entries, the index, and the saved pair identity were not changed.",
		})
		return
	}
	current, err := writer.ListSessionCommits(ctx, id)
	if err != nil {
		p.reportSessionDiagnostic(id, DiagnosticEntry{
			ErrorType:   "commit_refresh_failed",
			Location:    string(id),
			Message:     fmt.Sprintf("commit refresh for session %s could not read the current commit projection: %s", id, err),
			Remediation: "Restore database access and run peasant open --session " + string(id) + " again. Stored entries and the index were not changed.",
		})
		return
	}
	// An identical projection is not rewritten. Replacing it would allocate new
	// rows for the same commits and is the only work this refresh can avoid.
	if commitProjectionEqual(current, commits) {
		return
	}
	if err := writer.WriteSessionCommitProjection(ctx, id, commits, preserveExisting); err != nil {
		p.reportSessionDiagnostic(id, DiagnosticEntry{
			ErrorType:   "commit_refresh_failed",
			Location:    string(id),
			Message:     fmt.Sprintf("commit refresh for session %s could not store the detected commits: %s", id, err),
			Remediation: "Restore database access and run peasant open --session " + string(id) + " again. Stored entries, the index, and the saved pair identity were not changed.",
		})
	}
}

// capturedTranscriptReader serves the bytes a capture already holds, so
// command validation does not read the source a second time.
type capturedTranscriptReader struct {
	data []byte
}

func (r capturedTranscriptReader) ReadTranscript(context.Context, string) ([]byte, error) {
	return r.data, nil
}

func sameCommitFields(left, right CommitInfo) bool {
	if left.Message != right.Message || left.AuthorName != right.AuthorName {
		return false
	}
	if left.AuthorEmail != right.AuthorEmail || left.CommitTime != right.CommitTime {
		return false
	}
	return left.AuthorTime == right.AuthorTime
}

func commitProjectionEqual(current, detected []CommitInfo) bool {
	if len(current) != len(detected) {
		return false
	}
	byHash := make(map[string]CommitInfo, len(current))
	for _, commit := range current {
		if _, duplicate := byHash[commit.Hash]; duplicate {
			return false
		}
		byHash[commit.Hash] = commit
	}
	for _, commit := range detected {
		got, ok := byHash[commit.Hash]
		if !ok || !sameCommitFields(got, commit) {
			return false
		}
	}
	return true
}
