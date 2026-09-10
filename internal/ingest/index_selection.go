package ingest

import (
	"context"
	"path/filepath"
	"time"
)

func (p *Pipeline) includesIndexTarget(target reindexTarget) bool {
	return p.includesManagedSession(target.session.SessionID, target.session.Harness) &&
		(p.config.Since == nil || !time.UnixMilli(target.startMs).Before(*p.config.Since))
}

// capturedInputNeedsWork decides whether a captured input still has pending
// indexer work. A current parser and an equal input hash are not enough on
// their own: the stored index must also be bound to the current publication
// metadata capture, and a harness whose strict parser can certify complete
// content must have done so. Both are settled by the ordinary index write, so
// a repair completes in one harvest and the next unchanged harvest does no
// parser work.
func (p *Pipeline) capturedInputNeedsWork(input *CapturedIndexInput) bool {
	target := p.versionTargets()[input.session.Harness]
	expected := input.expected
	return p.config.Force ||
		expected.IndexerVersion < target.IndexerVersion ||
		expected.IndexedInputHash == nil || *expected.IndexedInputHash != input.inputHash ||
		// A current metadata capture exists (revision > 0) but the index is not
		// bound to it: bytes match, publication proof does not. Only an input
		// the capture can vouch for is pending on that account.
		expected.PublicationCaptureRevision > 0 && !expected.PublicationBound && input.bindsPublication() ||
		// The capture is incomplete and this build can certify it: the strict
		// format-1 parser is the only path that produces complete content, so
		// a harness on another declared format is at its steady state instead.
		// A refusal this build already recorded for this producer is a steady
		// state too, and is excluded below.
		expected.ContentStatus != ContentCaptureComplete && p.certifiesContent(input.session.Harness) &&
			!permanentRefusalIsSettled(expected, target)
}

// bindsPublication reports whether an index written from this input may be
// bound to the current publication metadata capture. A managed transcript's
// bytes are what the capture hashed, so a file input binds. A directory tree
// is read beside a header the capture hashed without the tree, so a tree read
// from retained input has no byte proof against the capture and its index is
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

// permanentCaptureRefusal reports whether a recorded failure code is one this
// build can never clear on its own. A capture nothing refused, and a capture
// that predates content capture, are both PENDING work rather than settled:
// they have simply never been tried by a build that could certify them.
func permanentCaptureRefusal(code ContentCaptureFailureCode) bool {
	return code == ContentCaptureStrictRefused || code == ContentCaptureSourceRecordsOmitted
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
	indexer, ok := p.indexers[target.session.Harness]
	if !ok {
		return false
	}
	input, err := p.captureIndexInput(ctx, indexedMeta{session: target.session, startMs: target.startMs, outputTranscriptPath: target.transcriptPath}, indexer)
	if err != nil {
		// Incomplete capture is not current input. The shared parse path owns
		// its visible refusal/log, once per invocation, without a success stamp.
		return true
	}
	return p.capturedInputNeedsWork(input)
}

// scanPeasantSyncSessions consumes the publisher's bounded metadata inventory;
// it does not recover or reconcile files. Capture later verifies each chosen pair.
func (p *Pipeline) scanPeasantSyncSessions(ctx context.Context) []reindexTarget {
	publisher, err := NewArtifactPublisher(p.fs, string(p.config.OutputDir), ArtifactPublisherOptions{Versions: p.versionTargets()})
	if err != nil {
		p.reportMetadataRefusal(string(p.config.OutputDir), err)
		return nil
	}
	var targets []reindexTarget
	err = publisher.WalkMetadata(ctx, func(sid SessionID, path string) error {
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
