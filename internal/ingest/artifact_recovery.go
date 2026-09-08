package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// artifactMirrorError distinguishes a failed database mirror from failed file
// publication/recovery, so successful file ingestion retains its own outcome.
type artifactMirrorError struct{ error }

func (e *artifactMirrorError) Unwrap() error { return e.error }

func currentArtifactFileHash(root ArtifactRoot, path string) (*string, error) {
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("inspect pending artifact path %q: not a regular owned file; recovery was refused", path)
	}
	data, err := root.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hash := schema.ComputeTranscriptHash(data)
	return &hash, nil
}

func rollbackArtifactIntent(root ArtifactRoot, key string, intent *artifactIntent) error {
	current := make([]*string, len(intent.Files))
	for index, file := range intent.Files {
		hash, err := currentArtifactFileHash(root, file.Path)
		if err != nil {
			return err
		}
		current[index] = hash
		if intent.Phase == artifactPrepared && file.Path != intentMetadataPath(intent) && !reflect.DeepEqual(hash, file.PreviousHash) {
			return fmt.Errorf("recover prepared session %s: active file %q changed before file replacement was authorized; recovery evidence was preserved", intent.SessionID, file.Path)
		}
		if hash != nil && !reflect.DeepEqual(hash, file.PreviousHash) && !reflect.DeepEqual(hash, file.CandidateHash) {
			return fmt.Errorf("recover session %s: %q no longer matches this intent's old or candidate bytes; newer/unrelated data was preserved; inspect the conflicting publication before retrying", intent.SessionID, file.Path)
		}
		if file.PreviousHash != nil && !reflect.DeepEqual(hash, file.PreviousHash) {
			backup, err := root.ReadFile(intentFilePath(key, "old", index))
			if err != nil || schema.ComputeTranscriptHash(backup) != *file.PreviousHash {
				return fmt.Errorf("restore session %s: original backup for %q is missing or invalid (%v); recovery evidence was retained without further replacement", intent.SessionID, file.Path, err)
			}
		}
	}
	if intent.Phase != artifactRollingBack {
		if err := updateArtifactIntentPhase(root, key, intent, artifactRollingBack); err != nil {
			return err
		}
	}
	order := make([]int, 0, len(intent.Files))
	for index, file := range intent.Files {
		if !strings.HasSuffix(file.Path, defaults.MetadataSuffix) {
			order = append(order, index)
		}
	}
	for index, file := range intent.Files {
		if strings.HasSuffix(file.Path, defaults.MetadataSuffix) {
			order = append(order, index)
		}
	}
	for _, index := range order {
		file := intent.Files[index]
		if reflect.DeepEqual(current[index], file.PreviousHash) {
			continue
		}
		if file.PreviousHash == nil {
			if err := root.Remove(file.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		} else {
			backup, err := root.ReadFile(intentFilePath(key, "old", index))
			if err != nil || schema.ComputeTranscriptHash(backup) != *file.PreviousHash {
				return fmt.Errorf("restore original artifact %q: backup changed during recovery (%v)", file.Path, err)
			}
			restore := filepath.Join(artifactTransactionPath(key), fmt.Sprintf("restore-%04d", index))
			if err := removeArtifactTemporary(root, restore); err != nil {
				return err
			}
			if err := durableArtifactWrite(root, restore, backup); err != nil {
				return err
			}
			if err := syncArtifactParents(root, filepath.Dir(file.Path)); err != nil {
				return err
			}
			if err := root.Rename(restore, file.Path); err != nil {
				return err
			}
		}
		if err := root.SyncDir(filepath.Dir(file.Path)); err != nil {
			return err
		}
	}
	return cleanupArtifactIntent(root, key, intent)
}

func removeArtifactTemporary(root ArtifactRoot, path string) error {
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("clean publication state %q: unexpected file type; preserved rather than deleting unrelated content", path)
	}
	return root.Remove(path)
}

func cleanupArtifactIntent(root ArtifactRoot, key string, intent *artifactIntent) error {
	journal := artifactTransactionPath(key)
	nonce, err := randomHex(defaults.TempSuffixLen)
	if err != nil {
		return err
	}
	retired := filepath.Join(filepath.Dir(journal), ".retired-"+key+"-"+nonce)
	// Retirement is atomic and durable before recovery evidence is removed.
	// A cleanup interruption cannot leave a canonical journal without intent.
	if err := root.Rename(journal, retired); err != nil {
		return err
	}
	if err := root.SyncDir(filepath.Dir(journal)); err != nil {
		return err
	}
	return cleanupArtifactIntentAt(root, retired, intent)
}

