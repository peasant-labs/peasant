package ingest

import (
	"context"
	"path/filepath"
	"time"
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
	ids, err := lister.ListSessionsNeedingRepair(ctx, p.indexerTargets())
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
