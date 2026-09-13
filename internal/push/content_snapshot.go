package push

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// readPublicationInput captures database-authoritative content and verifies
// that selection still refers to this captured session. Eligibility remains an
// explicit ValidatePublicationInput call at the publication boundary.
func (p *Pipeline) readPublicationInput(ctx context.Context, candidate ingest.PushSessionRow) (ingest.PublicationInputBundle, error) {
	input, err := LoadPublicationInput(ctx, p.store, candidate.SessionID)
	if err != nil || input.Readiness != ingest.PublicationReady {
		return input, err
	}
	meta := input.Metadata
	if string(meta.SessionID) != candidate.SessionID || string(meta.Project.Hash) != candidate.ProjectHash ||
		string(meta.ModelHarness) != candidate.ModelHarness {
		return input, fmt.Errorf("capture session %s for publication: its recorded identity changed after selection; no content was prepared; retry the push to select its current project and harness", candidate.SessionID)
	}
	if p.runCfg.Repository != nil && !p.runCfg.Repository.Admits(string(meta.Project.Hash)) {
		return input, fmt.Errorf("capture session %s for publication: its current project is outside the requested repository; no content was prepared; retry within the selected repository", candidate.SessionID)
	}
	if p.runCfg.Selection != nil {
		if p.runCfg.Selection.Decision(meta.SessionID) != ingest.BranchMatchYes ||
			stringValue(meta.Git.Branch) != stringValue(candidate.GitBranch) ||
			ingest.NormalizeRemoteForMatch(stringValue(meta.Git.Remote)) != ingest.NormalizeRemoteForMatch(candidate.GitRemote) ||
			input.ProjectPath != candidate.ProjectPath {
			return input, fmt.Errorf("capture session %s for publication: its project or branch no longer matches the prepared selection; no content was prepared; retry the push to refresh selection decisions", candidate.SessionID)
		}
	}
	return input, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