func cleanupArtifactIntentAt(root ArtifactRoot, journal string, intent *artifactIntent) error {
	allowed := map[string]bool{"old": true, "new": true, "intent.json": true, "intent.next": true, "derived.json": true}
	for index := range intent.Files {
		allowed[fmt.Sprintf("restore-%04d", index)] = true
	}
	entries, err := root.ReadDir(journal)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("clean publication state for %s: unrecognized file %q was preserved; inspect the private transaction directory", intent.SessionID, entry.Name())
		}
	}
	for _, group := range []string{"old", "new"} {
		groupPath := filepath.Join(journal, group)
		entries, err := root.ReadDir(groupPath)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for _, entry := range entries {
			valid := false
			for index := range intent.Files {
				if entry.Name() == fmt.Sprintf("%04d", index) {
					valid = true
					break
				}
			}
			if !valid || entry.IsDir() {
				return fmt.Errorf("clean publication state: unrecognized backup/candidate %q was preserved", entry.Name())
			}
			if err := removeArtifactTemporary(root, filepath.Join(groupPath, entry.Name())); err != nil {
				return err
			}
		}
		if err := root.Remove(groupPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for index := range intent.Files {
		if err := removeArtifactTemporary(root, filepath.Join(journal, fmt.Sprintf("restore-%04d", index))); err != nil {
			return err
		}
	}
	if err := removeArtifactTemporary(root, filepath.Join(journal, "derived.json")); err != nil {
		return err
	}
	if err := removeArtifactTemporary(root, filepath.Join(journal, "intent.next")); err != nil {
		return err
	}
	if err := root.SyncDir(journal); err != nil {
		return err
	}
	if err := removeArtifactTemporary(root, filepath.Join(journal, "intent.json")); err != nil {
		return err
	}
	if err := root.SyncDir(journal); err != nil {
		return err
	}
	if err := root.Remove(journal); err != nil {
		return err
	}
	return root.SyncDir(filepath.Dir(journal))
}

func committedArtifactIntent(root ArtifactRoot, intent *artifactIntent) (*ManagedArtifact, bool, error) {
	if intent.Phase != artifactFilesChanging {
		return nil, false, nil
	}
	data, err := root.ReadFile(intentMetadataPath(intent))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	identity, identityErr := artifactMetadataIdentity(data)
	if identityErr != nil || identity != intent.MetadataIdentity {
		return nil, false, nil
	}
	artifact, err := readArtifactPair(root, intentMetadataPath(intent), intent.SessionID)
	if err != nil {
		return nil, true, err
	}
	if artifact.ArtifactHash != intent.ArtifactHash {
		return nil, true, fmt.Errorf("recover committed artifact %s: semantic identity is inconsistent; no files were changed", intent.SessionID)
	}
	for _, file := range intent.Files {
		if file.Path == intentMetadataPath(intent) {
			continue
		}
		current, err := currentArtifactFileHash(root, file.Path)
		if err != nil {
			return nil, true, err
		}
		if !reflect.DeepEqual(current, file.CandidateHash) {
			return nil, true, fmt.Errorf("recover committed artifact %s: owned file %q differs from the committed candidate; no rollback or mirror was attempted", intent.SessionID, file.Path)
		}
	}
	return artifact, true, nil
}

func (p *ArtifactPublisher) reconcileArtifactIntent(ctx context.Context, root ArtifactRoot, key string, intent *artifactIntent) (*ManagedArtifact, error) {
	artifact, committed, err := committedArtifactIntent(root, intent)
	if err != nil {
		return nil, err
	}
	if !committed {
		if err := rollbackArtifactIntent(root, key, intent); err != nil {
			return nil, err
		}
		return nil, nil
	}
	// A process may have exited after metadata rename but before its directory
	// sync. Confirm visible committed directory entries before mirroring forward.
	directories := make(map[string]bool)
	for _, file := range intent.Files {
		directories[filepath.Dir(file.Path)] = true
	}
	for directory := range directories {
		if err := root.SyncDir(directory); err != nil {
			return nil, err
		}
	}
	if intent.NeedsDatabase && p.mirror == nil {
		return nil, &artifactMirrorError{fmt.Errorf("recover committed session %s: database reconciliation is pending; file-only mode did not open a database or discard the acquired source cursor; run a database-backed harvest to finish", intent.SessionID)}
	}
	if p.mirror != nil {
		results := p.mirror.MirrorArtifacts(ctx, []ArtifactMirrorRequest{{Artifact: artifact, EventSeq: intent.EventSeq, Origin: intent.Origin, CWDProvenance: intent.CWDProvenance, SourceFingerprint: intent.SourceFingerprint, CommitCaptureComplete: intent.CommitCaptureComplete}})
		if len(results) != 1 || results[0].SessionID != intent.SessionID {
			return nil, &artifactMirrorError{fmt.Errorf("mirror committed session %s: store returned an invalid per-session outcome; files and recovery evidence were retained", intent.SessionID)}
		}
		if results[0].Err != nil || !results[0].Mirrored {
			return nil, &artifactMirrorError{fmt.Errorf("mirror committed session %s: %v; committed files were not rolled back; restore database compatibility/access and retry harvest", intent.SessionID, results[0].Err)}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(artifact.MetadataJSON, &fields); err != nil {
			return nil, err
		}
		fields["derivedAt"], _ = json.Marshal(p.now().UnixMilli())
		derived, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		checked, err := NewManagedArtifact(derived, artifact.Transcript)
		if err != nil || checked.ArtifactHash != artifact.ArtifactHash {
			return nil, fmt.Errorf("refresh derived metadata for session %s: cache would change committed input semantics (%v); recovery state was retained", intent.SessionID, err)
		}
		path := filepath.Join(artifactTransactionPath(key), "derived.json")
		if err := removeArtifactTemporary(root, path); err != nil {
			return nil, err
		}
		if err := durableArtifactWrite(root, path, derived); err != nil {
			return nil, err
		}
		if err := root.Rename(path, intentMetadataPath(intent)); err != nil {
			return nil, err
		}
		if err := root.SyncDir(intent.Directory); err != nil {
			return nil, err
		}
		artifact = checked
	}
	if err := cleanupArtifactIntent(root, key, intent); err != nil {
		return nil, err
	}
	return artifact, nil
}

// Reconcile finishes this committed publication only; it never substitutes a
// newer session generation for the one the worker handed to the drain loop.
func (p *ArtifactPublisher) Reconcile(ctx context.Context, expected *ManagedArtifact) (*ManagedArtifact, error) {
	if err := expected.Validate(); err != nil {
		return nil, err
	}
	key := artifactKey(expected.Metadata.SessionID)
	root, lock, err := p.lockedRoot(ctx, key, ArtifactLockWrite, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	defer lock.Close()
	intent, err := readArtifactIntent(root, key)
	if err != nil {
		return nil, err
	}
	if intent.ArtifactHash != expected.ArtifactHash {
		return nil, fmt.Errorf("reconcile session %s: pending artifact differs from this worker's committed input; newer state was preserved; retry harvest", expected.Metadata.SessionID)
	}
	return p.reconcileArtifactIntent(ctx, root, key, intent)
}

// Recover repairs one session under the same OS ownership used for publication.
// A committed candidate reconciles forward; an uncommitted one restores only
// its previous owned files. No source adapter is called during recovery.
func (p *ArtifactPublisher) Recover(ctx context.Context, sid SessionID) error {
	key := artifactKey(sid)
	root, lock, err := p.lockedRoot(ctx, key, ArtifactLockWrite, false)
	if err != nil {
		return err
	}
	defer root.Close()
	defer lock.Close()
	intent, err := readArtifactIntent(root, key)
	if err != nil {
		return err
	}
	_, err = p.reconcileArtifactIntent(ctx, root, key, intent)
	return err
}

// PendingSessions returns a metadata-only inventory, not transcript snapshots.
// Callers can recover parents before children and apply their explicit scope.
func (p *ArtifactPublisher) PendingSessions() ([]SessionID, error) {
	root, err := p.fs.OpenArtifactRoot(p.output)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := root.ReadDir(filepath.Join(managedArtifactStateDir, "transactions"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []SessionID
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() || !validArtifactHash(entry.Name()) {
			continue
		}
		intent, err := readArtifactIntent(root, entry.Name())
		if err != nil {
			failures = append(failures, err)
			continue
		}
		ids = append(ids, intent.SessionID)
	}
	slices.Sort(ids)
	return ids, errors.Join(failures...)
}
