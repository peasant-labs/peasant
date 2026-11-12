package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// includesIndexTarget is the index inventory's scope test: the strict harness
// and session halves from includesManagedSession plus the run's age window.
func (p *Pipeline) includesIndexTarget(target reindexTarget) bool {
	return p.includesManagedSession(target.session.SessionID, target.session.Harness) &&
		p.withinRunAge(target.startMs)
}

// nativeRefreshSchemaVersions is the exact set of stored schema versions this
// build re-extracts from native input. The database adapter-refresh predicate
// binds this list, so it and metadataNeedsNativeRefresh cannot drift.
func (p *Pipeline) nativeRefreshSchemaVersions() []int {
	var versions []int
	for version := 1; version <= int(CurrentSchemaVersion); version++ {
		if metadataNeedsNativeRefresh(version) {
			versions = append(versions, version)
		}
	}
	return versions
}

// repairSessions returns the sessions the repair predicate selects: those a
// crash between the two write commits can leave. A store that cannot answer
// contributes nothing; the sessions are re-selected on the next harvest.
func (p *Pipeline) repairSessions(ctx context.Context) []SessionID {
	lister, ok := p.metricsStore.(RepairSessionLister)
	if !ok {
		return nil
	}
	ids, err := lister.ListSessionsNeedingRepair(ctx, p.indexerTargets(), p.runStoreScope())
	if err != nil {
		p.reportMetadataRefusal("repair selection", err)
		return nil
	}
	return ids
}

// capturedInputNeedsWork decides whether a captured input still has pending
// indexer work. A current parser and an equal input hash are not enough on
// their own: the stored index must also be bound to the current publication
// metadata capture, and a harness whose strict parser can certify complete
// content must have done so. Both are settled by the ordinary index write, so
// a repair completes in one harvest and the next unchanged harvest does no
// parser work.
func (p *Pipeline) capturedInputNeedsWork(input *CapturedIndexInput) bool {
	target := p.sessionVersionTarget(input.session)
	expected := input.expected
	// A latest attempt that settled against this session's current pair, the
	// stored producer revision, and its generation's adapter revision owes no
	// parser work: the attempt already consumed the current pair under this
	// build. The pair facet is bound to the work selector's own evidence here,
	// so an input captured from a different pair is never treated as settled.
	// A deliberate reindex disables the short-circuit (settled stays false), so
	// a pending term re-attempts the session once; Force selects outright. The
	// nil guard is local, so the dereference never depends on the helper's own
	// nil check.
	settled := false
	if pair := expected.LatestAttemptArtifactHash; pair != nil && !p.config.Reindex {
		settled = latestAttemptPreviewSettles(expected, target) && input.artifactHash == *pair
	}
	return p.config.Force ||
		(expected.IndexerVersion < target.IndexerVersion && !settled) ||
		(expected.IndexedInputHash == nil || *expected.IndexedInputHash != input.inputHash) && !settled ||
		// A current metadata capture exists (revision > 0) but the index is not
		// bound to it: bytes match, publication proof does not. Only an input
		// the capture can vouch for is pending on that account.
		(expected.PublicationCaptureRevision > 0 && !expected.PublicationBound && input.bindsPublication()) && !settled ||
		// The capture is incomplete and this build can certify it: the strict
		// format-1 parser is the only path that produces complete content, so
		// a harness on another declared format is at its steady state instead.
		// A refusal this build already recorded for this producer is a steady
		// state too, and is excluded below.
		(expected.ContentStatus != ContentCaptureComplete && p.certifiesContent(input.session.Harness) &&
			!permanentRefusalIsSettled(expected, target)) && !settled
}

// capturedInputBindsWithoutParse reports that the stored index already
// describes a captured input and only its bookkeeping is behind. That is the
// state a metadata-only install leaves: the transcript file was kept, so the
// stored input proof still equals the captured input, and the producer that
// built the index is still current. Re-parsing would reproduce the stored
// entries, so the write is one constant-size settle: it advances the index
// time past the save and binds the index to the newer metadata capture.
//
// It applies when the pair was saved this run or the index is unbound from a
// capture the input can vouch for. Every other pending term keeps the
// ordinary parse: an explicit rebuild, a behind producer or index format, a
// missing or different input proof, content this build could still certify,
// and a latest attempt held beside the active authority, whose retry is the
// parse's to make.
func (p *Pipeline) capturedInputBindsWithoutParse(input *CapturedIndexInput) bool {
	if _, ok := p.metricsStore.(PublicationIndexBinder); !ok {
		return false
	}
	target := p.sessionVersionTarget(input.session)
	state := input.expected
	if state == nil || p.config.Force || p.config.Reindex || state.LatestAttemptArtifactHash != nil {
		return false
	}
	if state.IndexerVersion < target.IndexerVersion || state.IndexVersion == nil || *state.IndexVersion != target.IndexVersion {
		return false
	}
	if state.IndexedInputHash == nil || *state.IndexedInputHash != input.inputHash {
		return false
	}
	if state.ContentStatus != ContentCaptureComplete && p.certifiesContent(input.session.Harness) && !permanentRefusalIsSettled(state, target) {
		return false
	}
	return input.published || (state.PublicationCaptureRevision > 0 && !state.PublicationBound && input.bindsPublication())
}

