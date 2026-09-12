package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// An omitted producer stamp predates tracking. Baseline 1 is only an eligibility
// default; it is never persisted as historical evidence of an adapter execution.
func (p *Pipeline) adapterNeedsRefresh(metadata *UnifiedMetadata) bool {
	if metadata == nil || metadata.SchemaVersion > CurrentSchemaVersion {
		return false
	}
	target := p.versionTargets()[metadata.ModelHarness].AdapterVersion
	version := 1
	if metadata.AdapterVersion != nil {
		version = *metadata.AdapterVersion
	}
	return version <= target && (version < target || metadataNeedsNativeRefresh(metadata.SchemaVersion))
}

func adapterTargetMetadataPath(target reindexTarget) string {
	return filepath.Join(filepath.Dir(target.transcriptPath), string(target.session.SessionID)+defaults.MetadataSuffix)
}

func (p *Pipeline) adapterTargetMetadata(ctx context.Context, target reindexTarget) *UnifiedMetadata {
	if !p.includesManagedSession(target.session.SessionID, target.session.Harness) || p.config.Since != nil && time.UnixMilli(target.startMs).Before(*p.config.Since) {
		return nil
	}
	if err := p.checkStoredRewriteVersion(ctx, target.session.SessionID, target.session.Harness); err != nil {
		p.reportMetadataRefusal(string(target.session.SessionID), err)
		return nil
	}
	path := adapterTargetMetadataPath(target)
	data, err := p.fs.ReadFile(path)
	if err != nil {
		return nil
	}
	metadata, err := decodeManagedMetadata(data, path)
	if err != nil {
		p.reportMetadataRefusal(string(target.session.SessionID), err)
		return nil
	}
	return metadata
}

func (p *Pipeline) adapterTargetNeedsWork(ctx context.Context, target reindexTarget) bool {
	return p.adapterNeedsRefresh(p.adapterTargetMetadata(ctx, target))
}

func (p *Pipeline) nativeSessionForTarget(target reindexTarget, metadata *UnifiedMetadata) DiscoveredSession {
	session := target.session
	// The managed transcript locator is only an index input. Native extraction
	// receives the original recorded source locator, never that managed path.
	session.SourcePath = ResolvedPath(target.originalSourcePath)
	if metadata != nil {
		session.CWD = metadata.CWD
		session.CreatedAt = time.UnixMilli(metadata.Timestamp.Start)
		if metadata.Git.Branch != nil {
			session.Branch = *metadata.Git.Branch
		}
	}
	if session.SourcePath != "" {
		if info, err := p.fs.Stat(session.SourcePath.String()); err == nil {
			session.ModTime = info.ModTime()
		}
	}
	return session
}

// Saved discovery selection does not suppress maintenance of retained sessions.
// Explicit harness/session/age filters still apply through adapterTargetMetadata.
//
// The inventory is database-driven: ListStaleAdapterSessions returns exactly
// the sessions whose stored adapter revision is behind this build, or whose
// stored schema this build re-extracts, reading no file. Each hit reconstructs
// its session from its stored location and its own retained pair.
func (p *Pipeline) appendStoredAdapterWork(ctx context.Context, entries []DiffEntry, discovered []DiscoveredSession) []DiffEntry {
	lister, ok := p.metricsStore.(StaleAdapterSessionLister)
	if !ok {
		return entries
	}
	staleIDs, err := lister.ListStaleAdapterSessions(ctx, p.indexerTargets(), p.nativeRefreshSchemaVersions())
	if err != nil {
		p.reportMetadataRefusal("adapter refresh selection", err)
		return entries
	}
	queued := make(map[SessionID]bool, len(entries))
	for _, entry := range entries {
		queued[entry.Session.SessionID] = true
	}
	native := make(map[SessionID]DiscoveredSession, len(discovered))
	for _, session := range discovered {
		native[session.SessionID] = session
	}
	for _, sid := range staleIDs {
		if queued[sid] {
			continue
		}
		reconstructed, startMs, transcriptPath, metadataErr := p.reconstructFromMetadata(ctx, sid)
		if metadataErr != nil {
			p.reportMetadataRefusal(string(sid), metadataErr)
			continue
		}
		if reconstructed == nil {
			continue
		}
		target := reindexTarget{session: *reconstructed, startMs: startMs, transcriptPath: transcriptPath}
		metadata := p.adapterTargetMetadata(ctx, target)
		if !p.adapterNeedsRefresh(metadata) {
			continue
		}
		// The stored metadata carries the original native source locator. A
		// retained-input reconstruction has no native session, so the target
		// must carry it forward: without it the reconstructed session's source
		// path is empty and reads as native input that moved, which forces a
		// native re-extraction the retained pair could have served.
		if metadata != nil {
			target.originalSourcePath = metadata.Source.FilePath
		}
		session, found := native[sid]
		if !found {
			session = p.nativeSessionForTarget(target, metadata)
		}
		if !p.config.IncludeActive && p.config.StalenessThreshold > 0 && !session.stalenessSourceTime().IsZero() && time.Since(session.stalenessSourceTime()) < p.config.StalenessThreshold {
			continue
		}
		entries = append(entries, DiffEntry{Session: session, Status: DiffUpdated, retainedOnly: !found})
		queued[session.SessionID] = true
	}
	return entries
}

