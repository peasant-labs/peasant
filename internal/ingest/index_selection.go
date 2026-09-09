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
		// bound to it: bytes match, publication proof does not.
		expected.PublicationCaptureRevision > 0 && !expected.PublicationBound ||
		// The capture is incomplete and this build can certify it: the strict
		// format-1 parser is the only path that produces complete content, so
		// a harness on another declared format is at its steady state instead.
		expected.ContentStatus != ContentCaptureComplete && p.certifiesContent(input.session.Harness)
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