// latestAttemptPreviewSettles reports whether a stored latest attempt has
// settled against this session's current facts and this build's targets. A
// settled attempt owes no parser work: re-reading it would reproduce the same
// bytes under the same producer and adapter revisions.
//
// Every facet is required:
//
//   - pair: the recorded attempt identity and the session's current artifact
//     hash are BOTH non-null and equal (a NULL is never claimed as equality);
//   - producer: the recorded attempt producer revision meets the target;
//   - adapter: the attempt generation's recorded adapter revision is non-null
//     and meets the target.
//
// A session with no latest attempt has nil facets and is never settled, so the
// ordinary selection predicate is unchanged for it. A facet that moved keeps
// the session selected: the next run re-attempts once and settles.
func latestAttemptPreviewSettles(state *SessionIndexState, target HarvesterVersions) bool {
	// A target that cannot express a settle decision never settles, mirroring
	// the store surfaces' own guard.
	if target.IndexerVersion < 1 || target.AdapterVersion < 1 {
		return false
	}
	if state == nil || state.LatestAttemptArtifactHash == nil || state.ArtifactHash == nil {
		return false
	}
	if *state.LatestAttemptArtifactHash != *state.ArtifactHash {
		return false
	}
	if state.LatestAttemptIndexerVersion == nil || *state.LatestAttemptIndexerVersion < target.IndexerVersion {
		return false
	}
	if state.LatestAttemptAdapterVersion == nil || *state.LatestAttemptAdapterVersion < target.AdapterVersion {
		return false
	}
	return true
}

// bindsPublication reports whether an index written from this input may be
// bound to the current publication metadata capture. A managed transcript's
// bytes are what the capture hashed, so a file input binds. A directory tree
// is read beside a header the capture hashed without the tree, so a tree read
// from stored input has no byte proof against the capture and its index is
// held unbound; the tree this run captured with the artifact is the tree the
// capture saw, and binds.
func (input *CapturedIndexInput) bindsPublication() bool {
	return input.kind != TranscriptSourceDirectory || input.published
}

// permanentRefusalIsSettled reports that the incomplete capture is as good as
// this build can make it, so re-parsing it would fail the same way again.
//
// The stored capture records a refusal NOTHING ABOUT THIS BUILD CAN LIFT: the
// strict parser rejected a record it does not represent, or the retained
// transcript is known to be missing a record ingest removed for being longer
// than the scanner's line limit, and re-reading the same source omits it
// again. The producer that recorded the refusal is the producer this build
// would use, and the bytes have not moved.
//
// Either of the two things that could change the answer lifts the steady state
// through the terms above: a newer indexer fails the version comparison, and
// new bytes fail the input hash. Without this, such a session is parsed
// strictly, parsed tolerantly and re-stamped on every harvest, and warns the
// user about a condition they cannot act on until Peasant is upgraded.
func permanentRefusalIsSettled(expected *SessionIndexState, target HarvesterVersions) bool {
	return permanentCaptureRefusal(expected.ContentFailureCode) &&
		expected.IndexerVersion >= target.IndexerVersion &&
		expected.IndexedInputHash != nil
}

// StoredRefusalIsFinal reports whether an index run of this build leaves a stored
// capture refusal in place. It reads the same predicate the selector uses,
// against the newest target this build holds for the harness, so a session a
// run would still upgrade is never reported as final.
func StoredRefusalIsFinal(state *SessionIndexState) bool {
	target, managed := GenerationRepairTargets[state.Harness]
	if !managed {
		target = HarvesterVersionRegistry[state.Harness]
	}
	return permanentRefusalIsSettled(state, target)
}

// permanentRefusalSettled reports whether a stored session location carries a
// refusal this build cannot lift, using the same predicate the index selector
// uses. The location carries the raw stored columns and the caller supplies
// the harness target; the store computes no part of the decision, so a caller
// with no target (a discovery hint outside the pipeline) keeps the previous
// readiness-triggered behaviour rather than guessing one.
func (loc SessionLocation) permanentRefusalSettled(target HarvesterVersions) bool {
	return permanentRefusalIsSettled(&SessionIndexState{
		ContentFailureCode: loc.ContentFailureCode,
		IndexerVersion:     loc.IndexerVersion,
		IndexedInputHash:   loc.IndexedInputHash,
	}, target)
}

