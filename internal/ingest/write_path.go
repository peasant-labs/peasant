package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
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
	// transcriptReadFromInstalled reports that transcript IS the installed
	// file's content: readArtifactPair read it from the session directory in
	// this run and verified it against the checksum its metadata records,
	// refusing a torn or mixed pair. The install then keeps the file without
	// reading it a second time. Only the stored-copy lane sets it; bytes that
	// come from anywhere else must be compared with the installed file.
	transcriptReadFromInstalled bool
}

// pairInstallResult reports which halves of a pair an install replaced. A half
// that was not replaced is the installed file itself: same inode, same mtime.
type pairInstallResult struct {
	TranscriptReplaced bool
	MetadataReplaced   bool
}

// keptFiles names the files an install owns but did not stage, because the
// installed bytes already equal the candidate's. The keys are paths relative
// to the session directory, in the form the prune walk produces, and include
// the debug directory when a file below it is kept. transcript reports that
// the transcript is among them: the index input proof names the transcript
// bytes, so it is cleared only when the transcript is replaced.
type keptFiles struct {
	names      map[string]bool
	transcript bool
}

func (k *keptFiles) keep(rel string) {
	if k.names == nil {
		k.names = make(map[string]bool)
	}
	k.names[rel] = true
}

// installedFileEquals reports whether the installed file at path holds exactly
// data. It reads the installed bytes and never trusts a recorded hash, so a
// torn pair compares unequal and is replaced. A file that is absent or cannot
// be read is reported unequal: the install then stages and renames it, and a
// real filesystem fault surfaces there.
func (p *Pipeline) installedFileEquals(path string, data []byte) bool {
	if info, err := p.fs.Stat(path); err != nil || info.IsDir() || info.Size() != int64(len(data)) {
		return false
	}
	installed, err := p.fs.ReadFile(path)
	return err == nil && bytes.Equal(installed, data)
}

