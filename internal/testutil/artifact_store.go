package testutil

import (
	"context"
	"errors"

	"github.com/peasant-labs/peasant/internal/ingest"
)

var _ ingest.ArtifactMirrorStore = (*StubSessionStore)(nil)

// MirrorArtifacts is the pipeline's atomic persistence dependency in memory
// fixtures. Real transaction, rollback and cursor behavior is tested in Store.
func (s *StubSessionStore) MirrorArtifacts(ctx context.Context, requests []ingest.ArtifactMirrorRequest) []ingest.ArtifactMirrorResult {
	results := make([]ingest.ArtifactMirrorResult, len(requests))
	for index, request := range requests {
		if request.Artifact != nil {
			results[index].SessionID = request.Artifact.Metadata.SessionID
		}
		if err := request.Artifact.Validate(); err != nil {
			results[index].Err = err
			continue
		}
		if err := errors.Join(s.InsertErr, s.UpsertCommitsErr); err != nil {
			results[index].Err = err
			continue
		}
		meta := request.Artifact.Metadata
		session := ingest.DiscoveredSession{SessionID: meta.SessionID, Harness: meta.ModelHarness, ParentUUID: meta.ParentUUID}
		if request.Origin != nil {
			session.Origin = *request.Origin
		}
		if request.EventSeq != nil {
			session.EventSeq = *request.EventSeq
		}
		// The mirror request carries the acquisition evidence the real store
		// persists with the row. Dropping it here leaves the recorded session
		// looking as if its source was never consumed, so a later run
		// re-ingests an unchanged session.
		entry := ingest.StoreEntry{
			Metadata:              &meta,
			Session:               session,
			SourceFingerprint:     request.SourceFingerprint,
			CWDProvenance:         request.CWDProvenance,
			CommitCaptureComplete: request.CommitCaptureComplete,
			EventSeq:              request.EventSeq,
		}
		if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			results[index].Err = err
			continue
		}
		if err := s.UpsertSessionCommits(ctx, meta.SessionID, meta.Git.Commits); err != nil {
			results[index].Err = err
			continue
		}
		results[index].Mirrored = true
	}
	return results
}
