package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// installedPair is the file names a session install writes, all as bytes the
// worker holds in memory. The transcript, its debug outputs and any private
// source-capture marker install first; the metadata installs last, so the
// presence of a fresh metadata file is the commit point of the pair: a crash
// before it leaves the old metadata naming the old transcript hash, and the
// pair refuses by that hash on the next read.
type installedPair struct {
	transcriptName string
	transcript     []byte
	debug          map[string][]byte
	sourceCapture  []byte
	metadataName   string
	metadata       []byte
}

// installManagedPair writes the session's files into a temporary directory on
// the same filesystem and installs them into sessionDir by rename, metadata
// last. It replaces only files this session's naming owns, prunes owned files
// this install no longer writes, and never touches the child subagents
// subtree. There is no sync and no lock: the database transaction is the
// durability point, and a torn install is detected on the next pair read by
// its hash. It refuses to replace a pair a newer adapter than this build
// produced.
func (p *Pipeline) installManagedPair(ctx context.Context, sessionDir, sessionID string, pair installedPair, session DiscoveredSession) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if existing, err := p.fs.ReadFile(filepath.Join(sessionDir, pair.metadataName)); err == nil {
		if headerErr := checkReplacementHeader(existing, session, p.versionTargets()); headerErr != nil {
			return headerErr
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return managedInputIOError(filepath.Join(sessionDir, pair.metadataName), err)
	}
	tmpSuffix, err := randomHex(defaults.TempSuffixLen)
	if err != nil {
		return fmt.Errorf("generate temp suffix for %s: %w", sessionID, err)
	}
	// NOTE (M14): the temporary directory is {outputDir}/.tmp-{sessionId}-{random};
	// cleanOrphans() scans {outputDir} for .tmp-* prefixes, so both must agree.
	tmpDir := fmt.Sprintf("%s/%s%s-%s", string(p.config.OutputDir), defaults.TempDirPrefix, sessionID, tmpSuffix)
	if err := p.fs.MkdirAll(tmpDir, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf("create temp dir for %s: %w", sessionID, err)
	}
	fail := func(err error) error { return errors.Join(err, p.fs.RemoveAll(tmpDir)) }
	if err := p.fs.WriteFile(filepath.Join(tmpDir, pair.transcriptName), pair.transcript, defaults.PrivateFilePerm); err != nil {
		return fail(fmt.Errorf("write transcript for %s: %w", sessionID, err))
	}
	if len(pair.debug) > 0 {
		debugDir := filepath.Join(tmpDir, defaults.DirDebug.String())
		if err := p.fs.MkdirAll(debugDir, defaults.PrivateDirPerm); err != nil {
			return fail(fmt.Errorf("create debug dir for %s: %w", sessionID, err))
		}
		for name, data := range pair.debug {
			if err := p.fs.WriteFile(filepath.Join(debugDir, name), data, defaults.PrivateFilePerm); err != nil {
				return fail(fmt.Errorf("write debug output %s for %s: %w", name, sessionID, err))
			}
		}
	}
	if len(pair.sourceCapture) > 0 {
		if err := p.fs.WriteFile(filepath.Join(tmpDir, fileCaptureEvidenceName(session.SessionID)), pair.sourceCapture, defaults.PrivateFilePerm); err != nil {
			return fail(fmt.Errorf("write source-capture marker for %s: %w", sessionID, err))
		}
	}
	if err := p.fs.WriteFile(filepath.Join(tmpDir, pair.metadataName), pair.metadata, defaults.PrivateFilePerm); err != nil {
		return fail(fmt.Errorf("write metadata for %s: %w", sessionID, err))
	}
	if err := p.replaceSessionDir(tmpDir, sessionDir, sessionID, pair.metadataName); err != nil {
		return fail(err)
	}
	return p.fs.RemoveAll(tmpDir)
}

