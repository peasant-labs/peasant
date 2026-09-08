package ingest

import (
	"context"
	"fmt"
	"time"
)

func (p *Pipeline) artifactPublisher(lane *storeWriteLane) (*ArtifactPublisher, error) {
	var mirror ArtifactMirrorStore
	if p.store != nil {
		var ok bool
		mirror, ok = p.store.(ArtifactMirrorStore)
		if !ok {
			return nil, fmt.Errorf("prepare managed artifact publication: configured store has no transactional artifact mirror; no artifact was replaced; use the production store or provide its atomic mirror capability")
		}
	} else if available, ok := p.metricsStore.(ArtifactMirrorStore); ok {
		mirror = available
	}
	if mirror != nil && lane != nil {
		mirror = &serializedArtifactMirror{pipeline: p, lane: lane, store: mirror}
	}
	return NewArtifactPublisher(p.fs, string(p.config.OutputDir), ArtifactPublisherOptions{Versions: p.versionTargets(), Mirror: mirror})
}

type serializedArtifactMirror struct {
	pipeline *Pipeline
	lane     *storeWriteLane
	store    ArtifactMirrorStore
}

var _ ArtifactMirrorStore = (*serializedArtifactMirror)(nil)

func (s *serializedArtifactMirror) MirrorArtifacts(ctx context.Context, requests []ArtifactMirrorRequest) []ArtifactMirrorResult {
	var results []ArtifactMirrorResult
	s.pipeline.runStoreWrite(s.lane, func() { results = s.store.MirrorArtifacts(ctx, requests) })
	return results
}

func (s *serializedArtifactMirror) ReadIndexState(ctx context.Context, sid SessionID) (*SessionIndexState, error) {
	reader, ok := s.store.(SessionIndexStateReader)
	if !ok {
		return nil, fmt.Errorf("inspect retained session %s: configured mirror has no index-state reader; no artifact was changed; configure the production store", sid)
	}
	return reader.ReadIndexState(ctx, sid)
}

func (p *Pipeline) includesManagedArtifact(meta *UnifiedMetadata) bool {
	return p.includesManagedSession(meta.SessionID, meta.ModelHarness) &&
		(p.config.Since == nil || !time.UnixMilli(meta.Timestamp.Start).Before(*p.config.Since))
}

func (p *Pipeline) includesManagedSession(sid SessionID, harness Harness) bool {
	return (p.config.Harness == nil || harness == *p.config.Harness) &&
		(p.config.AllowedSessionIDs == nil || p.config.AllowedSessionIDs[sid])
}

// reconcileManagedArtifacts runs before native/stored work selection. It does
// not apply saved discovery selection to retained data, invoke adapters, or
// decide indexing eligibility from an artifact digest.
func (p *Pipeline) reconcileManagedArtifacts(ctx context.Context) {
	publisher, err := p.artifactPublisher(nil)
	if err != nil {
		p.reportDiagnostic(artifactRecoveryDiagnostic(string(p.config.OutputDir), err))
		return
	}
	changed, err := publisher.recoverPending(ctx, p.includesManagedSession, p.config.Since)
	p.reconciledArtifacts = append(p.reconciledArtifacts, changed...)
	if err != nil {
		p.reportDiagnostic(artifactRecoveryDiagnostic(string(p.config.OutputDir), err))
	}
	if publisher.mirror != nil {
		err = publisher.WalkMetadata(ctx, func(sid SessionID, path string) error {
			_, changed, err := publisher.ReconcileStored(ctx, sid, path, p.includesManagedArtifact)
			if err != nil {
				p.reportDiagnostic(artifactRecoveryDiagnostic(path, err))
			} else if changed {
				p.reconciledArtifacts = append(p.reconciledArtifacts, sid)
			}
			return nil // An independent valid session can still make progress.
		})
		if err != nil {
			p.reportDiagnostic(artifactRecoveryDiagnostic(string(p.config.OutputDir), err))
		}
	}
	var includeLifecycle func(SessionID, Harness) bool
	if p.config.Harness != nil || p.config.AllowedSessionIDs != nil || p.config.Since != nil {
		includeLifecycle = func(sid SessionID, harness Harness) bool {
			// Lifecycle records without available session age remain untouched
			// by age-scoped cleanup; an unscoped harvest can collect them later.
			return p.config.Since == nil && p.includesManagedSession(sid, harness)
		}
	}
	for _, diagnostic := range publisher.Cleanup(ctx, includeLifecycle) {
		p.reportDiagnostic(diagnostic)
	}
}