// permanentCaptureRefusal reports whether a recorded failure code is one this
// build can never clear on its own. A capture nothing refused, and a capture
// that predates content capture, are both PENDING work rather than settled:
// they have simply never been tried by a build that could certify them.
func permanentCaptureRefusal(code ContentCaptureFailureCode) bool {
	return code == ContentCaptureStrictRefused || code == ContentCaptureSourceRecordsOmitted || code == ContentCaptureUnknownDataRetained
}

// certifiesContent reports whether this build's indexer for the harness can
// produce a verified complete capture: a strict parser writing the strict
// stored format. Anything else stores what it parsed without certification.
func (p *Pipeline) certifiesContent(harness Harness) bool {
	_, strict := p.indexers[harness].(AuthoritativeTranscriptIndexer)
	return strict && p.versionTargets()[harness].IndexVersion == strictIndexFormat
}

func (p *Pipeline) indexTargetNeedsWork(ctx context.Context, target reindexTarget) bool {
	if !p.includesIndexTarget(target) || p.metricsStore == nil {
		return false
	}
	// A session whose stored metadata schema is newer than this build is
	// refused here, before it can become a target: the refusal is one
	// diagnostic on the run, never a structured log line or an index-log entry.
	if err := p.checkStoredMetadataVersion(ctx, target.session.SessionID); err != nil {
		p.reportMetadataRefusal(string(target.session.SessionID), err)
		return false
	}
	if _, ok := p.indexers[target.session.Harness]; !ok {
		return false
	}
	// Selection is database-first: it decides from the stored index state and
	// never opens the pair. The pair is read only when a chosen target is
	// indexed. A pair whose bytes changed through the write path had its
	// indexed_input_hash cleared before installation (and kept NULL by the
	// mirror), so process interruption selects it even before mirror commit.
	// Hand edits or older unmarked damage on a settled row leave no database
	// signal and are not found by this scan: that damage
	// is reported as damaged on the next pair read (harvest index --all, the
	// content stage, or peasant redact), through the pair hash check.
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return true
	}
	state, stateErr := reader.ReadIndexState(ctx, target.session.SessionID)
	if stateErr != nil {
		return true
	}
	return p.stateNeedsIndexWork(state, p.sessionVersionTarget(target.session))
}

// stateNeedsIndexWork decides from stored SQL state alone whether a session has
// pending indexer work, reading no file. It is the database-first half of
// capturedInputNeedsWork: it drops the input-hash comparison and the
// publication byte proof, both of which need the pair. A changed pair reaches
// this predicate as a NULL indexed_input_hash, because the write path clears
// the proof before installation and the mirror preserves it; a revision left unbound is selected so
// the ordinary index write can bind it; and a stored producer or index format
// newer than this build is selected so the shared parse path reports the
// refusal once. A session with no stored pair identity is selected so the
// index path can establish it. The caller supplies the session's effective
// target, so a session with no readable native snapshot compares against the
// stored baseline instead of being selected forever against the native
// override.
func (p *Pipeline) stateNeedsIndexWork(state *SessionIndexState, target HarvesterVersions) bool {
	if state == nil || state.ArtifactHash == nil {
		return true
	}
	if p.checkIndexProducer(state) != nil {
		return true
	}
	// A settled latest attempt short-circuits every pending-work term except an
	// explicit rebuild: the attempt already consumed the current pair under this
	// build, so re-parsing it would be pure churn. Force selects outright; the
	// deliberate reindex disables the short-circuit (settled stays false), so
	// the ordinary terms decide and a pending fact re-attempts the session
	// once. A reindex is not a full bypass: a session with no pending term
	// still settles.
	settled := !p.config.Reindex && latestAttemptPreviewSettles(state, target)
	return p.config.Force ||
		(state.IndexerVersion < target.IndexerVersion && !settled) ||
		(state.IndexedInputHash == nil && !settled) ||
		(state.PublicationCaptureRevision > 0 && !state.PublicationBound && !settled) ||
		(state.ContentStatus != ContentCaptureComplete &&
			p.certifiesContent(state.Harness) &&
			!permanentRefusalIsSettled(state, target) && !settled)
}

