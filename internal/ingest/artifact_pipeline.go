package ingest

import (
	"context"
	"errors"
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

// reportPendingRecoveryFailure reports what recoverPending could not finish.
//
// The failures are joined across independent intents, so each leaf is reported
// on its own. A leaf that is a stored-metadata refusal goes to the refusal
// funnel against ITS session, where it collapses with the same refusal from
// the selection and carries the remedy that lifts it; anything else stays an
// artifact-recovery diagnostic against the output directory.
func (p *Pipeline) reportPendingRecoveryFailure(err error) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, leaf := range joined.Unwrap() {
			p.reportPendingRecoveryFailure(leaf)
		}
		return
	}
	// Report the REFUSAL, not the operation that met it. The selection reports
	// the same refusal for the same session, and diagnostics collapse by
	// whole-value equality, so the wrapper this path adds ("mirror committed
	// session ...") would be one more spelling of one cause. The refusal's own
	// sentence already says the session's artifacts and index were not
	// changed, and the remedy is the same either way: upgrade.
	//
	// The cause comes from the same closed list that decides whether this IS a
	// compatibility refusal, so a type added to that list cannot be recognised
	// here and then reported wrapped anyway.
	var sessionErr *artifactSessionError
	if cause, ok := metadataCompatibilityCause(err); ok && errors.As(err, &sessionErr) {
		p.reportMetadataRefusal(string(sessionErr.SessionID), cause)
		return
	}
	p.reportDiagnostic(artifactRecoveryDiagnostic(string(p.config.OutputDir), err))
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
		p.reportPendingRecoveryFailure(err)
	}
	if publisher.mirror != nil {
		err = publisher.WalkMetadata(ctx, func(sid SessionID, path string) error {
			// Decide the refusal BEFORE any intent is staged. ReconcileStored
			// stages a publication intent and only then asks the mirror, so a
			// session whose stored schema this build cannot read used to leave
			// a pending intent behind: every later harvest then met that intent
			// first and reported the same refusal twice more, in words that
			// send the user to delete recovery evidence or to re-run the very
			// harvest that cannot reconcile it. Refusing here leaves nothing
			// behind, so the second harvest costs exactly what the first did.
			//
			// Only a refusal this build cannot lift is decided here. A check that
			// merely failed - a transient lookup fault the run goes on to recover
			// from - is left to the paths that already own it, so an extra reader
			// cannot turn a recovered fault into a warning the user must read.
			if err := p.checkStoredMetadataVersion(ctx, sid); isMetadataCompatibilityError(err) {
				p.reportMetadataRefusal(string(sid), err)
				return nil
			}
			_, changed, err := publisher.ReconcileStored(ctx, sid, path, p.includesManagedArtifact)
			var schemaErr *UnsupportedMetadataVersionError
			if errors.As(err, &schemaErr) {
				// A stored schema newer than this build is the same refusal the
				// index selection reports; one diagnostic with the schema remedy.
				p.reportMetadataRefusal(string(sid), schemaErr)
			} else if err != nil {
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
