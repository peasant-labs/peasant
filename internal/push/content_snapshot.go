package push

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/transcript"
)

// readContent requires the same retained-input proof as full export. Candidate
// rows select work; only the captured snapshot supplies publication evidence.
func (p *Pipeline) readContent(ctx context.Context, candidate ingest.PushSessionRow) (*transcript.ContentSnapshot, error) {
	reader, ok := p.store.(transcript.ContentStore)
	if !ok {
		return nil, fmt.Errorf("capture session %s for publication: the configured store cannot read coherent session content; no content was prepared; configure the production Store content reader and retry", candidate.SessionID)
	}
	snapshot, err := transcript.ReadSessionContent(ctx, reader, p.fs, p.cfg.Output.BasePath, candidate.SessionID)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.Detail == nil {
		return nil, fmt.Errorf("capture session %s for publication: no recorded local session remains; pulled transcripts cannot supply local publication input; harvest the recorded source before retrying", candidate.SessionID)
	}
	if snapshot.FullContentError != nil {
		failure := snapshot.FullContentError
		if errors.Is(failure, fs.ErrNotExist) {
			failure = fmt.Errorf("%w: %w", ErrMetadataMissing, failure)
			if remedy := p.redactedSlugRemedy(candidate); remedy != "" {
				failure = fmt.Errorf("%w\n%s", failure, remedy)
			}
		}
		return nil, failure
	}
	if snapshot.Artifact == nil || snapshot.Metrics == nil {
		return nil, fmt.Errorf("capture session %s for publication: retained artifact or metrics are unavailable; no content was prepared; complete harvest before retrying", candidate.SessionID)
	}
	detail := snapshot.Detail
	if detail.SessionID != candidate.SessionID || detail.ProjectHash != candidate.ProjectHash || detail.ModelHarness != candidate.ModelHarness ||
		string(snapshot.Artifact.Metadata.Project.Hash) != detail.ProjectHash {
		return nil, fmt.Errorf("capture session %s for publication: its recorded identity changed after selection; no content was prepared; retry the push to select its current project and harness", candidate.SessionID)
	}
	if p.runCfg.Repository != nil && !p.runCfg.Repository.Admits(detail.ProjectHash) {
		return nil, fmt.Errorf("capture session %s for publication: its current project is outside the requested repository; no content was prepared; retry within the selected repository", candidate.SessionID)
	}
	if p.runCfg.Selection != nil {
		projectPath := detail.GitWorktree
		if projectPath == "" {
			projectPath = detail.ProjectPath
		}
		if p.runCfg.Selection.Decision(snapshot.Artifact.Metadata.SessionID) != ingest.BranchMatchYes ||
			stringValue(detail.GitBranch) != stringValue(candidate.GitBranch) || stringValue(detail.GitRemote) != candidate.GitRemote || projectPath != candidate.ProjectPath {
			return nil, fmt.Errorf("capture session %s for publication: its project or branch no longer matches the prepared selection; no content was prepared; retry the push to refresh selection decisions", candidate.SessionID)
		}
	}
	if _, err := sessionorigin.Parse(detail.SessionOrigin); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
