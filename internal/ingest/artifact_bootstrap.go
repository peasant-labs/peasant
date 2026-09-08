package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/schema"
)

// ReconcileStored bootstraps/reconciles an already committed retained pair.
// A matching SQL artifact identity leaves metadata bytes and DerivedAt alone.
// It never invokes an adapter, stamps a parser revision, or advances a cursor.
// The returned changed flag is a work-selection candidate, not index success.
func (p *ArtifactPublisher) ReconcileStored(ctx context.Context, sid SessionID, metadataPath string, include func(*UnifiedMetadata) bool) (*ManagedArtifact, bool, error) {
	path, err := p.relativeMetadataPath(metadataPath, sid)
	if err != nil {
		return nil, false, err
	}
	reader, ok := p.mirror.(SessionIndexStateReader)
	if !ok {
		return nil, false, fmt.Errorf("reconcile retained session %s: database mirror cannot read current artifact identity; no retained file was changed; configure the production store's index-state reader", sid)
	}
	// Scope is checked before creating coordination files for excluded sessions.
	root, err := p.fs.OpenArtifactRoot(p.output)
	if err != nil {
		return nil, false, err
	}
	data, err := root.ReadFile(path)
	_ = root.Close()
	if err != nil {
		return nil, false, err
	}
	meta, err := decodeManagedMetadata(data, path)
	if err != nil {
		return nil, false, err
	}
	if include != nil && !include(meta) {
		return nil, false, nil
	}
	key := artifactKey(sid)
	root, lock, err := p.lockedRoot(ctx, key, ArtifactLockWrite, true)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	defer lock.Close()
	if err := refusePendingArtifact(root, key); err != nil {
		return nil, false, err
	}
	artifact, err := readArtifactPair(root, path, sid)
	if err != nil {
		return nil, false, err
	}
	if include != nil && !include(&artifact.Metadata) {
		return nil, false, nil
	}
	state, err := reader.ReadIndexState(ctx, sid)
	if err != nil {
		return nil, false, err
	}
	if state != nil && state.SessionID == sid && state.Harness == artifact.Metadata.ModelHarness && state.ArtifactHash != nil && *state.ArtifactHash == artifact.ArtifactHash {
		return artifact, false, nil
	}
	identity, err := artifactMetadataIdentity(artifact.MetadataJSON)
	if err != nil {
		return nil, false, err
	}
	metadataHash := schema.ComputeTranscriptHash(artifact.MetadataJSON)
	intent := &artifactIntent{
		Version: artifactTransactionVersion, Phase: artifactPrepared,
		SessionID: sid, Harness: artifact.Metadata.ModelHarness, Directory: filepath.Dir(path),
		ArtifactHash: artifact.ArtifactHash, MetadataIdentity: identity, NeedsDatabase: true,
		Files: []artifactIntentFile{{Path: path, PreviousHash: &metadataHash, CandidateHash: &metadataHash}},
	}
	// The pair is already committed. This metadata-only intent reuses the same
	// crash-recovery protocol for the DB/cache steps without copying transcript.
	if err := stageArtifactIntent(root, intent, map[string][]byte{path: artifact.MetadataJSON}); err != nil {
		return nil, false, err
	}
	if err := updateArtifactIntentPhase(root, key, intent, artifactFilesChanging); err != nil {
		return nil, false, err
	}
	reconciled, err := p.reconcileArtifactIntent(ctx, root, key, intent)
	return reconciled, err == nil && reconciled != nil, err
}

func intentCandidateMetadata(root ArtifactRoot, key string, intent *artifactIntent) (*UnifiedMetadata, error) {
	for index, file := range intent.Files {
		if file.Path != intentMetadataPath(intent) {
			continue
		}
		data, err := root.ReadFile(intentFilePath(key, "new", index))
		if errors.Is(err, fs.ErrNotExist) {
			data, err = root.ReadFile(file.Path)
		}
		if err != nil {
			return nil, err
		}
		identity, err := artifactMetadataIdentity(data)
		if err != nil || identity != intent.MetadataIdentity {
			return nil, fmt.Errorf("inspect pending session %s for recovery scope: candidate metadata is unavailable or inconsistent; recovery evidence was preserved", intent.SessionID)
		}
		meta, err := decodeManagedMetadata(data, file.Path)
		if err != nil {
			return nil, err
		}
		if meta.SessionID != intent.SessionID || meta.ModelHarness != intent.Harness {
			return nil, fmt.Errorf("inspect pending session %s: candidate identity differs from its intent; no recovery was attempted", intent.SessionID)
		}
		return meta, nil
	}
	return nil, fmt.Errorf("inspect pending session %s: candidate metadata is missing", intent.SessionID)
}

// recoverPending visits independent pending intents in bounded pages. A bad
// session reports its own failure without preventing another session's recovery.
// Flat parent publications finish before nested child publications.
func (p *ArtifactPublisher) recoverPending(ctx context.Context, include func(SessionID, Harness) bool, since *time.Time) ([]SessionID, error) {
	root, err := p.fs.OpenArtifactRoot(p.output)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var changed []SessionID
	var failures error
	for pass := 0; pass < 2; pass++ {
		failures = errors.Join(failures, walkArtifactDirectory(ctx, root, filepath.Join(managedArtifactStateDir, "transactions"), func(entry fs.DirEntry) error {
			if !entry.IsDir() || !validArtifactHash(entry.Name()) {
				return nil
			}
			key := entry.Name()
			lock, err := root.Lock(ctx, artifactLockPath(key), ArtifactLockWrite, false)
			if err != nil {
				return err
			}
			defer lock.Close()
			intent, err := readArtifactIntent(root, key)
			if errors.Is(err, fs.ErrNotExist) {
				if _, statErr := root.Lstat(artifactTransactionPath(key)); errors.Is(statErr, fs.ErrNotExist) {
					return nil
				}
			}
			if err != nil {
				if pass == 0 {
					return err
				}
				return nil
			}
			nested := strings.Count(filepath.ToSlash(intent.Directory), "/") == 3
			if nested != (pass == 1) {
				return nil
			}
			if include != nil && !include(intent.SessionID, intent.Harness) {
				return nil
			}
			if since != nil {
				meta, err := intentCandidateMetadata(root, key, intent)
				if err != nil {
					return err
				}
				if time.UnixMilli(meta.Timestamp.Start).Before(*since) {
					return nil
				}
			}
			artifact, err := p.reconcileArtifactIntent(ctx, root, key, intent)
			if err == nil && artifact != nil {
				changed = append(changed, intent.SessionID)
			}
			return err
		}))
	}
	return changed, failures
}