// installManagedPair installs the session's files into sessionDir, metadata
// last. A file whose installed bytes already equal the candidate's is kept in
// place and never renamed over; the remaining files are written into a
// temporary directory on the same filesystem and installed by rename. When the
// transcript is replaced the metadata is always replaced with it, so the
// metadata rename stays the commit point of the pair. An install that changes
// no file creates no temporary directory and sends nothing to the writer
// lane. It replaces only files this session's naming owns, prunes owned files
// this install no longer writes, and never touches the child subagents
// subtree. There is no sync and no lock: the database transaction is the
// durability point, and a torn install is detected on the next pair read by
// its hash. It refuses to replace a pair a newer adapter than this build
// produced.
func (p *Pipeline) installManagedPair(ctx context.Context, sessionDir, sessionID string, pair installedPair, session DiscoveredSession, writeLane *storeWriteLane) (pairInstallResult, error) {
	var result pairInstallResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	existing, err := p.fs.ReadFile(filepath.Join(sessionDir, pair.metadataName))
	if err == nil {
		if headerErr := checkReplacementHeader(existing, session, p.versionTargets()); headerErr != nil {
			return result, headerErr
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return result, managedInputIOError(filepath.Join(sessionDir, pair.metadataName), err)
	}
	type stagedFile struct {
		rel  string
		data []byte
		what string
	}
	var staged []stagedFile
	var kept keptFiles
	// Bytes a verified pair read just took from the installed file are that
	// file: a second read could learn nothing the first did not, and a torn
	// pair never reaches here because that read refuses one by its checksum.
	// Every other candidate is compared with the installed bytes.
	kept.transcript = pair.transcriptReadFromInstalled ||
		p.installedFileEquals(filepath.Join(sessionDir, pair.transcriptName), pair.transcript)
	if kept.transcript {
		kept.keep(pair.transcriptName)
	} else {
		staged = append(staged, stagedFile{pair.transcriptName, pair.transcript, "transcript"})
	}
	debugDir := defaults.DirDebug.String()
	debugNames := make([]string, 0, len(pair.debug))
	for name := range pair.debug {
		debugNames = append(debugNames, name)
	}
	sort.Strings(debugNames)
	for _, name := range debugNames {
		rel := filepath.Join(debugDir, name)
		if p.installedFileEquals(filepath.Join(sessionDir, rel), pair.debug[name]) {
			kept.keep(debugDir)
			kept.keep(rel)
			continue
		}
		staged = append(staged, stagedFile{rel, pair.debug[name], "debug output " + name})
	}
	if len(pair.sourceCapture) > 0 {
		rel := fileCaptureEvidenceName(session.SessionID)
		if p.installedFileEquals(filepath.Join(sessionDir, rel), pair.sourceCapture) {
			kept.keep(rel)
		} else {
			staged = append(staged, stagedFile{rel, pair.sourceCapture, "source-capture marker"})
		}
	}
	// The metadata is kept only beside a kept transcript. Beside a replaced
	// transcript it is always renamed last, even when its bytes are equal, so
	// a reader never needs more than the metadata rename to see a whole pair.
	if kept.transcript && err == nil && bytes.Equal(existing, pair.metadata) {
		kept.keep(pair.metadataName)
	} else {
		staged = append(staged, stagedFile{pair.metadataName, pair.metadata, "metadata"})
		result.MetadataReplaced = true
	}
	result.TranscriptReplaced = !kept.transcript
	if len(staged) == 0 {
		// Nothing differs: no temporary directory and no writer-lane job. An
		// owned file this install no longer writes is still pruned.
		return result, p.pruneObsoleteOwned(sessionDir, sessionID, kept.names)
	}
	tmpSuffix, err := randomHex(defaults.TempSuffixLen)
	if err != nil {
		return result, fmt.Errorf("generate temp suffix for %s: %w", sessionID, err)
	}
	// NOTE (M14): the temporary directory is {outputDir}/.tmp-{sessionId}-{random};
	// cleanOrphans() scans {outputDir} for .tmp-* prefixes, so both must agree.
	tmpDir := fmt.Sprintf("%s/%s%s-%s", string(p.config.OutputDir), defaults.TempDirPrefix, sessionID, tmpSuffix)
	if err := p.fs.MkdirAll(tmpDir, defaults.PrivateDirPerm); err != nil {
		return result, fmt.Errorf("create temp dir for %s: %w", sessionID, err)
	}
	fail := func(err error) (pairInstallResult, error) {
		return pairInstallResult{}, errors.Join(err, p.fs.RemoveAll(tmpDir))
	}
	madeDebugDir := false
	for _, file := range staged {
		if filepath.Dir(file.rel) == debugDir && !madeDebugDir {
			if err := p.fs.MkdirAll(filepath.Join(tmpDir, debugDir), defaults.PrivateDirPerm); err != nil {
				return fail(fmt.Errorf("create debug dir for %s: %w", sessionID, err))
			}
			madeDebugDir = true
		}
		if err := p.fs.WriteFile(filepath.Join(tmpDir, file.rel), file.data, defaults.PrivateFilePerm); err != nil {
			return fail(fmt.Errorf("write %s for %s: %w", file.what, sessionID, err))
		}
	}
	if err := p.replaceSessionDir(ctx, tmpDir, sessionDir, sessionID, pair.metadataName, kept, writeLane); err != nil {
		return fail(err)
	}
	return result, p.fs.RemoveAll(tmpDir)
}

// replaceSessionDir installs every file staged under src into dst by rename,
// installing metadataName LAST so the metadata is the commit point. Files in
// kept were not staged because the installed bytes already equal the
// candidate's; they stay in place and are not pruned. The index input proof is
// cleared before any rename unless the transcript is among the kept files. It
// then prunes obsolete parent-owned files, preserving the child-owned
// subagents subtree. Rename is atomic on the same filesystem; there is no
// sync, no lock and no rollback, because the database transaction is the
// durability point and a mixed pair is refused on the next read by its hash.
func (p *Pipeline) replaceSessionDir(ctx context.Context, src, dst, sessionID, metadataName string, kept keptFiles, writeLane *storeWriteLane) error {
	var ordered, metadata []string
	wanted := make(map[string]bool, len(kept.names))
	for rel := range kept.names {
		wanted[rel] = true
	}
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
	// The proof names the transcript bytes. An install that keeps the
	// installed transcript leaves them untouched, so the proof stays true and
	// no writer-lane job is sent; the metadata is then replaced by one atomic
	// rename and the pair is whole before and after it.
	if !kept.transcript {
		mirror, err := p.mirrorArtifactStore()
		if err == nil && mirror != nil {
			var sid SessionID
			sid, err = NewSessionID(sessionID)
			if err == nil {
				p.runStoreWrite(writeLane, func() { err = mirror.PrepareArtifactInstall(ctx, sid) })
			}
		}
		if err != nil {
			return fmt.Errorf("prepare artifact install for session %s at %s before replacing files: %w; existing installed pair and database entries were preserved; restore database access and retry harvest", sessionID, dst, err)
		}
	}
	for _, rel := range append(ordered, metadata...) {
		if err := p.fs.Rename(filepath.Join(src, rel), filepath.Join(dst, rel)); err != nil {
			return fmt.Errorf("install parent files at %s: %w; existing child output remains in place; fix filesystem access or free disk space and rerun ingest", dst, err)
		}
	}
	return p.pruneObsoleteOwned(dst, sessionID, wanted)
}

// pruneObsoleteOwned removes the parent-owned files under dst that the current
// install neither staged nor kept. It skips the entire child-owned tree,
// including children excluded by FILTER.
func (p *Pipeline) pruneObsoleteOwned(dst, sessionID string, wanted map[string]bool) error {
// Prune obsolete parent-owned files only after every new file is installed.
	var stale []string
	err := p.fs.WalkDir(dst, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		// filepath.Rel yields native separators from the real filesystem
		// (backslash on Windows) but forward slashes from the MemFS test double
		// this package's tests use, so the "under debug/" containment test is
		// made on a normalized copy. rel itself stays native, because `wanted`
		// is keyed by the native rel produced by the staging walk above and
		// `stale` carries real filesystem paths for RemoveAll. Without this, a
		// real Windows walk yields "debug\<name>", the prefix never matches,
		// every file below debug/ takes the not-parent-owned branch, and
		// obsolete debug artifacts are never pruned.
		relSlash := filepath.ToSlash(rel)
		if rel == defaults.DirSubagents.String() {
			return fs.SkipDir
		}
		if rel != "." && !strings.HasPrefix(rel, sessionID+"--") && rel != defaults.DirDebug.String() && !strings.HasPrefix(relSlash, defaults.DirDebug.String()+"/") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if rel != "." && !wanted[rel] {
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
			p.reportSessionDiagnostic(wr.result.SessionID, DiagnosticEntry{ErrorType: "artifact_mirror_pending", Location: string(wr.result.SessionID), Message: failure.Error(), Remediation: "Restore database access and rerun harvest to record the saved session."})
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
// identity changes. Only the parent-owned files are removed from the old
// directory; the child subagents subtree is left in place, so a child is never
// lost by the parent's move and is relocated on its own next harvest. oldDir is
// the session directory the pair moved away from; it is absolute.
func (p *Pipeline) removeRelocatedSession(oldDir, sessionID string) error {
	entries, err := p.fs.ReadDir(oldDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		// The subagents subtree is child-owned and stays; every parent-owned
		// file and the debug directory are the ones the move leaves behind.
		if name == defaults.DirSubagents.String() {
			continue
		}
		if !strings.HasPrefix(name, sessionID+"--") && name != defaults.DirDebug.String() {
			continue
		}
		if err := p.fs.RemoveAll(filepath.Join(oldDir, name)); err != nil {
			return err
		}
	}
	return nil
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
				continue
			}
			p.rebuiltFromFiles++
		}
		page = page[:0]
	}
	for _, target := range scanned {
		if err := ctx.Err(); err != nil {
			return
		}
		if !p.includesIndexTarget(target) {
			continue
		}
		state, err := reader.ReadIndexState(ctx, target.session.SessionID)
		if err != nil {
			continue
		}
		// Skip a session this build would refuse: a stored schema or producer
		// newer than this build is left untouched and reported by the selection,
		// not read or mirror-refused here (which would duplicate the diagnostic).
		if p.checkStoredMetadataVersion(ctx, target.session.SessionID) != nil {
			continue
		}
		if state != nil && p.checkIndexProducer(state) != nil {
			continue
		}
		metadataPath := filepath.Join(filepath.Dir(target.transcriptPath), string(target.session.SessionID)+defaults.MetadataSuffix)
		if state != nil && state.ArtifactHash != nil {
			// The row already identifies a pair. A plain harvest index never
			// reads it again; only the full rebuild pass does, and only to
			// report a saved copy whose bytes no longer match the row. Such a
			// pair is NEVER mirrored: overwriting the database with the file's
			// backup after a power loss would fire the identity trigger. Only a
			// completed index still serves the session; the file is what needs
			// repair either way.
			if !p.config.RebuildAll {
				continue
			}
			artifact, err := readArtifactPair(p.fs, output, metadataPath, target.session.SessionID)
			if err != nil {
				p.reportDamagedPair(target.session.SessionID, state)
				p.rebuildStale = append(p.rebuildStale, target.session.SessionID)
				continue
			}
			if artifact.ArtifactHash != *state.ArtifactHash {
				p.reportDamagedPair(target.session.SessionID, state)
				p.rebuildStale = append(p.rebuildStale, target.session.SessionID)
			}
			continue
		}
		artifact, err := readArtifactPair(p.fs, output, metadataPath, target.session.SessionID)
		if err != nil {
			// A torn or unreadable pair with no recorded pair identity cannot be
			// mirrored: there is nothing to mirror it against. A row may exist
			// here without that identity, so this says nothing about whether an
			// index has completed. The rebuild names the pair as a skipped saved
			// copy; a plain index leaves it to the ordinary readers.
			if p.config.RebuildAll {
				p.reportDamagedPair(target.session.SessionID, state)
				p.rebuildStale = append(p.rebuildStale, target.session.SessionID)
			}
			continue
		}
		page = append(page, ArtifactMirrorRequest{Artifact: artifact})
		if len(page) == MirrorPageSize {
			flush()
		}
	}
	flush()
}

// mirrorRequestFor builds the database mirror request for one installed pair.
func mirrorRequestFor(wr *workerResult) ArtifactMirrorRequest {
	return ArtifactMirrorRequest{
		Artifact:                 wr.artifact,
		EventSeq:                 wr.acquiredEventSeq,
		Origin:                   wr.origin,
		CWDProvenance:            wr.cwdProvenance,
		SourceFingerprint:        wr.sourceFingerprint,
		CommitCaptureComplete:    wr.commitCaptureComplete,
		SchedulingParentResolved: wr.schedulingResolved,
		SchedulingParentID:       wr.schedulingParentID,
		TranscriptKept:           wr.transcriptKept,
	}
}
