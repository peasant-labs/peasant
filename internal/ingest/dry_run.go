package ingest

import (
	"context"
	"fmt"
)

func (p *Pipeline) dryRunIndexNeedsWork(ctx context.Context, target reindexTarget) bool {
	if !p.includesIndexTarget(target) {
		return false
	}
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return false
	}
	state, err := reader.ReadIndexState(ctx, target.session.SessionID)
	if err != nil {
		p.reportMetadataRefusal(string(target.session.SessionID), err)
		return false
	}
	if state == nil || state.ArtifactHash == nil {
		p.reportDiagnostic(DiagnosticEntry{
			ErrorType:   "dry_run_unindexed",
			Location:    string(target.session.SessionID),
			Message:     fmt.Sprintf("dry-run cannot determine the index work for session %s until it is saved; no files or database rows were changed", target.session.SessionID),
			Remediation: "Run harvest without --dry-run to save the session, then retry the forecast.",
		})
		return false
	}
	if err := p.checkIndexProducer(state); err != nil {
		p.reportIndexRefusal(target.session.SessionID, err)
		return false
	}
	if p.config.Force || state.IndexedAt == nil {
		return true
	}
	return p.stateNeedsIndexWork(state, p.sessionVersionTarget(target.session))
}