// scanPeasantSyncSessions walks the stored tree and returns one target per
// session it holds. It is the whole-tree reader, and only `harvest index`
// runs it: the ordinary harvest selects its work from the database. It opens
// no transcript and takes no lock; the pair is read only when a chosen target
// is indexed.
func (p *Pipeline) scanPeasantSyncSessions(ctx context.Context) []reindexTarget {
	var targets []reindexTarget
	err := walkManagedMetadata(ctx, p.fs, string(p.config.OutputDir), func(sid SessionID, path string) error {
		metadata, err := p.readSessionMetadata(filepath.Dir(filepath.Dir(path)), sid, "index inventory")
		if err != nil || metadata == nil {
			return nil // The metadata reader reports refusal; independent peers continue.
		}
		targets = append(targets, reindexTarget{session: metadata.session, startMs: metadata.startMs, transcriptPath: metadata.transcriptPath, originalSourcePath: metadata.originalSourcePath, refreshMetadata: metadata.refreshMetadata})
		return nil
	})
	if err != nil {
		p.reportMetadataRefusal(string(p.config.OutputDir), err)
	}
	return targets
}

// scopedIndexScan is what a session-scoped `harvest index` found in the saved
// tree. It is built before any stage runs, so a selection no saved pair can
// satisfy is refused with nothing recorded.
type scopedIndexScan struct {
	// targets are the saved pairs this run may index. Each pair is named by an
	// id the run already resolved; the tree is never walked to find one.
	targets []reindexTarget
	// report is the cohort the scan located: the selected sessions, their
	// children, and any unstored ancestor a saved pair holds.
	report ScopeReport
	// unreadable names the recorded sessions whose saved pair is gone. Their
	// stored transcripts are kept and their store-side upkeep still runs, but
	// their pairs are never rebuilt by this run.
	unreadable map[SessionID]bool
	// parents holds each located member's stored logical parent, so the
	// unstored-ancestor walk can continue from the pair it just found.
	parents map[SessionID]*SessionID
}

// scanScopedManagedSessions is the session-scoped variant of
// scanPeasantSyncSessions. It reconstructs each named session's saved pair from
// the location the store records, probes the retained tree for a session the
// store does not record, and never walks the output root.
//
// The scan reads and writes nothing: it builds the cohort report and the pairs
// this run may index, and a selected id no saved pair holds is a miss that stops
// the run before any stage records anything.
func (p *Pipeline) scanScopedManagedSessions(ctx context.Context) (*scopedIndexScan, error) {
	scope := p.config.Scope
	if scope == nil {
		return nil, fmt.Errorf("ingest: a session-scoped index scan was requested without a session scope; where: ingest.Pipeline.scanScopedManagedSessions in internal/ingest/index_selection.go; when: starting a `harvest index --session` run; impact: no saved pair was read and no store row was written; fix: build the scope with NewSessionScope, or run without --session")
	}
	reader, err := p.scopedIndexSourceReader()
	if err != nil {
		return nil, err
	}
	locations := p.compatibilityBacking()
	if locations == nil {
		return nil, fmt.Errorf("ingest: a session-scoped index run needs the analytics database to say where each named session's saved pair is; where: ingest.Pipeline.scanScopedManagedSessions in internal/ingest/index_selection.go; when: before any saved pair was read; impact: no saved pair was read and no store row was written; fix: run `harvest index` with its database attached")
	}
	tree, err := p.readIndexPairTree()
	if err != nil {
		return nil, err
	}

	selected := scope.IDs()
	stored, err := reader.StoredSessionSources(ctx, selected)
	if err != nil {
		return nil, fmt.Errorf("ingest: read the stored locations of the selected sessions before a session-scoped index run: %w; no saved pair was read and no store row was written; restore database access and retry", err)
	}
	storedLocations, err := locations.BulkLookupSessionLocations(ctx, selected)
	if err != nil {
		return nil, fmt.Errorf("ingest: read the stored pair locations of the selected sessions before a session-scoped index run: %w; no saved pair was read and no store row was written; restore database access and retry", err)
	}

	scan := &scopedIndexScan{
		unreadable: make(map[SessionID]bool, len(selected)),
		parents:    make(map[SessionID]*SessionID, len(selected)),
	}
	var misses []ScopeMiss

	for _, id := range selected {
		row, isStored := stored[id]
		if isStored {
			member, target, miss := p.locateStoredIndexPair(id, row, storedLocations[id], CohortSelected)
			if miss != nil {
				misses = append(misses, *miss)
				continue
			}
			scan.add(member, target)
			scan.parents[id] = row.Parent
			continue
		}
		result, probeErr := p.probeIndexPair(tree, id)
		if probeErr != nil {
			return nil, probeErr
		}
		if result == nil {
			misses = append(misses, ScopeMiss{ID: id, Reason: MissNotFound})
			continue
		}
		member := LocatedMember{ID: id, Harness: result.session.Harness, Outcome: LocateDirectLookup, Role: CohortSelected}
		if !p.includesIndexHarness(member.Harness) {
			misses = append(misses, ScopeMiss{ID: id, Reason: MissHarnessFiltered, StoredHarness: member.Harness})
			continue
		}
		if !p.withinRunAge(result.startMs) {
			misses = append(misses, ScopeMiss{ID: id, Reason: MissSinceFiltered})
			continue
		}
		target := indexTargetFrom(result)
		scan.add(member, &target)
		scan.parents[id] = result.session.ParentUUID
	}

	if len(misses) > 0 {
		return nil, p.scopedIndexMissError(misses)
	}

	if scope.Related() == RelatedInclude {
		if err := p.scanRelatedIndexPairs(ctx, reader, locations, tree, selected, scan); err != nil {
			return nil, err
		}
	}

	scan.report = orderScopeReport(scan.report.Members)
	return scan, nil
}