// pairRepairCandidateIDs returns the stored sessions that may need their saved
// pair re-ingested: the stale index revisions and the crash-repair predicate
// the ordinary index inventory already selects. It reads no file.
func (p *Pipeline) pairRepairCandidateIDs(ctx context.Context) ([]SessionID, error) {
	if p.metricsStore == nil {
		return nil, nil
	}
	// The inventory needs both the stale list and the captured index state the
	// identity comparison reads. A store that cannot answer contributes no
	// repair work, as every other capability-based selection treats it.
	if _, ok := p.metricsStore.(SessionIndexStateReader); !ok {
		return nil, nil
	}
	stale, err := p.metricsStore.ListStaleIndexSessions(ctx, p.indexerTargets())
	if err != nil {
		return nil, err
	}
	var repair []SessionID
	if lister, ok := p.metricsStore.(RepairSessionLister); ok {
		if repair, err = lister.ListSessionsNeedingRepair(ctx, p.indexerTargets()); err != nil {
			return nil, err
		}
	}
	seen := make(map[SessionID]bool, len(stale)+len(repair))
	ids := make([]SessionID, 0, len(stale)+len(repair))
	for _, sid := range append(stale, repair...) {
		if seen[sid] {
			continue
		}
		seen[sid] = true
		ids = append(ids, sid)
	}
	return ids, nil
}

// storedMetadataPath returns the metadata locator the database records for a
// session, and whether it has one.
func (p *Pipeline) storedMetadataPath(ctx context.Context, sid SessionID) (string, bool) {
	hostSlug, parentID, err := p.metricsStore.LookupSessionLocation(ctx, sid)
	if err != nil || hostSlug == "" {
		return "", false
	}
	return SessionMetadataPath(string(p.config.OutputDir), hostSlug, string(sid), parentID), true
}

// pairNeedsRepair reports whether a stored session's saved pair is missing or
// damaged (content that no longer matches the identity the row records). It
// reads only the session's recorded location: no tree is walked and no session
// this run did not select is touched. An unreadable file is a fault to restore,
// not a pair to overwrite, and a stored metadata or schema this build refuses
// is not damage; both stay with the existing refusal paths.
func (p *Pipeline) pairNeedsRepair(ctx context.Context, sid SessionID) bool {
	if p.metricsStore == nil {
		return false
	}
	if err := p.checkStoredMetadataVersion(ctx, sid); err != nil {
		return false
	}
	metadataPath, ok := p.storedMetadataPath(ctx, sid)
	if !ok {
		return false // No recorded location: the source-info fallback owns it.
	}
	data, err := p.fs.ReadFile(metadataPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true
		}
		return false
	}
	meta, decodeErr := decodeManagedMetadata(data, p.managedRelativePath(metadataPath))
	if decodeErr != nil {
		if isMetadataCompatibilityError(decodeErr) {
			return false
		}
		return true
	}
	if meta.Source.Format == "" {
		return true
	}
	transcriptPath := filepath.Join(filepath.Dir(metadataPath), string(sid)+"--transcript."+string(meta.Source.Format))
	transcript, readErr := p.fs.ReadFile(transcriptPath)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return true
		}
		return false
	}
	// A present pair is compared against the recorded identity: content that no
	// longer matches is repaired from native like a missing half. A row with no
	// recorded identity is left to the ordinary index write, which establishes
	// the identity from the pair it parsed.
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return false
	}
	state, stateErr := reader.ReadIndexState(ctx, sid)
	if stateErr != nil || state == nil || state.ArtifactHash == nil {
		return false
	}
	artifact, artifactErr := NewManagedArtifact(data, transcript)
	if artifactErr != nil {
		if isMetadataCompatibilityError(artifactErr) {
			return false
		}
		return true
	}
	return artifact.ArtifactHash != *state.ArtifactHash
}

