package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// recoverRetainedMetadata restores recorded context around intact managed
// input. It does not execute an adapter or acquire new native input.
func (p *Pipeline) recoverRetainedMetadata(ctx context.Context, session DiscoveredSession, transcriptPath string) (*ManagedArtifact, error) {
	if p.config.DryRun || !p.includesManagedSession(session.SessionID, session.Harness) {
		return nil, nil
	}
	if _, ok := p.indexers[session.Harness]; !ok {
		return nil, nil
	}
	reader, ok := p.metricsStore.(StoredMetadataReader)
	if !ok {
		return nil, fmt.Errorf("recover retained session %s: store cannot read recorded metadata; its transcript and index were preserved; configure the production store and retry harvest", session.SessionID)
	}
	metadataPath := filepath.Join(filepath.Dir(transcriptPath), string(session.SessionID)+defaults.MetadataSuffix)
	// Check age scope before ownership creates a coordination file. Read again
	// after observation so the candidate cannot mix old SQL context with files
	// that another publisher replaced before the observation.
	data, err := reader.ReadStoredMetadata(ctx, session.SessionID)
	if err != nil || data == nil {
		return nil, err
	}
	meta, err := decodeManagedMetadata(data, metadataPath)
	if err != nil || !p.includesManagedArtifact(meta) {
		return nil, err
	}
	if err := p.checkStoredRewriteVersion(ctx, session.SessionID, session.Harness); err != nil {
		return nil, err
	}
	publisher, err := p.artifactPublisher(nil)
	if err != nil {
		return nil, err
	}
	observation, err := publisher.Observe(ctx, session, metadataPath)
	if err != nil {
		return nil, err
	}
	data, err = reader.ReadStoredMetadata(ctx, session.SessionID)
	if err != nil || data == nil {
		return nil, err
	}
	root, err := publisher.fs.OpenArtifactRoot(publisher.output)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	path, err := publisher.relativeMetadataPath(metadataPath, session.SessionID)
	if err != nil {
		return nil, err
	}
	if current, readErr := root.ReadFile(path); readErr == nil {
		if _, err := decodeManagedMetadata(current, metadataPath); err == nil || isMetadataCompatibilityError(err) {
			return nil, fmt.Errorf("recover retained session %s: metadata is now readable or incompatible (%v); current files were preserved; retry harvest with compatible metadata", session.SessionID, err)
		}
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return nil, managedInputIOError(metadataPath, readErr)
	}
	transcriptRelative := filepath.Join(filepath.Dir(path), filepath.Base(transcriptPath))
	transcript, err := root.ReadFile(transcriptRelative)
	if err != nil {
		return nil, managedInputIOError(transcriptPath, err)
	}
	observed := false
	transcriptHash := schema.ComputeTranscriptHash(transcript)
	for _, file := range observation.files {
		if file.Path == transcriptRelative && file.Hash != nil && *file.Hash == transcriptHash {
			observed = true
		}
	}
	if !observed {
		return nil, &StaleIndexWorkError{SessionID: session.SessionID}
	}
	if session.Harness == HarnessOpenCode {
		origin, err := recognizeManagedOpenCodeProjection(transcript, session.SessionID)
		if err != nil {
			return nil, err
		}
		if origin != TranscriptOriginOpenCodeLegacySQLite && origin != TranscriptOriginOpenCodeCurrentSQLite {
			return nil, fmt.Errorf("recover retained OpenCode session %s: transcript is not a validated managed SQLite envelope; files and index were preserved; restore the managed envelope or harvest its native source", session.SessionID)
		}
	} else if session.SourceFormat != SourceFormatJSONL {
		return nil, fmt.Errorf("recover retained session %s: source format %s is not supported for managed JSONL recovery; files and index were preserved; restore compatible managed input or harvest its native source", session.SessionID, session.SourceFormat)
	} else if err := validateRetainedJSONL(ctx, transcript); err != nil {
		return nil, fmt.Errorf("recover retained session %s: %w; files and index were preserved; restore complete supported JSONL and retry harvest", session.SessionID, err)
	}
	artifact, err := NewManagedArtifact(data, transcript)
	if err != nil {
		return nil, err
	}
	parent := ""
	if artifact.Metadata.ParentUUID != nil {
		parent = string(*artifact.Metadata.ParentUUID)
	}
	if artifact.Metadata.Source.Format != session.SourceFormat || artifact.Metadata.Source.FilePath != session.SourcePath.String() ||
		SessionDir("", string(artifact.Metadata.HostSlug), string(session.SessionID), parent) != observation.directory {
		return nil, fmt.Errorf("recover retained session %s: recorded source or host/parent differs from the observed transcript locator; files were preserved; reconcile the recorded session context before retrying", session.SessionID)
	}
	if !p.includesManagedArtifact(&artifact.Metadata) {
		return nil, nil
	}
	stateReader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return nil, fmt.Errorf("recover retained session %s: store cannot read index producer compatibility; no files changed; configure the production store and retry", session.SessionID)
	}
	state, err := stateReader.ReadIndexState(ctx, session.SessionID)
	if err != nil || state == nil {
		return nil, err
	}
	if state.SessionID != artifact.Metadata.SessionID || state.Harness != artifact.Metadata.ModelHarness {
		return nil, &StaleIndexWorkError{SessionID: session.SessionID}
	}
	if err := p.checkIndexProducer(state); err != nil {
		return nil, err
	}
	committed, err := publisher.Publish(ctx, ArtifactPublication{Artifact: artifact, Observation: observation})
	if err != nil {
		return nil, err
	}
	return publisher.Reconcile(ctx, committed)
}