// scopedIndexSourceReader returns the store capability that says which named
// sessions are recorded and who their children are.
func (p *Pipeline) scopedIndexSourceReader() (SessionSourceReader, error) {
	if reader, ok := p.store.(SessionSourceReader); ok {
		return reader, nil
	}
	if reader, ok := p.metricsStore.(SessionSourceReader); ok {
		return reader, nil
	}
	return nil, fmt.Errorf("ingest: the attached store cannot say which sessions are recorded, so a session-scoped index run cannot tell a saved pair the database already knows from one a full index would record; where: ingest.Pipeline.scanScopedManagedSessions in internal/ingest/index_selection.go; when: starting a `harvest index --session` run; impact: no saved pair was read and no store row was written; fix: attach a store that implements SessionSourceReader, or run `harvest index` without --session")
}

// indexPairTree is the retained tree's host list, read once so every unstored
// probe costs one metadata read per host and never a directory walk.
type indexPairTree struct {
	output string
	hosts  []string
}

// readIndexPairTree reads the output root's host directories once. The list is
// the bound every saved-pair probe in this run is subject to.
func (p *Pipeline) readIndexPairTree() (*indexPairTree, error) {
	output := string(p.config.OutputDir)
	tree := &indexPairTree{output: output}
	entries, err := p.fs.ReadDir(output)
	if errors.Is(err, fs.ErrNotExist) {
		return tree, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ingest: inspect the retained transcript tree %q before a session-scoped index run: %w; where: ingest.Pipeline.readIndexPairTree in internal/ingest/index_selection.go; when: before any saved pair was read; impact: no saved pair was read and no store row was written; fix: restore read access to the output directory and retry", p.managedRelativePath(output), err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == managedArtifactStateDir || strings.HasPrefix(entry.Name(), defaults.TempDirPrefix) {
			continue
		}
		if _, err := NewHostSlug(entry.Name()); err != nil {
			continue
		}
		tree.hosts = append(tree.hosts, entry.Name())
	}
	sort.Strings(tree.hosts)
	return tree, nil
}

// probeIndexPair reads {output}/*/{id}/{id}--metadata.json for an id the store
// does not record. The probe only inspects the flat root layout: a subagent pair
// lives under its parent's directory, so an unstored subagent cannot be found by
// name. A nil result means no host holds the pair.
func (p *Pipeline) probeIndexPair(tree *indexPairTree, id SessionID) (*sessionMetadataResult, error) {
	for _, host := range tree.hosts {
		result, err := p.readSessionMetadata(filepath.Join(tree.output, host), id, "index inventory")
		if err != nil {
			return nil, fmt.Errorf("ingest: read the saved pair of session %s from the retained tree at %q: %w; where: ingest.Pipeline.probeIndexPair in internal/ingest/index_selection.go; when: probing the retained tree for the named session; impact: no saved pair was indexed and no store row was written; fix: repair or remove the damaged saved pair, then retry", id, filepath.Join(tree.output, host), err)
		}
		if result != nil {
			return result, nil
		}
	}
	return nil, nil
}