// pairSourceAvailable reports that the session's recorded native source is
// still readable, so a native re-ingest can run.
func (p *Pipeline) pairSourceAvailable(session DiscoveredSession) bool {
	if session.SourcePath == "" {
		return false
	}
	if _, err := p.fs.Stat(session.SourcePath.String()); err != nil {
		return false
	}
	return true
}

// reportPairRepairUnavailable names a session whose saved pair is missing or
// damaged and whose native source could not be reached to repair it. The
// stored copy keeps serving meanwhile; the repair runs automatically once the
// source is available again.
func (p *Pipeline) reportPairRepairUnavailable(sid SessionID, source string) {
	p.reportDiagnostic(DiagnosticEntry{
		ErrorType:   "pair_repair_unavailable",
		Location:    fmt.Sprintf("session %s pair repair", sid),
		Message:     missingPairText(sid, source),
		Remediation: "Restore the original harness source and rerun harvest; the stored copy keeps serving meanwhile.",
	})
}

// pairRepairInScope applies the run's explicit harness, session and age filters
// to a repair candidate. A session whose start is unknown is excluded when
// --since is set, matching how the index selection treats a reconstructed
// target without a recorded start.
func (p *Pipeline) pairRepairInScope(session DiscoveredSession, startMs int64) bool {
	if !p.includesManagedSession(session.SessionID, session.Harness) {
		return false
	}
	if p.config.Since == nil {
		return true
	}
	if startMs <= 0 && !session.CreatedAt.IsZero() {
		startMs = session.CreatedAt.UnixMilli()
	}
	if startMs <= 0 && !session.ModTime.IsZero() {
		startMs = session.ModTime.UnixMilli()
	}
	return startMs > 0 && !time.UnixMilli(startMs).Before(*p.config.Since)
}

// cacheRepairLocations makes the repair candidates answerable from the bulk
// location cache, so the native write path resolves their owned locator with
// one stat instead of falling through to a whole-tree lookup.
func (p *Pipeline) cacheRepairLocations(ctx context.Context, ids []SessionID) {
	if p.store == nil || len(ids) == 0 {
		return
	}
	locations, err := p.store.BulkLookupSessionLocations(ctx, ids)
	if err != nil {
		return
	}
	if p.locationCache == nil {
		p.locationCache = locations
		return
	}
	for sid, location := range locations {
		p.locationCache[sid] = location
	}
}

// appendPairRepairWork appends the stored sessions whose saved pair is missing
// or damaged as native re-ingest work. The repair used to be an explicit
// `--force --session` action; a session with no usable retained input has
// nothing to protect, so the ordinary harvest performs it automatically. A
// session already queued for other work is marked for repair in place, so a
// database-first "unchanged" verdict cannot leave the damaged pair behind.
func (p *Pipeline) appendPairRepairWork(ctx context.Context, entries []DiffEntry, discovered []DiscoveredSession) []DiffEntry {
	if p.metricsStore == nil {
		return entries
	}
	ids, err := p.pairRepairCandidateIDs(ctx)
	if err != nil {
		p.reportMetadataRefusal("pair repair selection", err)
		return entries
	}
	entryIndex := make(map[SessionID]int, len(entries))
	for i := range entries {
		entryIndex[entries[i].Session.SessionID] = i
	}
	native := make(map[SessionID]DiscoveredSession, len(discovered))
	for _, session := range discovered {
		native[session.SessionID] = session
	}
	var repaired []SessionID
	for _, sid := range ids {
		if !p.pairNeedsRepair(ctx, sid) {
			continue
		}
		metadataPath, _ := p.storedMetadataPath(ctx, sid)
		if i, ok := entryIndex[sid]; ok {
			// Already queued for other work: carry the repair verdict onto the
			// existing entry instead of skipping it.
			entries[i].pairRepair = true
			entries[i].repairMetadataPath = metadataPath
			repaired = append(repaired, sid)
			continue
		}
		session, found := native[sid]
		if !found {
			reconstructed, startMs, _ := p.reconstructFromSourceInfo(ctx, sid)
			if reconstructed == nil {
				p.reportPairRepairUnavailable(sid, "")
				continue
			}
			session = *reconstructed
			if !p.pairRepairInScope(session, startMs) {
				continue
			}
		} else if !p.pairRepairInScope(session, 0) {
			continue
		}
		if !p.pairSourceAvailable(session) {
			p.reportPairRepairUnavailable(sid, session.SourcePath.String())
			continue
		}
		entries = append(entries, DiffEntry{
			Session: session, Status: DiffUpdated, pairRepair: true, repairMetadataPath: metadataPath,
		})
		repaired = append(repaired, sid)
	}
	p.cacheRepairLocations(ctx, repaired)
	return entries
}