// replaceSessionDir installs every file staged under src into dst by rename,
// installing metadataName LAST so the metadata is the commit point. It then
// prunes obsolete parent-owned files, preserving the child-owned subagents
// subtree. Rename is atomic on the same filesystem; there is no sync, no lock
// and no rollback, because the database transaction is the durability point
// and a mixed pair is refused on the next read by its hash.
func (p *Pipeline) replaceSessionDir(src, dst, sessionID, metadataName string) error {
	var ordered, metadata []string
	wanted := make(map[string]bool)
	err := p.fs.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == defaults.DirSubagents.String() {
			return fmt.Errorf("staged parent output unexpectedly contains child directory %s", path)
		}
		wanted[rel] = true
		if entry.IsDir() {
			return p.fs.MkdirAll(filepath.Join(dst, rel), defaults.PrivateDirPerm)
		}
		if filepath.Base(rel) == metadataName {
			metadata = append(metadata, rel)
		} else {
			ordered = append(ordered, rel)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("install parent files at %s: %w; existing child output remains in place; fix filesystem access or free disk space and rerun ingest", dst, err)
	}
	for _, rel := range append(ordered, metadata...) {
		if err := p.fs.Rename(filepath.Join(src, rel), filepath.Join(dst, rel)); err != nil {
			return fmt.Errorf("install parent files at %s: %w; existing child output remains in place; fix filesystem access or free disk space and rerun ingest", dst, err)
		}
	}

	// Prune obsolete parent-owned files only after every new file is installed.
	// Skip the entire child-owned tree, including children excluded by FILTER.
	var stale []string
	err = p.fs.WalkDir(dst, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		if rel == defaults.DirSubagents.String() {
			return fs.SkipDir
		}
		if rel != "." && !strings.HasPrefix(rel, sessionID+"--") && rel != defaults.DirDebug.String() && !strings.HasPrefix(rel, defaults.DirDebug.String()+"/") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !wanted[rel] {
			stale = append(stale, path)
			if entry.IsDir() {
				return fs.SkipDir
			}
		}
		return nil
	})
	if err == nil {
		for _, path := range stale {
			if err = p.fs.RemoveAll(path); err != nil {
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("prune obsolete parent files at %s: %w; installed files and child output remain in place; fix filesystem access and rerun ingest", dst, err)
	}
	return nil
}

// mirrorArtifactStore is the store the drain records installed pairs in. It is
// nil in a database-free harvest, where the files are the whole output and
// there is nothing to record.
func (p *Pipeline) mirrorArtifactStore() (ArtifactMirrorStore, error) {
	if p.store != nil {
		mirror, ok := p.store.(ArtifactMirrorStore)
		if !ok {
			return nil, fmt.Errorf("record saved sessions: configured store has no transactional artifact mirror; no session was recorded; use the production store or provide its atomic mirror capability")
		}
		return mirror, nil
	}
	if mirror, ok := p.metricsStore.(ArtifactMirrorStore); ok {
		return mirror, nil
	}
	return nil, nil
}

// mirrorDrainedBatch records every installed pair in one drain batch in the
// database, in pages of at most MirrorPageSize, so one transaction covers many
// sessions. It returns the set of sessions whose row could not be recorded:
// their files are on disk and the next harvest reads them again, so they are
// not indexed this run. Each page runs through the write lane, so its commit
// is serialized with the index writes.
func (p *Pipeline) mirrorDrainedBatch(ctx context.Context, results []workerResult, writeLane *storeWriteLane, errCh chan<- error) map[SessionID]bool {
	failed := make(map[SessionID]bool)
	mirror, err := p.mirrorArtifactStore()
	if err != nil {
		for index := range results {
			wr := &results[index]
			if wr.result.Error == nil && wr.artifact != nil {
				wr.result.Error = err
				failed[wr.result.SessionID] = true
			}
		}
		return failed
	}
	if mirror == nil {
		return failed
	}
	var page []*workerResult
	flush := func() {
		if len(page) == 0 {
			return
		}
		requests := make([]ArtifactMirrorRequest, len(page))
		for i, wr := range page {
			requests[i] = mirrorRequestFor(wr)
		}
		var outcomes []ArtifactMirrorResult
		p.runStoreWrite(writeLane, func() { outcomes = mirror.MirrorArtifacts(ctx, requests) })
		byID := make(map[SessionID]ArtifactMirrorResult, len(outcomes))
		for _, outcome := range outcomes {
			byID[outcome.SessionID] = outcome
		}
		for _, wr := range page {
			outcome, ok := byID[wr.result.SessionID]
			if ok && outcome.Err == nil && outcome.Mirrored {
				continue
			}
			cause := error(errArtifactNotMirrored)
			if ok && outcome.Err != nil {
				cause = outcome.Err
			}
			failure := fmt.Errorf("record saved session %s: %w; the saved files were kept and the next harvest reads the session again", wr.result.SessionID, cause)
			failed[wr.result.SessionID] = true
			wr.result.mirrorPending = true
			p.reportDiagnostic(DiagnosticEntry{ErrorType: "artifact_mirror_pending", Location: string(wr.result.SessionID), Message: failure.Error(), Remediation: "Restore database access and rerun harvest to record the saved session."})
			// The pair is on disk; only the row is missing. Preserve the run's
			// file counts, but push the failure so the run reports it and does
			// not authorize indexing on a session with no row.
			errCh <- failure
		}
		page = page[:0]
	}
	for index := range results {
		wr := &results[index]
		if wr.result.Error != nil || wr.artifact == nil {
			continue
		}
		page = append(page, wr)
		if len(page) == MirrorPageSize {
			flush()
		}
	}
	flush()
	return failed
}

// removeRelocatedSession clears a session's previous location after its pair
// has been installed at a new one, which happens when the session's project
// identity changes. The child subagents subtree is moved to the new location
// so a filtered child is not lost, then the old session directory is removed.
// oldDir and newDir are the session directories; both are absolute.
func (p *Pipeline) removeRelocatedSession(oldDir, newDir, sessionID string) error {
	oldSubagents := filepath.Join(oldDir, defaults.DirSubagents.String())
	entries, err := p.fs.ReadDir(oldSubagents)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(entries) > 0 {
		newSubagents := filepath.Join(newDir, defaults.DirSubagents.String())
		if err := p.fs.MkdirAll(newSubagents, defaults.PrivateDirPerm); err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			target := filepath.Join(newSubagents, entry.Name())
			if _, err := p.fs.Stat(target); err == nil {
				// The child was already re-ingested at the new location; its old
				// copy is superseded and removed with the old directory below.
				continue
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := p.fs.Rename(filepath.Join(oldSubagents, entry.Name()), target); err != nil {
				return err
			}
		}
	}
	return p.fs.RemoveAll(oldDir)
}

// reconcileScannedPairs records the database rows for saved pairs the database
// does not yet identify, so harvest index rebuilds a lost or incomplete
// database from the files. It reads a pair only for a session whose row is
// absent or carries no artifact hash; a present row whose recorded hash
// disagrees with a valid pair is a crash-row-3 state and is left untouched,
// reported by the ordinary readers, never overwritten from its backup. Parents
// are recorded before children, and the mirror refuses a child whose parent is
// not stored, so the next scan settles it.
func (p *Pipeline) reconcileScannedPairs(ctx context.Context, scanned []reindexTarget) {
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	mirror, mirrorErr := p.mirrorArtifactStore()
	if !ok || mirrorErr != nil || mirror == nil || p.config.DryRun {
		return
	}
	output := string(p.config.OutputDir)
	var page []ArtifactMirrorRequest
	flush := func() {
		if len(page) == 0 {
			return
		}
		for _, result := range mirror.MirrorArtifacts(ctx, page) {
			if result.Err != nil {
				p.reportMetadataRefusal(string(result.SessionID), fmt.Errorf("record saved session %s from its files: %w", result.SessionID, result.Err))
			}
		}
		page = page[:0]
	}
	for _, target := range scanned {
		if err := ctx.Err(); err != nil {
			return
		}
		if !p.includesManagedSession(target.session.SessionID, target.session.Harness) {
			continue
		}
		state, err := reader.ReadIndexState(ctx, target.session.SessionID)
		if err != nil {
			continue
		}
		if state != nil && state.ArtifactHash != nil {
			continue // Already identified; a disagreeing hash is not overwritten here.
		}
		metadataPath := filepath.Join(filepath.Dir(target.transcriptPath), string(target.session.SessionID)+defaults.MetadataSuffix)
		artifact, err := readArtifactPair(p.fs, output, metadataPath, target.session.SessionID)
		if err != nil {
			continue // A torn or unreadable pair is reported by the ordinary readers.
		}
		page = append(page, ArtifactMirrorRequest{Artifact: artifact})
		if len(page) == MirrorPageSize {
			flush()
		}
	}
	flush()
}

// bootstrapUnrecordedPair records a selected session's saved pair in the
// database when the row does not yet identify it (a null artifact hash on a
// legacy or logs-only row). It reads only that session's pair, never the tree,
// and does nothing when the row already records an artifact hash or a valid
// pair is not present. A present hash that disagrees is left untouched.
func (p *Pipeline) bootstrapUnrecordedPair(ctx context.Context, session DiscoveredSession, transcriptPath string) {
	if transcriptPath == "" || p.config.DryRun {
		return
	}
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	mirror, mirrorErr := p.mirrorArtifactStore()
	if !ok || mirrorErr != nil || mirror == nil {
		return
	}
	state, err := reader.ReadIndexState(ctx, session.SessionID)
	if err != nil || (state != nil && state.ArtifactHash != nil) {
		return
	}
	metadataPath := filepath.Join(filepath.Dir(transcriptPath), string(session.SessionID)+defaults.MetadataSuffix)
	artifact, err := readArtifactPair(p.fs, string(p.config.OutputDir), metadataPath, session.SessionID)
	if err != nil {
		return
	}
	for _, result := range mirror.MirrorArtifacts(ctx, []ArtifactMirrorRequest{{Artifact: artifact}}) {
		if result.Err != nil {
			p.reportMetadataRefusal(string(result.SessionID), fmt.Errorf("record saved session %s from its files: %w", result.SessionID, result.Err))
		}
	}
}

// mirrorRequestFor builds the database mirror request for one installed pair.
func mirrorRequestFor(wr *workerResult) ArtifactMirrorRequest {
	return ArtifactMirrorRequest{
		Artifact:              wr.artifact,
		EventSeq:              wr.acquiredEventSeq,
		Origin:                wr.origin,
		CWDProvenance:         wr.cwdProvenance,
		SourceFingerprint:     wr.sourceFingerprint,
		CommitCaptureComplete: wr.commitCaptureComplete,
	}
}