// locateStoredIndexPair reconstructs one recorded session's saved pair from the
// location the store holds. A pair the recorded location does not hold is
// unreadable, not a miss: the stored transcript is kept and the run continues.
func (p *Pipeline) locateStoredIndexPair(id SessionID, row StoredSessionSource, location SessionLocation, role CohortRole) (LocatedMember, *reindexTarget, *ScopeMiss) {
	member := LocatedMember{ID: id, Harness: row.Harness, Role: role, StoredPath: row.Path}
	if row.Harness == "" {
		member.Outcome = LocateStoredUnreadable
		member.Unreadable = UnreadableSourceFailed
		member.Detail = "the store records no harness for this session, so its saved pair cannot be attributed to an adapter"
		return member, nil, nil
	}
	// A named --harness is the more specific answer: the user excluded this
	// session on this run. The config's own disabled state answers only when no
	// harness filter was given.
	if !p.includesIndexHarness(row.Harness) {
		if role == CohortSelected {
			return member, nil, &ScopeMiss{ID: id, Reason: MissHarnessFiltered, StoredHarness: row.Harness}
		}
		// A related saved pair the harness filter excludes is dropped, which is
		// what a full `harvest index` with the same filter does: its scan never
		// lists the pair. The source-locating path instead marks such a related
		// member unreadable, because a full harvest's store-side upkeep still
		// reaches its row. The two commands differ here on purpose; this run
		// only maintains saved pairs, so an excluded pair is not its work.
		return member, nil, nil
	}
	if _, enabled := p.enabledSourceConfig(row.Harness); !enabled {
		member.Outcome = LocateStoredUnreadable
		member.Unreadable = UnreadableHarnessDisabled
		return member, nil, nil
	}
	if location.HostSlug == "" {
		member.Outcome = LocateStoredUnreadable
		member.Unreadable = UnreadableSourceFailed
		member.Detail = "the store records no host location for this session, so its saved pair cannot be reconstructed"
		return member, nil, nil
	}
	metadataPath := SessionMetadataPath(string(p.config.OutputDir), location.HostSlug, string(id), location.ParentID)
	member.StoredPath = ResolvedPath(metadataPath)
	result, err := p.readSessionMetadata(filepath.Dir(filepath.Dir(metadataPath)), id, "index inventory")
	if err != nil {
		member.Outcome = LocateStoredUnreadable
		member.Unreadable = UnreadableSourceFailed
		member.Detail = firstLineOr(err.Error(), "the saved pair could not be read")
		return member, nil, nil
	}
	if result == nil {
		member.Outcome = LocateStoredUnreadable
		member.Unreadable = UnreadableSourceGone
		return member, nil, nil
	}
	if role == CohortSelected && !p.withinRunAge(result.startMs) {
		return member, nil, &ScopeMiss{ID: id, Reason: MissSinceFiltered}
	}
	if role != CohortSelected && !p.withinRunAge(result.startMs) {
		return member, nil, nil
	}
	member.Outcome = LocateStoredPath
	target := indexTargetFrom(result)
	return member, &target, nil
}

// scanRelatedIndexPairs adds the stored children of each selected session and
// any unstored ancestor a saved pair holds. A stored ancestor stops the walk: it
// is not this run's work, and its own pair is irrelevant to the selected
// session's pair.
func (p *Pipeline) scanRelatedIndexPairs(ctx context.Context, reader SessionSourceReader, locations SessionStore, tree *indexPairTree, selectedIDs []SessionID, scan *scopedIndexScan) error {
	if len(selectedIDs) == 0 {
		return nil
	}
	children, err := reader.StoredChildren(ctx, selectedIDs)
	if err != nil {
		return fmt.Errorf("ingest: read the stored children of the selected sessions before a session-scoped index run: %w; no saved pair was indexed and no store row was written; restore database access and retry", err)
	}
	childIDs := make([]SessionID, 0, len(children))
	for _, rows := range children {
		for _, child := range rows {
			childIDs = append(childIDs, child.ID)
		}
	}
	sort.Slice(childIDs, func(i, j int) bool { return childIDs[i] < childIDs[j] })
	childLocations, err := locations.BulkLookupSessionLocations(ctx, childIDs)
	if err != nil {
		return fmt.Errorf("ingest: read the stored pair locations of the related sessions before a session-scoped index run: %w; no saved pair was indexed and no store row was written; restore database access and retry", err)
	}
	for _, id := range childIDs {
		if scan.has(id) {
			continue // already in the cohort under a higher-precedence role
		}
		row := storedChild(id, children)
		member, target, _ := p.locateStoredIndexPair(id, row, childLocations[id], CohortChild)
		if member.Outcome == locateOutcomeUnset {
			continue // dropped by the run's own filters
		}
		// The saved-pair scan knows the recorded parent, so the warning's
		// `also:` count names only this session's children.
		member.Parent = row.Parent
		scan.add(member, target)
		scan.parents[id] = row.Parent
	}

	// Unstored ancestors: the stored logical parent of a selected or child
	// member names an id the database has no row for. Its saved pair can still
	// be on disk, and a full scan would index it.
	starts := append(append([]SessionID(nil), selectedIDs...), childIDs...)
	for _, start := range starts {
		if err := p.walkUnstoredIndexAncestors(ctx, reader, tree, start, scan); err != nil {
			return err
		}
	}
	return nil
}