// pairRepairTargets returns reindex targets for stored sessions whose saved
// pair is missing or damaged. The tree scan cannot find them (the metadata
// locator is what is gone), so they are enumerated from the database and
// routed to native re-ingestion like any other extractable target.
func (p *Pipeline) pairRepairTargets(ctx context.Context, scanned []reindexTarget) []reindexTarget {
	if p.metricsStore == nil {
		return nil
	}
	ids, err := p.pairRepairCandidateIDs(ctx)
	if err != nil {
		p.reportMetadataRefusal("pair repair selection", err)
		return nil
	}
	seen := make(map[SessionID]bool, len(scanned))
	for _, target := range scanned {
		seen[target.session.SessionID] = true
	}
	var targets []reindexTarget
	for _, sid := range ids {
		if seen[sid] || !p.pairNeedsRepair(ctx, sid) {
			continue
		}
		session, startMs, transcriptPath := p.reconstructFromSourceInfo(ctx, sid)
		if session == nil {
			p.reportPairRepairUnavailable(sid, "")
			continue
		}
		if !p.pairRepairInScope(*session, startMs) {
			continue
		}
		targets = append(targets, reindexTarget{
			session: *session, startMs: startMs, transcriptPath: transcriptPath,
			originalSourcePath: session.SourcePath.String(), pairRepair: true,
		})
	}
	return targets
}

func (p *Pipeline) nativeInputChanged(session DiscoveredSession, metadata *UnifiedMetadata) bool {
	if session.SourcePath.String() != metadata.Source.FilePath || !reflect.DeepEqual(session.ParentUUID, metadata.ParentUUID) {
		return true
	}
	if !session.ModTime.IsZero() && (metadata.Timestamp.Ingested == nil || session.ModTime.After(time.UnixMilli(*metadata.Timestamp.Ingested))) {
		return true
	}
	stored, known := p.seqCursorCache[session.SessionID]
	return known && session.EventSeq > stored
}

type adapterAcquisitionError struct{ cause error }

func (e *adapterAcquisitionError) Error() string { return e.cause.Error() }
func (e *adapterAcquisitionError) Unwrap() error { return e.cause }

