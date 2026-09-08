package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

// Inspect only recorded recovery/input evidence. Planning does not establish
// new ownership, read whole transcripts or repair a pending publication.
func (p *Pipeline) inspectDryRunArtifacts(ctx context.Context) {
	publisher, err := p.artifactPublisher(nil)
	if err != nil {
		p.reportDiagnostic(artifactRecoveryDiagnostic(string(p.config.OutputDir), err))
		return
	}
	root, err := publisher.fs.OpenArtifactRoot(publisher.output)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		p.reportDiagnostic(artifactRecoveryDiagnostic(string(p.config.OutputDir), err))
		return
	}
	defer root.Close()
	err = walkArtifactDirectory(ctx, root, filepath.Join(managedArtifactStateDir, "transactions"), func(entry fs.DirEntry) error {
		if !entry.IsDir() || !validArtifactHash(entry.Name()) {
			return nil
		}
		intent, err := readArtifactIntent(root, entry.Name())
		if err != nil {
			return err
		}
		if !p.includesManagedSession(intent.SessionID, intent.Harness) {
			return nil
		}
		if p.config.Since != nil {
			meta, err := intentCandidateMetadata(root, entry.Name(), intent)
			if err != nil {
				return err
			}
			if time.UnixMilli(meta.Timestamp.Start).Before(*p.config.Since) {
				return nil
			}
		}
		return fmt.Errorf("dry-run found pending publication for session %s; its current work cannot be determined until recovery; files were preserved; run harvest without --dry-run to recover and then retry planning", intent.SessionID)
	})
	if err != nil {
		p.reportDiagnostic(artifactRecoveryDiagnostic(string(p.config.OutputDir), err))
	}
}

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
		p.reportDiagnostic(artifactRecoveryDiagnostic(string(target.session.SessionID), fmt.Errorf("dry-run requires retained metadata reconciliation before index work can be determined; no files or database rows were changed; run harvest without --dry-run and retry")))
		return false
	}
	if err := p.checkIndexProducer(state); err != nil {
		p.reportIndexRefusal(target.session.SessionID, err)
		return false
	}
	return p.config.Force || state.IndexerVersion < p.versionTargets()[state.Harness].IndexerVersion || state.IndexedInputHash == nil || state.IndexedAt == nil
}