// storedChild finds the child row a StoredChildren map returned for id.
func storedChild(id SessionID, children map[SessionID][]StoredSessionSource) StoredSessionSource {
	for _, rows := range children {
		for _, row := range rows {
			if row.ID == id {
				return row
			}
		}
	}
	return StoredSessionSource{ID: id}
}

// walkUnstoredIndexAncestors follows the stored logical parent chain of start
// while each parent is unrecorded, probing the retained tree for that parent's
// saved pair. The walk stops at a recorded parent, a root, a miss, or an id
// already in the cohort.
func (p *Pipeline) walkUnstoredIndexAncestors(ctx context.Context, reader SessionSourceReader, tree *indexPairTree, start SessionID, scan *scopedIndexScan) error {
	seen := map[SessionID]bool{start: true}
	parent := scan.parents[start]
	for parent != nil && !seen[*parent] {
		ancestorID := *parent
		seen[ancestorID] = true

		rows, err := reader.StoredSessionSources(ctx, []SessionID{ancestorID})
		if err != nil {
			return fmt.Errorf("ingest: read the stored location of ancestor session %s before a session-scoped index run: %w; no saved pair was indexed and no store row was written; restore database access and retry", ancestorID, err)
		}
		if _, isStored := rows[ancestorID]; isStored {
			return nil // a recorded ancestor is not this run's work
		}

		result, probeErr := p.probeIndexPair(tree, ancestorID)
		if probeErr != nil {
			return probeErr
		}
		if result == nil || !p.includesIndexHarness(result.session.Harness) || !p.withinRunAge(result.startMs) {
			return nil
		}
		if scan.has(ancestorID) {
			return nil
		}
		member := LocatedMember{ID: ancestorID, Harness: result.session.Harness, Outcome: LocateDirectLookup, Role: CohortAncestor}
		target := indexTargetFrom(result)
		scan.add(member, &target)
		scan.parents[ancestorID] = result.session.ParentUUID
		parent = result.session.ParentUUID
	}
	return nil
}

// add records one located member and its saved pair.
func (s *scopedIndexScan) add(member LocatedMember, target *reindexTarget) {
	s.report.Members = append(s.report.Members, member)
	if member.Outcome == LocateStoredUnreadable && (member.Role == CohortSelected || member.Role == CohortChild) {
		s.unreadable[member.ID] = true
	}
	if target != nil {
		s.targets = append(s.targets, *target)
	}
}

// has reports whether the cohort already holds a member for id.
func (s *scopedIndexScan) has(id SessionID) bool {
	for _, member := range s.report.Members {
		if member.ID == id {
			return true
		}
	}
	return false
}

// indexTargetFrom turns one saved pair the metadata reader reconstructed into a
// reindex target.
func indexTargetFrom(result *sessionMetadataResult) reindexTarget {
	return reindexTarget{session: result.session, startMs: result.startMs, transcriptPath: result.transcriptPath, originalSourcePath: result.originalSourcePath, refreshMetadata: result.refreshMetadata}
}

// includesIndexHarness is the harness half of the run's explicit filters for a
// saved pair.
func (p *Pipeline) includesIndexHarness(harness Harness) bool {
	return p.config.Harness == nil || harness == *p.config.Harness
}

// enabledSourceConfig reports whether the harness that recorded a session is
// enabled in this run's configuration.
func (p *Pipeline) enabledSourceConfig(harness Harness) (SourceConfig, bool) {
	cfg, ok := p.config.Sources[harness]
	if !ok || !cfg.Enabled {
		return SourceConfig{}, false
	}
	return cfg, true
}

// scopedIndexMissError renders the refusal for the selected ids no saved pair
// can serve.
func (p *Pipeline) scopedIndexMissError(misses []ScopeMiss) error {
	command := p.config.Command
	if command == "" {
		command = "harvest index"
	}
	return &SessionScopeError{Misses: misses, Command: command, Prefix: p.config.CommandPrefix, Store: StoreAttached}
}