// processSession keeps adapter preparation separate from publication failure.
// Only failed native acquisition permits the last-good retained index fallback.
func (p *Pipeline) processSession(ctx context.Context, entry DiffEntry) workerResult {
	var metadata *UnifiedMetadata
	var metadataErr error
	var metadataPath string
	var pathErr error
	if entry.pairRepair {
		// The repair already resolved the session's owned locator, and a pair
		// that cannot be read has no retained-first path to prefer. Both
		// lookups are skipped so a missing sidecar cannot fall through to a
		// whole-tree lookup.
		metadataPath = entry.repairMetadataPath
	} else {
		metadata, metadataErr = p.metadataForRewrite(ctx, entry.Session)
		metadataPath, pathErr = p.findMetadataPath(ctx, entry.Session)
	}
	// A forced run is an explicit manual refresh and tries native input first;
	// routine version-driven maintenance prefers sufficient retained input.
	if metadataErr == nil && pathErr == nil && metadata != nil && p.adapterNeedsRefresh(metadata) && !metadataNeedsNativeRefresh(metadata.SchemaVersion) && !p.config.Force && !entry.pairRepair && !p.nativeInputChanged(entry.Session, metadata) {
		refreshed := p.processRetainedSession(ctx, entry.Session, metadataPath)
		var insufficient *InsufficientRetainedInputError
		if refreshed.result.Error == nil || !errors.As(refreshed.result.Error, &insufficient) {
			return refreshed
		}
	}
	var result workerResult
	if entry.retainedOnly && metadata != nil && (metadata.Redaction.Applied || len(metadata.Subagents) > 0) {
		result.result = SessionResult{SessionID: entry.Session.SessionID, Harness: entry.Session.Harness, ParentUUID: entry.Session.ParentUUID, Status: entry.Status,
			Error: &adapterAcquisitionError{cause: fmt.Errorf("refresh retained session %s from native input: required original discovery context is unavailable or redacted; no native attribution or child paths were guessed; restore native discovery and retry harvest", entry.Session.SessionID)}}
	} else {
		result = p.processNativeSession(ctx, entry)
	}
	var acquisition *adapterAcquisitionError
	if !errors.As(result.result.Error, &acquisition) || metadataPath == "" || pathErr != nil {
		return result
	}
	// The manual index refresh (harvest index --force) names its own
	// unavailable native input; every other path, including a forced ordinary
	// harvest, reports the adapter refresh it could not complete.
	errorType, location := "adapter_refresh_unavailable", fmt.Sprintf("%s session %s adapter refresh", entry.Session.Harness, entry.Session.SessionID)
	if p.config.Reindex && p.config.Force {
		errorType, location = "native_refresh_unavailable", fmt.Sprintf("%s session %s forced refresh", entry.Session.Harness, entry.Session.SessionID)
	}
	p.reportDiagnostic(DiagnosticEntry{
		ErrorType: errorType, Location: location,
		Message:     acquisition.Error() + "; the previous artifact and adapter stamp were preserved; supported retained indexing may continue",
		Remediation: "Restore access to the original harness source and discovery context, then retry harvest.",
	})
	result.result.Error = nil
	result.result.Status = DiffUnchanged
	retained, err := readArtifactPair(p.fs, string(p.config.OutputDir), metadataPath, entry.Session.SessionID)
	if err != nil {
		p.reportMetadataRefusal(p.managedRelativePath(metadataPath), managedInputIOError(p.managedRelativePath(metadataPath), err))
		return result
	}
	// No new file commit or DB success is claimed. The normal index capture
	// guards independently verify that this retained pair matches its stored state.
	result.meta = &retained.Metadata
	result.transcriptData = retained.Transcript
	result.result.OutputPath = filepath.Dir(metadataPath)
	result.outputTranscriptPath = filepath.Join(result.result.OutputPath, string(entry.Session.SessionID)+"--transcript."+string(retained.Metadata.Source.Format))
	result.originalRoot = entry.Session.OriginalRoot
	result.transcriptOrigin = entry.Session.TranscriptOrigin
	result.startMs = retained.Metadata.Timestamp.Start
	return result
}

// hasUsableRetainedSession decides, only when native discovery FAILED, whether
// the harvest can still maintain a session it saved earlier. It is the one
// place the retained tree is read on the ordinary path, and only on a
// discovery failure: it walks the saved sessions and returns true as soon as
// one pair reads and validates. A missing or corrupt pair is skipped, and the
// stored recovery candidates are then given their guarded read before native
// discovery failure is declared fatal.
func (p *Pipeline) hasUsableRetainedSession(ctx context.Context) bool {
	output := string(p.config.OutputDir)
	usable := false
	_ = walkManagedMetadata(ctx, p.fs, output, func(sid SessionID, path string) error {
		if usable {
			return nil
		}
		// Harness and start are read from the pair, so scope is applied after
		// the pair reads and an unreadable pair is simply skipped.
		artifact, err := readArtifactPair(p.fs, output, path, sid)
		if err != nil {
			return nil
		}
		if !p.includesManagedSession(sid, artifact.Metadata.ModelHarness) {
			return nil
		}
		if p.config.Since != nil && time.UnixMilli(artifact.Metadata.Timestamp.Start).Before(*p.config.Since) {
			return nil
		}
		usable = true
		return nil
	})
	if usable {
		return true
	}
	if p.config.DryRun || p.metricsStore == nil {
		return false
	}
	// Missing/corrupt metadata is absent from the file inventory. Give the
	// same stored candidates used by normal maintenance their guarded read
	// before declaring native discovery failure fatal.
	staleIDs, err := p.metricsStore.ListStaleIndexSessions(ctx, p.indexerTargets())
	if err != nil {
		p.reportMetadataRefusal("retained discovery recovery", fmt.Errorf("read stored recovery candidates after native discovery failed: %w; no recovery was attempted; restore database access and retry harvest", err))
		return false
	}
	for _, sid := range staleIDs {
		session, _, transcriptPath := p.reconstructFromSourceInfo(ctx, sid)
		if session == nil {
			continue
		}
		// A lost metadata file beside an intact transcript is still a usable
		// retained session: the index-recovery pass reconstructs it from stored
		// source information and rebuilds the pair. The recoverable evidence is
		// the transcript, not the metadata the recovery will rewrite, so confirm
		// the transcript reads before declaring native discovery failure fatal.
		if _, err := p.fs.Stat(transcriptPath); err == nil {
			return true
		}
	}
	return false
}
