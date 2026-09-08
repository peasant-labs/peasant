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

func (p *Pipeline) capturedInputNeedsWork(input *capturedIndexInput) bool {
	target := p.versionTargets()[input.session.Harness]
	return p.config.Force || input.expected.IndexerVersion < target.IndexerVersion ||
		input.expected.IndexedInputHash == nil || *input.expected.IndexedInputHash != input.inputHash
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