// rebuildIndexDerivedLookups reconciles the derived lookup tables from the walk
// this run just enumerated. `harvest index --all` runs it: the teammate spawns
// are re-derived from the stored transcript evidence, and the Codex rollout
// catalog from the enumeration the Codex walk reported.
func (p *Pipeline) rebuildIndexDerivedLookups(ctx context.Context, enumeration RolloutEnumeration) error {
	var rebuilder DerivedLookupRebuilder
	if candidate, ok := p.store.(DerivedLookupRebuilder); ok {
		rebuilder = candidate
	} else if candidate, ok := p.metricsStore.(DerivedLookupRebuilder); ok {
		rebuilder = candidate
	}
	if rebuilder == nil {
		return fmt.Errorf("ingest: the attached store cannot rebuild the derived lookup tables, which `harvest index --all` is the entry point for; where: ingest.Pipeline.rebuildIndexDerivedLookups in internal/ingest/index_selection.go; when: rebuilding the derived lookups after the source walk; impact: the derived tables were left as they were and no saved pair was indexed; fix: run `harvest index --all` with its analytics database attached")
	}
	if len(enumeration.CompletedRoots) > 0 {
		catalog, ok := p.store.(CodexRolloutCatalogStore)
		if !ok {
			return fmt.Errorf("pipeline reindex: cannot read the Codex rollout catalog; attach the analytics database and retry")
		}
		existing, err := catalog.LoadCodexRolloutCatalog(ctx)
		if err != nil {
			return fmt.Errorf("pipeline reindex: load Codex rollout catalog before reconciliation: %w", err)
		}
		enumeration.Deletes = codexCatalogDeletes(p.fs, existing, enumeration.Files, enumeration.CompletedRoots)
	}
	if err := rebuilder.RebuildDerivedLookups(ctx, enumeration); err != nil {

		return fmt.Errorf("pipeline reindex: rebuild the derived lookup tables: %w; no saved pair was indexed after the failure; repair the store and rerun `harvest index --all`", err)
	}
	return nil
}

// orderScopeReport returns the report's members selected first, then children,
// then ancestors, then context members, so a caller reads the cohort in the
// order the scan resolved it.
func orderScopeReport(members []LocatedMember) ScopeReport {
	order := []CohortRole{CohortSelected, CohortChild, CohortAncestor, CohortContext}
	ordered := make([]LocatedMember, 0, len(members))
	for _, role := range order {
		for _, member := range members {
			if member.Role == role {
				ordered = append(ordered, member)
			}
		}
	}
	return ScopeReport{Members: ordered}
}

// withinRunAge applies the run's --since window to a session start. A nil
// window includes every start. This is the age half of the run's explicit
// filters, stated once; the harness and session halves live in
// includesManagedSession. A start the caller could not read stays the caller's
// decision: the zero time is before any window, so a session with no recorded
// start is excluded whenever a window is set.
func (p *Pipeline) withinRunAge(startMs int64) bool {
	return p.config.Since == nil || !time.UnixMilli(startMs).Before(*p.config.Since)
}

// discoveredSessionStart is the start a session carries on its own, for a
// candidate whose caller has no metadata start: the created time the harness
// recorded, or the file time when it recorded none. Zero means the session
// cannot say when it started.
func discoveredSessionStart(session DiscoveredSession) int64 {
	if !session.CreatedAt.IsZero() {
		return session.CreatedAt.UnixMilli()
	}
	if !session.ModTime.IsZero() {
		return session.ModTime.UnixMilli()
	}
	return 0
}

// includesSession is the session half of the run's explicit filters: a run that
// names sessions reads only those. It is stated once, so a scope test that
// applies the session half without the harness half — as the reindex FILTER
// stage does, where the harness half was already decided upstream, by
// pairRepairTargets in Stage 1 for a repair and by the Stage 2 inventory for the
// rest — still reads the same rule as includesManagedSession.
func (p *Pipeline) includesSession(sid SessionID) bool {
	return p.config.AllowedSessionIDs == nil || p.config.AllowedSessionIDs[sid]
}

// includesManagedSession is the strict harness and session half of the run's
// explicit filters. Every database-driven inventory applies it, each with
// withinRunAge for the age half: the index inventory, which the artifact mirror
// and the dry-run plan share, the adapter refresh selection, the stored
// metrics refresh, and the stored-discovery usability check. An unnamed harness
// does not match a --harness run, because it is no evidence that the session
// belongs to that harness.
func (p *Pipeline) includesManagedSession(sid SessionID, harness Harness) bool {
	return (p.config.Harness == nil || harness == *p.config.Harness) && p.includesSession(sid)
}

// includesManagedSessionOrUnknown is includesManagedSession for a repair the
// database already selected. Such a candidate is this run's work before the
// filters are applied, and only a named harness can fail a --harness filter, so
// a harness the store could not name stays in scope instead of dropping the
// repair. Inventories do not use it: there an unnamed harness would widen what
// a --harness run reads.
func (p *Pipeline) includesManagedSessionOrUnknown(sid SessionID, harness Harness) bool {
	if harness == "" && p.config.Harness != nil {
		harness = *p.config.Harness
	}
	return p.includesManagedSession(sid, harness)
}
