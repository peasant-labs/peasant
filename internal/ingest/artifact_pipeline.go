package ingest

import (
	"context"
	"fmt"
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
