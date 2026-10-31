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
	return p.adapterTargetMetadataIn(ctx, target, nil)
}

func (p *Pipeline) adapterTargetMetadataIn(ctx context.Context, target reindexTarget, snap locationSnapshot) *UnifiedMetadata {
	// The strict harness and session filters plus the age window: an adapter
	// refresh reads the stored pair for a session the run explicitly asked for.
	if !p.includesManagedSession(target.session.SessionID, target.session.Harness) || !p.withinRunAge(target.startMs) {
		return nil
	}
	adapterTarget := p.versionTargets()[target.session.Harness].AdapterVersion
	if err := p.checkStoredMetadataCompatibilityIn(ctx, target.session.SessionID, &adapterTarget, snap); err != nil {
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

// Saved discovery selection does not suppress maintenance of stored sessions.
// Explicit harness/session/age filters still apply through adapterTargetMetadata.
//
// The inventory is database-driven: ListStaleAdapterSessions returns exactly
// the sessions whose stored adapter revision is behind this build, or whose
// stored schema this build re-extracts, reading no file. Each hit reconstructs
// its session from its stored location and its own stored pair.
//
// The candidates' locations are read with one bulk query, and the
// per-candidate reconstruction runs on the bounded worker pool. Outcomes are
// appended in candidate order, so the result is the same as a serial pass.
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
	candidates := make([]SessionID, 0, len(staleIDs))
	for _, sid := range staleIDs {
		if queued[sid] {
			continue
		}
		queued[sid] = true
		candidates = append(candidates, sid)
	}
	snap := p.snapshotLocations(ctx, candidates)
	outcomes := runParallel(ctx.Err, candidates, parallelWorkers(p.config), func(sid SessionID) *DiffEntry {
		return p.storedAdapterEntry(ctx, sid, native, snap)
	})
	for _, entry := range outcomes {
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	return entries
}

// storedAdapterEntry decides one stale-adapter candidate. It reads shared
// pipeline state only, and its diagnostics go through the locked reporter, so
// it is safe on parallel workers. Nil means the candidate contributes no work.
func (p *Pipeline) storedAdapterEntry(ctx context.Context, sid SessionID, native map[SessionID]DiscoveredSession, snap locationSnapshot) *DiffEntry {
	reconstructed, startMs, transcriptPath, metadataErr := p.reconstructFromMetadataIn(ctx, sid, snap)
	if metadataErr != nil {
		p.reportMetadataRefusal(string(sid), metadataErr)
		return nil
	}
	if reconstructed == nil {
		return nil
	}
	target := reindexTarget{session: *reconstructed, startMs: startMs, transcriptPath: transcriptPath}
	metadata := p.adapterTargetMetadataIn(ctx, target, snap)
	if !p.adapterNeedsRefresh(metadata) {
		return nil
	}
	// The stored metadata carries the original native source locator. A
	// stored-input reconstruction has no native session, so the target
	// must carry it forward: without it the reconstructed session's source
	// path is empty and reads as native input that moved, which forces a
	// native re-extraction the stored pair could have served.
	if metadata != nil {
		target.originalSourcePath = metadata.Source.FilePath
	}
	session, found := native[sid]
	if !found {
		session = p.nativeSessionForTarget(target, metadata)
	}
	if !p.config.IncludeActive && p.config.StalenessThreshold > 0 && !session.stalenessSourceTime().IsZero() && time.Since(session.stalenessSourceTime()) < p.config.StalenessThreshold {
		return nil
	}
	return &DiffEntry{Session: session, Status: DiffUpdated, retainedOnly: !found}
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
func (p *Pipeline) storedMetadataPath(ctx context.Context, sid SessionID, snap locationSnapshot) (string, bool) {
	hostSlug, parentID, err := p.storedLocation(ctx, sid, snap)
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
	return p.pairNeedsRepairIn(ctx, sid, nil)
}

// pairNeedsRepairIn is pairNeedsRepair answering stored location reads from
// snap when it holds the session. It writes no shared pipeline state, so the
// repair selection calls it from parallel workers.
func (p *Pipeline) pairNeedsRepairIn(ctx context.Context, sid SessionID, snap locationSnapshot) bool {
	if p.metricsStore == nil {
		return false
	}
	if err := p.checkStoredMetadataCompatibilityIn(ctx, sid, nil, snap); err != nil {
		return false
	}
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return false
	}
	state, stateErr := reader.ReadIndexState(ctx, sid)
	if stateErr != nil || state == nil {
		return false
	}
	// A stored producer or index format newer than this build must never be
	// replaced from native input: the newer build's output is preserved and
	// the refusal stays with the selection. Checking it here also keeps the
	// pair unread for a session this run refuses.
	if err := p.checkIndexProducer(state); err != nil {
		return false
	}
	metadataPath, ok := p.storedMetadataPath(ctx, sid, snap)
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
	if state.ArtifactHash == nil {
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
// repair runs automatically once the source is available again. A completed
// index is the only state that still has something to serve, so the message
// promises that copy only then. A non-nil cause is the acquisition failure
// that stopped the repair, and it leads the message so a source that is
// present but unusable names its real cause (a malformed record and its line)
// instead of reading as a missing file.
func (p *Pipeline) reportPairRepairUnavailable(ctx context.Context, sid SessionID, source string, cause error, lane *storeWriteLane) {
	// A nil cause is a missing source: the repair was not attempted because no
	// file could be reached. Any other cause is classified by its source bytes.
	causeClass, shaped := SourceUnavailableNoSavedCopy, true
	if cause != nil {
		causeClass, shaped = sourceUnavailableCause(cause)
	}
	if shaped {
		if !p.settleUnavailableSource(ctx, sid, causeClass, lane) || p.laneCauseAlreadyReported(sid, causeClass) {
			// A routine harvest stays quiet once this cause is recorded, and
			// so does this lane when stored-content recovery already reported
			// the same cause in this harvest. A store, locator, or write fault
			// is not a source cause: it is not recorded and keeps reporting.
			return
		}
		p.markLaneCauseReported(sid, causeClass)
	}
	serving := p.hasCompletedStoredIndex(ctx, sid)
	message := missingPairText(sid, source)
	remediation := "Restore the original harness source and rerun harvest."
	if serving {
		remediation = "Restore the original harness source and rerun harvest; the stored copy keeps serving meanwhile."
	}
	if cause != nil {
		message = fmt.Sprintf("session %s could not be re-ingested from its original source %q: %v", sid, source, cause)
		remediation = "Restore a complete, supported transcript at the original source and rerun harvest."
		if serving {
			message += "; the stored copy keeps its last-good entries"
			remediation = "Restore a complete, supported transcript at the original source and rerun harvest; the stored copy keeps serving meanwhile."
		}
	}
	p.reportSessionDiagnostic(sid, DiagnosticEntry{
		ErrorType:   "pair_repair_unavailable",
		Location:    fmt.Sprintf("session %s pair repair", sid),
		Message:     message,
		Remediation: remediation,
	})
}

// hasCompletedStoredIndex reports whether the stored index is finished and can
// keep serving while its pair cannot be rebuilt. It reads the row's snapshot and
// asks SessionIndexState.Completed, the one definition of a finished index, so a
// row that has served keeps its promise and one that never completed does not
// make one. A missing row, or a store that cannot answer, has nothing to serve.
func (p *Pipeline) hasCompletedStoredIndex(ctx context.Context, sid SessionID) bool {
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return false
	}
	state, err := reader.ReadIndexState(ctx, sid)
	return err == nil && state.Completed()
}

// noteUnroutedForcedRepair records one forced repair that could not be sent to
// native re-ingestion. inScope is the run's explicit-filter verdict, taken
// where the repair was selected: a repair the run's filters exclude is not this
// run's failed session, and a --since window is one of those filters. A repair
// that reached the selection is already in scope, so a harness the store cannot
// name keeps it instead of dropping it. A routine harvest does not call
// through: the diagnostic is the whole report. A dry run plans and does not
// fail sessions. The same session is recorded once. The process path's own
// error, when it already failed the session, is the one the summary counts.
func (p *Pipeline) noteUnroutedForcedRepair(sid SessionID, harness Harness, source string, inScope bool) {
	if !inScope || !p.config.Force || p.config.DryRun {
		return
	}
	p.unroutedRepairMu.Lock()
	defer p.unroutedRepairMu.Unlock()
	if p.unroutedRepairSeen == nil {
		p.unroutedRepairSeen = make(map[SessionID]struct{})
	}
	if _, seen := p.unroutedRepairSeen[sid]; seen {
		return
	}
	p.unroutedRepairSeen[sid] = struct{}{}
	p.unroutedRepairs = append(p.unroutedRepairs, SessionResult{
		SessionID: sid,
		Harness:   harness,
		Status:    DiffUpdated,
		Error:     unroutedRepairError(sid, source),
	})
}

func unroutedRepairError(sid SessionID, source string) error {
	if source == "" {
		return fmt.Errorf("repair session %s: no recorded original source could be reached during this harvest, so the forced repair did not take and nothing was indexed; restore the original harness source and rerun harvest", sid)
	}
	return fmt.Errorf("repair session %s: original source %q could not be reached during this harvest, so the forced repair did not take and nothing was indexed; restore that source and rerun harvest", sid, source)
}

// storedSessionHarness reads the harness recorded with the index snapshot.
// An unrouted repair may have no discovered session to name it.
func (p *Pipeline) storedSessionHarness(ctx context.Context, sid SessionID) Harness {
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return ""
	}
	state, err := reader.ReadIndexState(ctx, sid)
	if err != nil || state == nil {
		return ""
	}
	return state.Harness
}

// appendUnroutedRepairFailures adds forced repairs that never entered the
// process path. A session the results already carry is left as the process path
// recorded it, so one repair never produces two rows: an already failed session
// keeps the process path's error, and a session the process path settled is not
// failed by a selection-stage note it outgrew.
func (p *Pipeline) appendUnroutedRepairFailures(results []SessionResult) []SessionResult {
	p.unroutedRepairMu.Lock()
	pending := append([]SessionResult(nil), p.unroutedRepairs...)
	p.unroutedRepairMu.Unlock()
	if len(pending) == 0 {
		return results
	}
	present := make(map[SessionID]bool, len(results)+len(pending))
	for _, result := range results {
		present[result.SessionID] = true
	}
	for _, result := range pending {
		if present[result.SessionID] {
			continue
		}
		results = append(results, result)
		present[result.SessionID] = true
	}
	return results
}

func (p *Pipeline) resetUnroutedRepairs() {
	p.unroutedRepairMu.Lock()
	defer p.unroutedRepairMu.Unlock()
	p.unroutedRepairs = nil
	p.unroutedRepairSeen = nil
}

// pairRepairInScope applies the run's explicit filters to a repair candidate.
// The harness half is the tolerant one (includesManagedSessionOrUnknown): the
// repair already reached the selection from the database, so a harness the
// store could not name keeps the candidate while only a named harness can fail
// a --harness filter. The session half and the age window are the same ones
// every inventory applies. A start the candidate itself carries is used when
// the caller has none, because a repair selected from native discovery has no
// metadata start to read; discoveredSessionStart resolves it, preferring the
// created time over the file time. A session with neither is excluded when
// --since is set, matching how the index selection treats a reconstructed
// target without a recorded start.
func (p *Pipeline) pairRepairInScope(session DiscoveredSession, startMs int64) bool {
	if !p.includesManagedSessionOrUnknown(session.SessionID, session.Harness) {
		return false
	}
	if p.config.Since == nil {
		return true
	}
	if startMs <= 0 {
		startMs = discoveredSessionStart(session)
	}
	if startMs <= 0 {
		// An unknown start cannot be placed inside a window, and this branch is
		// only reached when one is set.
		return false
	}
	return p.withinRunAge(startMs)
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

// queuedForNativeChange reports whether an entry already queued for other work
// was queued for a reason that proves the queued path rewrites the pair from
// native input: a forced run, a schema this build re-extracts from native
// input, or a newer clock on a row with no supported stored fingerprint that
// could overrule it. Such a session recreates a missing sidecar and republishes
// the pair, so a selection-stage damage verdict for it would duplicate a read
// the queued path does not need. It deliberately does not prove the native
// bytes changed, and it does not defer arbitrary producer-version queues. The
// stored row is read from the location cache, so the decision reads no file.
func (p *Pipeline) queuedForNativeChange(session DiscoveredSession) bool {
	// A forced run re-acquires native input for every queued session.
	if p.config.Force {
		return true
	}
	loc, ok := p.locationCache[session.SessionID]
	if !ok || loc.SchemaVersion > CurrentSchemaVersion {
		return false
	}
	// A schema this build re-extracts from native input forces the update.
	if metadataNeedsNativeRefresh(loc.SchemaVersion) {
		return true
	}
	if loc.IngestedMs == nil || *loc.IngestedMs <= 0 || !session.ModTime.After(time.UnixMilli(*loc.IngestedMs)) {
		return false
	}
	// The newer mod time is only a clock hint. For a supported captured source
	// that already holds a fingerprint, the post-capture comparison can overrule
	// it with an unchanged verdict before anything is published, and the queued
	// worker then exits without rewriting the pair. Deferring detection there
	// would let a damaged pair on unchanged bytes lose its repair, so keep the
	// detection read whenever a stored fingerprint can overrule the hint. An
	// empty fingerprint cannot: the capture is forced to update.
	return !(loc.SourceEvidenceSupported && len(loc.SourceFingerprint) > 0)
}

// appendPairRepairWork appends the stored sessions whose saved pair is missing
// or damaged as native re-ingest work. The repair used to be an explicit
// `--force --session` action; a session with no usable stored input has
// nothing to protect, so the ordinary harvest performs it automatically. A
// session already queued for other work is marked for repair in place, so a
// database-first "unchanged" verdict cannot leave the damaged pair behind. The
// one exception is a session whose queue reason proves the queued path rewrites
// the pair from native input: a forced run, a schema this build re-extracts, or
// a newer clock on a row with no stored fingerprint that could overrule it.
// Selection does not read the pair for those.
func (p *Pipeline) appendPairRepairWork(ctx context.Context, entries []DiffEntry, discovered []DiscoveredSession) []DiffEntry {
	// A fresh set each run: the index inventory reads it to skip candidates
	// this pass owns, and a reused pipeline must not carry stale entries.
	p.pairRepairOwned = make(map[SessionID]bool)
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
	snap := p.snapshotLocations(ctx, ids)
	outcomes := runParallel(ctx.Err, ids, parallelWorkers(p.config), func(sid SessionID) pairRepairOutcome {
		return p.pairRepairDecision(ctx, sid, entries, entryIndex, native, snap)
	})
	var repaired []SessionID
	for i, outcome := range outcomes {
		if outcome.unavailableSource != nil {
			p.reportPairRepairUnavailable(ctx, ids[i], *outcome.unavailableSource, nil, nil)
			// The session was not queued, so the process path will not record
			// this failure. An explicit forced run still has to count it, but
			// only when the run's own filters cover the repair.
			p.noteUnroutedForcedRepair(ids[i], outcome.unavailableHarness, *outcome.unavailableSource, outcome.unavailableInScope)
		}
		if outcome.damaged {
			// The pair is not a readable index input. Record it so the index
			// inventory does not re-read it and repeat the acquisition failure.
			p.pairRepairOwned[ids[i]] = true
		}
		switch outcome.kind {
		case pairRepairMarkQueued:
			entries[outcome.queuedIndex].pairRepair = true
			entries[outcome.queuedIndex].repairMetadataPath = outcome.metadataPath
		case pairRepairAppend:
			entries = append(entries, outcome.entry)
		default:
			continue
		}
		repaired = append(repaired, ids[i])
	}
	// The location cache is written only here, after every worker finished.
	p.cacheRepairLocations(ctx, repaired)
	return entries
}

// pairRepairOutcomeKind is what the repair selection decided for one candidate.
type pairRepairOutcomeKind int

const (
	pairRepairNone       pairRepairOutcomeKind = iota // no repair work
	pairRepairMarkQueued                              // mark the already-queued entry
	pairRepairAppend                                  // append a new repair entry
)

// pairRepairOutcome carries one candidate's decision from a parallel worker
// back to the serial pass that applies it in candidate order.
type pairRepairOutcome struct {
	unavailableSource  *string
	unavailableHarness Harness
	// unavailableInScope is the run's explicit-filter verdict for the repair
	// this candidate could not route. A candidate the run's filters exclude is
	// reported, but its unrouted repair is not counted as this run's failure.
	unavailableInScope bool
	kind               pairRepairOutcomeKind
	queuedIndex        int
	metadataPath       string
	entry              DiffEntry
	// damaged reports that the candidate's saved pair is missing or damaged,
	// whether or not this pass found a native source to repair it from. The
	// index inventory must not re-attempt such a candidate: its pair is not a
	// readable index input, and the pair-repair pass owns it (re-ingesting from
	// native when a source is available, reporting it once when not).
	damaged bool
}

// pairRepairDecision decides one repair candidate. It only reads entries, the
// location cache and the snapshot; diagnostics go through the locked reporter.
func (p *Pipeline) pairRepairDecision(ctx context.Context, sid SessionID, entries []DiffEntry, entryIndex map[SessionID]int, native map[SessionID]DiscoveredSession, snap locationSnapshot) pairRepairOutcome {
	if i, queued := entryIndex[sid]; queued {
		if p.queuedForNativeChange(entries[i].Session) {
			// Queued for a proven rewrite reason (a forced run, a
			// native-refresh schema, or a clock hint no stored fingerprint
			// can overrule): the queued path re-acquires native input and
			// rewrites the pair, recreating a missing sidecar, so a
			// selection-stage damage verdict would only add a read the path
			// does not need.
			return pairRepairOutcome{}
		}
		if !p.pairNeedsRepairIn(ctx, sid, snap) {
			return pairRepairOutcome{}
		}
		// Queued for a reason that can settle without republishing the
		// pair: index readiness, or a newer clock that captured fingerprint
		// comparison may classify unchanged before any write. Carry the
		// damage verdict onto the existing entry so a database-first no-op
		// cannot leave the damaged pair behind.
		metadataPath, _ := p.storedMetadataPath(ctx, sid, snap)
		return pairRepairOutcome{kind: pairRepairMarkQueued, queuedIndex: i, metadataPath: metadataPath, damaged: true}
	}
	if !p.pairNeedsRepairIn(ctx, sid, snap) {
		return pairRepairOutcome{}
	}
	metadataPath, _ := p.storedMetadataPath(ctx, sid, snap)
	session, found := native[sid]
	if !found {
		reconstructed, startMs, _, err := p.reconstructFromSourceInfo(ctx, sid)
		if err != nil {
			p.reportMetadataRefusal(string(sid), err)
			return pairRepairOutcome{damaged: true}
		}
		if reconstructed == nil {
			source := ""
			harness := p.storedSessionHarness(ctx, sid)
			// No file pair and no recorded locator: only the stored harness is
			// known, so the run's filters decide without a start.
			return pairRepairOutcome{
				damaged:            true,
				unavailableSource:  &source,
				unavailableHarness: harness,
				unavailableInScope: p.pairRepairInScope(DiscoveredSession{SessionID: sid, Harness: harness}, 0),
			}
		}
		session = *reconstructed
		if !p.pairRepairInScope(session, startMs) {
			return pairRepairOutcome{damaged: true}
		}
	} else if !p.pairRepairInScope(session, 0) {
		return pairRepairOutcome{damaged: true}
	}
	if !p.pairSourceAvailable(session) {
		source := session.SourcePath.String()
		// The scope was applied immediately above, so this repair is this
		// run's work and its failure is this run's failure.
		return pairRepairOutcome{
			damaged: true, unavailableSource: &source,
			unavailableHarness: session.Harness, unavailableInScope: true,
		}
	}
	return pairRepairOutcome{kind: pairRepairAppend, entry: DiffEntry{
		Session: session, Status: DiffUpdated, pairRepair: true, repairMetadataPath: metadataPath,
	}, damaged: true}
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
		session, startMs, transcriptPath, err := p.reconstructFromSourceInfo(ctx, sid)
		if err != nil {
			p.reportMetadataRefusal(string(sid), err)
			continue
		}
		if session == nil {
			p.reportPairRepairUnavailable(ctx, sid, "", nil, nil)
			// No target is added, so the reindex process path never sees this
			// session and the process path cannot count the repair that could
			// not be routed. A forced run still has to count it, but only when
			// the run's own filters cover the repair.
			harness := p.storedSessionHarness(ctx, sid)
			p.noteUnroutedForcedRepair(sid, harness, "", p.pairRepairInScope(DiscoveredSession{SessionID: sid, Harness: harness}, 0))
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
// Only failed native acquisition permits the last-good stored index fallback.
func (p *Pipeline) processSession(ctx context.Context, entry DiffEntry, writeLane *storeWriteLane) workerResult {
	var metadata *UnifiedMetadata
	var metadataErr error
	var metadataPath string
	var pathErr error
	if entry.pairRepair {
		// The repair already resolved the session's owned locator, and a pair
		// that cannot be read has no stored-first path to prefer. Both
		// lookups are skipped so a missing sidecar cannot fall through to a
		// whole-tree lookup.
		metadataPath = entry.repairMetadataPath
	} else {
		metadata, metadataErr = p.metadataForRewrite(ctx, entry.Session)
		metadataPath, pathErr = p.findMetadataPath(ctx, entry.Session)
	}
	// A forced run is an explicit manual refresh and tries native input first;
	// routine version-driven maintenance prefers sufficient stored input.
	if metadataErr == nil && pathErr == nil && metadata != nil && p.adapterNeedsRefresh(metadata) && !metadataNeedsNativeRefresh(metadata.SchemaVersion) && !p.config.Force && !entry.pairRepair && !p.nativeInputChanged(entry.Session, metadata) {
		refreshed := p.processRetainedSession(ctx, entry.Session, metadataPath, writeLane)
		var insufficient *InsufficientRetainedInputError
		if refreshed.result.Error == nil || !errors.As(refreshed.result.Error, &insufficient) {
			return refreshed
		}
	}
	var result workerResult
	if entry.retainedOnly && metadata != nil && (metadata.Redaction.Applied || len(metadata.Subagents) > 0) {
		result.result = SessionResult{SessionID: entry.Session.SessionID, Harness: entry.Session.Harness, ParentUUID: entry.Session.ParentUUID, Status: entry.Status,
			Error: &adapterAcquisitionError{cause: fmt.Errorf("refresh stored session %s from native input: required original discovery context is unavailable or redacted; no native attribution or child paths were guessed; restore native discovery and retry harvest", entry.Session.SessionID)}}
	} else {
		result = p.processNativeSession(ctx, entry, writeLane)
	}
	var acquisition *adapterAcquisitionError
	if !errors.As(result.result.Error, &acquisition) || metadataPath == "" || pathErr != nil {
		return result
	}
	if entry.pairRepair {
		// A repair has no usable stored pair by definition: when the native
		// source cannot be acquired, the failure is the outcome. Falling back
		// would try to read the missing or damaged pair and hide the error.
		p.reportPairRepairUnavailable(ctx, entry.Session.SessionID, string(entry.Session.SourcePath), acquisition, writeLane)
		// A routine harvest reports a source-shaped refusal instead of failing
		// on it, but only while a completed stored index is what still serves.
		// A store, locator, or write fault keeps the failure even then. An
		// explicit --force run is the remediation the diagnostic names, so it
		// keeps the failure: a person or a script has to be able to tell that
		// the repair did not take.
		if !p.config.Force && sourceUnavailable(acquisition) && p.hasCompletedStoredIndex(ctx, entry.Session.SessionID) {
			result.result.Error = nil
			result.result.Status = DiffUnchanged
		}
		return result
	}
	// The manual index refresh (harvest index --force) names its own
	// unavailable native input; every other path, including a forced ordinary
	// harvest, reports the adapter refresh it could not complete.
	errorType, location := "adapter_refresh_unavailable", fmt.Sprintf("%s session %s adapter refresh", entry.Session.Harness, entry.Session.SessionID)
	if p.config.Reindex && p.config.Force {
		errorType, location = "native_refresh_unavailable", fmt.Sprintf("%s session %s forced refresh", entry.Session.Harness, entry.Session.SessionID)
	}
	p.reportSessionDiagnostic(entry.Session.SessionID, DiagnosticEntry{
		ErrorType: errorType, Location: location,
		Message:     acquisition.Error() + "; the previous artifact and adapter stamp were preserved; supported stored indexing may continue",
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
	// guards independently verify that this stored pair matches its stored state.
	result.meta = &retained.Metadata
	result.transcriptData = retained.Transcript
	// Carry the exact metadata bytes this read validated, beside the
	// transcript from the same read, so the index forms one snapshot. This
	// field deliberately does not set result.artifact: the run committed no
	// pair, and claiming one would mirror a phantom pair and report a false
	// publication.
	result.retainedMetadataJSON = retained.MetadataJSON
	result.result.OutputPath = filepath.Dir(metadataPath)
	result.outputTranscriptPath = filepath.Join(result.result.OutputPath, string(entry.Session.SessionID)+"--transcript."+string(retained.Metadata.Source.Format))
	result.originalRoot = entry.Session.OriginalRoot
	result.transcriptOrigin = entry.Session.TranscriptOrigin
	result.startMs = retained.Metadata.Timestamp.Start
	return result
}

// hasUsableRetainedSession decides, only when native discovery FAILED, whether
// the harvest can still maintain a session it saved earlier. It is the one
// place the stored tree is read on the ordinary path, and only on a
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
		if !p.withinRunAge(artifact.Metadata.Timestamp.Start) {
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
		p.reportMetadataRefusal("stored discovery recovery", fmt.Errorf("read stored recovery candidates after native discovery failed: %w; no recovery was attempted; restore database access and retry harvest", err))
		return false
	}
	for _, sid := range staleIDs {
		session, _, transcriptPath, err := p.reconstructFromSourceInfo(ctx, sid)
		if err != nil {
			p.reportMetadataRefusal(string(sid), err)
			continue
		}
		if session == nil {
			continue
		}
		// A lost metadata file beside an intact transcript is still a usable
		// stored session: the index-recovery pass reconstructs it from stored
		// source information and rebuilds the pair. The recoverable evidence is
		// the transcript, not the metadata the recovery will rewrite, so confirm
		// the transcript reads before declaring native discovery failure fatal.
		if _, err := p.fs.Stat(transcriptPath); err == nil {
			return true
		}
	}
	return false
}
