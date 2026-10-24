package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// installJournal spans the filesystem/index boundary, not a remote operation.
// Paths are derived from the requested transcript; only the random staging name
// is persisted. No account-private collective metadata belongs here.
type installJournal struct {
	Version        int              `json:"version"`
	Stage          string           `json:"stage"`
	Digest         string           `json:"digest"`
	Commit         store.PullCommit `json:"commit"`
	ReplaceInvalid bool             `json:"replaceInvalid,omitempty"`
	MetadataOnly   bool             `json:"metadataOnly,omitempty"`
}

const installJournalVersion = 1
const stageOwnerFilename = ".pull-stage-owner"

func lockContentInstall(ctx context.Context, root string, id schema.TranscriptID) (func() error, error) {
	locker, err := store.NewFileSessionLocker(filepath.Join(root, ".pull-locks"))
	if err != nil {
		return nil, err
	}
	lockID, err := schema.NewSessionID(id.String())
	if err != nil {
		return nil, err
	}
	return locker.LockExclusive(ctx, lockID)
}

func syncContentPath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	flags := os.O_RDWR
	if info.IsDir() {
		// Windows does not expose a portable directory flush through os.File.
		// File data is flushed; rename/restart guarantees still apply there.
		if runtime.GOOS == "windows" {
			return nil
		}
		flags = os.O_RDONLY
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func (p *Pipeline) syncInstallMove(root, final string) error {
	return errors.Join(p.syncInstall(root), p.syncInstall(filepath.Dir(final)))
}

// Flush entries beneath the established data anchor, including directories lock
// setup may have created. Flush every parent in that bounded namespace, whether
// this process or a peer created it; no racy creation/ownership census is needed.
// Unchanged ancestors above the Store's data anchor require traversal only.
func (p *Pipeline) syncInstallAncestors(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	configuredRoot, err := filepath.Abs(p.pullsRoot)
	if err != nil {
		return err
	}
	anchor := filepath.Dir(configuredRoot)
	rel, err := filepath.Rel(anchor, abs)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("pull installation: creation barriers must remain beneath the established data anchor")
	}
	for {
		if err := p.syncInstall(abs); err != nil {
			return err
		}
		if abs == anchor {
			return nil
		}
		abs = filepath.Dir(abs)
	}
}

func (p *Pipeline) installRoot(host string, id schema.TranscriptID) string {
	return filepath.Join(p.pullsRoot, host, ".install-"+id.String())
}

func installError(err error) error {
	return fmt.Errorf("peasant village pull: content/index installation interrupted: %w; complete published files and any previous backup are retained; retry the pull (or --force) to validate files and repair the index before cleanup", err)
}

// updateMetadata publishes a changed metadata snapshot without transferring blob
// bytes. The existing manifest/annotation provenance remains unchanged; the
// derived row is projected through the same recoverable install boundary.
func (p *Pipeline) updateMetadata(ctx context.Context, host string, id schema.TranscriptID, meta *schema.PullTranscriptInfo, dryRun bool) error {
	if dryRun {
		return nil
	}
	dir := p.pullDir(host, id)
	data, err := p.fs.ReadFile(filepath.Join(dir, MetadataFilename))
	if err != nil {
		return installError(err)
	}
	var old schema.PullTranscriptInfo
	if err := json.Unmarshal(data, &old); err != nil {
		return installError(err)
	}
	a, err := json.Marshal(old)
	if err != nil {
		return installError(err)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return installError(err)
	}
	if bytes.Equal(a, b) {
		return nil
	}
	manifest, ok := p.storedManifest(dir)
	if !ok {
		return installError(fmt.Errorf("metadata update requires a validated previous manifest"))
	}
	commit := p.buildCommit(host, dir, manifest.ServedBlobHash, p.clock.NowUnixMilli(), meta, nil)
	commit.Transcript.FirstPulledAt = manifest.PulledAt
	commit.Transcript.AnnotationCount = len(manifest.Annotations)
	return p.installContent(ctx, host, id, nil, meta, manifest, commit, false, true)
}

func (p *Pipeline) installContent(ctx context.Context, host string, id schema.TranscriptID, blob []byte, meta *schema.PullTranscriptInfo, manifest PullManifest, commit store.PullCommit, replaceInvalid, metadataOnly bool) error {
	root := p.installRoot(host, id)
	if info, err := p.fs.Lstat(root); err == nil && (!info.IsDir() || info.Mode()&fs.ModeSymlink != 0) {
		return installError(fmt.Errorf("owned install namespace is not a real directory; no files removed"))
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return installError(err)
	}
	name := uuid.NewString()
	stage := filepath.Join(root, name)
	if err := p.fs.MkdirAll(stage, defaults.PrivateDirPerm); err != nil {
		return installError(err)
	}
	if err := p.fs.WriteFile(filepath.Join(stage, stageOwnerFilename), []byte(id.String()), defaults.PrivateFilePerm); err != nil {
		return installError(errors.Join(err, p.fs.RemoveAll(stage)))
	}
	if err := p.syncInstall(filepath.Join(stage, stageOwnerFilename)); err != nil {
		return installError(err)
	}
	var stageErr error
	files := []string{TranscriptFilename, MetadataFilename, ManifestFilename}
	if metadataOnly {
		files = []string{MetadataFilename}
		stageErr = p.fs.MkdirAll(stage, defaults.PrivateDirPerm)
		if stageErr == nil {
			var data []byte
			data, stageErr = json.MarshalIndent(meta, "", "  ")
			if stageErr == nil {
				stageErr = p.fs.WriteFile(filepath.Join(stage, MetadataFilename), data, defaults.PrivateFilePerm)
			}
		}
	} else {
		stageErr = p.stageFiles(stage, blob, meta, manifest)
	}
	if stageErr != nil {
		return installError(errors.Join(stageErr, p.fs.RemoveAll(stage)))
	}
	for _, file := range files {
		if err := p.syncInstall(filepath.Join(stage, file)); err != nil {
			return installError(err)
		}
	}
	if err := p.syncInstall(stage); err != nil {
		return installError(err)
	}
	if err := p.syncInstallMove(root, p.pullDir(host, id)); err != nil {
		return installError(err)
	}
	digestDir := stage
	if metadataOnly {
		digestDir = p.pullDir(host, id)
	}
	digest, err := p.contentDigestWithMetadata(digestDir, filepath.Join(stage, MetadataFilename), host, id)
	if err != nil {
		return installError(err)
	}
	j := installJournal{Version: installJournalVersion, Stage: name, Digest: digest, Commit: commit, ReplaceInvalid: replaceInvalid, MetadataOnly: metadataOnly}
	data, err := json.Marshal(j)
	if err != nil {
		return installError(err)
	}
	journal := filepath.Join(root, "journal.json")
	if err := p.fs.WriteFile(journal+".tmp", data, defaults.PrivateFilePerm); err != nil {
		return installError(err)
	}
	if err := p.syncInstall(journal + ".tmp"); err != nil {
		return installError(err)
	}
	if err := p.fs.Rename(journal+".tmp", journal); err != nil {
		return installError(err)
	}
	if err := p.syncInstall(root); err != nil {
		return installError(err)
	}
	return p.recoverInstall(ctx, host, id, replaceInvalid)
}

// contentDigest validates the complete file set and binds the journal to exact
// bytes, including metadata/provenance. The transport's ETag remains separate.
func (p *Pipeline) contentDigest(dir, host string, id schema.TranscriptID) (string, error) {
	return p.contentDigestWithMetadata(dir, filepath.Join(dir, MetadataFilename), host, id)
}

func (p *Pipeline) contentDigestWithMetadata(dir, metadataPath, host string, id schema.TranscriptID) (string, error) {
	if err := p.validateDirectory(dir); err != nil {
		return "", err
	}
	for _, path := range []string{filepath.Join(dir, ManifestFilename), filepath.Join(dir, TranscriptFilename), metadataPath} {
		if err := p.validateRegular(path); err != nil {
			return "", err
		}
	}
	var manifest PullManifest
	var meta schema.PullTranscriptInfo
	m, err := p.fs.ReadFile(filepath.Join(dir, ManifestFilename))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(m, &manifest); err != nil {
		return "", err
	}
	b, err := p.fs.ReadFile(filepath.Join(dir, TranscriptFilename))
	if err != nil {
		return "", err
	}
	d, err := p.fs.ReadFile(metadataPath)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(d, &meta); err != nil {
		return "", err
	}
	if len(b) == 0 ||
		manifest.ManifestVersion != PullManifestVersion ||
		manifest.TranscriptID != id || meta.TranscriptID != id ||
		manifest.VillageHost != host || manifest.VillageURL != p.creds.VillageURL ||
		manifest.OwnerUserID != meta.OwnerUserID ||
		manifest.LocalSessionID != meta.LocalID || manifest.ServedBlobHash == "" {
		return "", fmt.Errorf("validate complete pull files: manifest, metadata or blob identity does not match transcript %s; retain files and re-pull with --force", id)
	}
	if schema.ComputeTranscriptHash(b) != manifest.ServedBlobHash {
		return "", fmt.Errorf("validate complete pull files: blob checksum does not match manifest for transcript %s; previous content retained; retry with --force", id)
	}
	// Hash each component first so concatenation has an unambiguous boundary.
	return schema.ComputeTranscriptHash([]byte(schema.ComputeTranscriptHash(m) + schema.ComputeTranscriptHash(b) + schema.ComputeTranscriptHash(d))), nil
}

func (p *Pipeline) exists(path string) (bool, error) {
	_, err := p.fs.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// recoverInstall is called with exclusive per-transcript OS ownership. It never
// removes a prior directory until exact replacement bytes and index commit have
// both been verified. Repeated index commits are idempotent store projections.
func (p *Pipeline) recoverInstall(ctx context.Context, host string, id schema.TranscriptID, force bool) error {
	root := p.installRoot(host, id)
	if info, err := p.fs.Lstat(root); err == nil && (!info.IsDir() || info.Mode()&fs.ModeSymlink != 0) {
		return installError(fmt.Errorf("owned recovery namespace is not a real directory; no files removed"))
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return installError(err)
	}
	journal := filepath.Join(root, "journal.json")
	if present, err := p.exists(journal); err != nil {
		return installError(err)
	} else if present {
		if err := p.validateRegular(journal); err != nil {
			return installError(err)
		}
	}
	data, err := p.fs.ReadFile(journal)
	if errors.Is(err, fs.ErrNotExist) {
		return p.cleanAbandonedStages(root, id)
	}
	if err != nil {
		return installError(err)
	}
	var j installJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return installError(err)
	}
	stageID, err := uuid.Parse(j.Stage)
	final := p.pullDir(host, id)
	if err != nil ||
		j.Version != installJournalVersion ||
		stageID.String() != j.Stage ||
		j.Digest == "" ||
		j.Commit.Transcript.TranscriptID != id ||
		j.Commit.Transcript.VillageHost != host || j.Commit.Transcript.PullDir != final {
		return installError(fmt.Errorf("invalid install journal identity; no files were removed"))
	}
	stage := filepath.Join(root, j.Stage)
	if present, err := p.exists(stage); err != nil {
		return installError(err)
	} else if present {
		if err := p.validateStage(stage, id); err != nil {
			return installError(err)
		}
	}
	if j.MetadataOnly {
		return p.recoverMetadata(ctx, host, id, root, stage, j)
	}
	backup := filepath.Join(root, "previous")
	staged, err := p.exists(stage)
	if err != nil {
		return installError(err)
	}
	if staged {
		digest, err := p.contentDigest(stage, host, id)
		if err != nil {
			return installError(err)
		}
		if digest != j.Digest {
			return installError(fmt.Errorf("staged file checksum differs from journal; previous content retained"))
		}
		if err := p.validateProjection(stage, j.Commit, false); err != nil {
			return installError(err)
		}
		present, err := p.exists(final)
		if err != nil {
			return installError(err)
		}
		if present {
			if err := p.validateDirectory(final); err != nil {
				return installError(err)
			}
			if _, err := p.contentDigest(final, host, id); err != nil && !j.ReplaceInvalid && !force {
				return installError(err)
			}
			previous, err := p.exists(backup)
			if err != nil {
				return installError(err)
			}
			if previous {
				return installError(fmt.Errorf("prior backup already exists; no files removed"))
			}
			if err := p.fs.Rename(final, backup); err != nil {
				return installError(err)
			}
			if err := p.syncInstallMove(root, final); err != nil {
				return installError(err)
			}
		}
		if err := p.fs.Rename(stage, final); err != nil {
			return installError(err)
		}
		if err := p.syncInstallMove(root, final); err != nil {
			return installError(err)
		}
	}
	if err := p.syncInstallMove(root, final); err != nil {
		return installError(err)
	}
	if err := p.validateStage(final, id); err != nil {
		return installError(err)
	}
	digest, err := p.contentDigest(final, host, id)
	if err != nil {
		return installError(err)
	}
	if digest != j.Digest {
		return installError(fmt.Errorf("published file checksum differs from journal; backup retained"))
	}
	if err := p.validateProjection(final, j.Commit, j.MetadataOnly); err != nil {
		return installError(err)
	}
	if err := p.store.CommitPull(ctx, j.Commit); err != nil {
		return installError(fmt.Errorf("project published files into database: %w", err))
	}
	if err := p.fs.RemoveAll(backup); err != nil {
		return installError(err)
	}
	if err := p.syncInstall(root); err != nil {
		return installError(err)
	}
	if err := p.fs.Remove(journal); err != nil {
		return installError(err)
	}
	if err := p.syncInstall(root); err != nil {
		return installError(err)
	}
	return p.cleanAbandonedStages(root, id)
}

func (p *Pipeline) validateDirectory(path string) error {
	info, err := p.fs.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("pull installation: expected a real directory, not a symlink; no unowned content may be published")
	}
	return nil
}
func (p *Pipeline) validateRegular(path string) error {
	info, err := p.fs.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("pull installation: expected a regular file, not a symlink or special file; retain previous content and repair the owned stage")
	}
	return nil
}
func (p *Pipeline) validateStage(path string, id schema.TranscriptID) error {
	if err := p.validateDirectory(path); err != nil {
		return err
	}
	marker := filepath.Join(path, stageOwnerFilename)
	if err := p.validateRegular(marker); err != nil {
		return err
	}
	owner, err := p.fs.ReadFile(marker)
	if err != nil {
		return err
	}
	if string(owner) != id.String() {
		return fmt.Errorf("pull installation: stage ownership marker does not match transcript; no files removed")
	}
	return nil
}

// Metadata publication is an atomic file replacement, so content/provenance
// paths never move. Keep the previous metadata until the new projection commits.
func (p *Pipeline) recoverMetadata(ctx context.Context, host string, id schema.TranscriptID, root, stage string, j installJournal) error {
	final := p.pullDir(host, id)
	metadata := filepath.Join(final, MetadataFilename)
	stagedMetadata := filepath.Join(stage, MetadataFilename)
	backup := filepath.Join(root, "previous-metadata.json")
	staged, err := p.exists(stagedMetadata)
	if err != nil {
		return installError(err)
	}
	if staged {
		digest, err := p.contentDigestWithMetadata(final, stagedMetadata, host, id)
		if err != nil {
			return installError(err)
		}
		if digest != j.Digest {
			return installError(fmt.Errorf("staged metadata checksum differs from journal; previous metadata retained"))
		}
		if err := p.validateProjectionWithMetadata(final, stagedMetadata, j.Commit, true); err != nil {
			return installError(err)
		}
		// A crash during a backup copy cannot make a partial file authoritative.
		// Staged metadata still present means the original has not been replaced.
		if err := p.fs.CopyFile(metadata, backup+".tmp", defaults.PrivateFilePerm); err != nil {
			return installError(err)
		}
		if err := p.syncInstall(backup + ".tmp"); err != nil {
			return installError(err)
		}
		if err := p.fs.Rename(backup+".tmp", backup); err != nil {
			return installError(err)
		}
		if err := p.syncInstall(root); err != nil {
			return installError(err)
		}
		if err := p.fs.Rename(stagedMetadata, metadata); err != nil {
			return installError(err)
		}
	}
	if err := p.syncInstall(final); err != nil {
		return installError(err)
	}
	if present, err := p.exists(stage); err != nil {
		return installError(err)
	} else if present {
		if err := p.syncInstall(stage); err != nil {
			return installError(err)
		}
	}
	digest, err := p.contentDigest(final, host, id)
	if err != nil {
		return installError(err)
	}
	if digest != j.Digest {
		return installError(fmt.Errorf("published metadata checksum differs from journal; previous metadata retained"))
	}
	if err := p.validateProjection(final, j.Commit, true); err != nil {
		return installError(err)
	}
	if err := p.store.CommitPull(ctx, j.Commit); err != nil {
		return installError(fmt.Errorf("project published metadata into database: %w", err))
	}
	if err := p.fs.RemoveAll(backup); err != nil {
		return installError(err)
	}
	if err := p.syncInstall(root); err != nil {
		return installError(err)
	}
	if err := p.fs.Remove(filepath.Join(root, "journal.json")); err != nil {
		return installError(err)
	}
	if err := p.syncInstall(root); err != nil {
		return installError(err)
	}
	return p.cleanAbandonedStages(root, id)
}

func (p *Pipeline) validateProjection(dir string, commit store.PullCommit, metadataOnly bool) error {
	return p.validateProjectionWithMetadata(dir, filepath.Join(dir, MetadataFilename), commit, metadataOnly)
}

func (p *Pipeline) validateProjectionWithMetadata(dir, metadataPath string, commit store.PullCommit, metadataOnly bool) error {
	data, err := p.fs.ReadFile(metadataPath)
	if err != nil {
		return err
	}
	var meta schema.PullTranscriptInfo
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	manifest, ok := p.storedManifest(dir)
	if !ok {
		return fmt.Errorf("cannot validate index projection without complete manifest")
	}
	annotations := make([]schema.PullAnnotation, 0, len(commit.Annotations))
	for _, row := range commit.Annotations {
		var a schema.PullAnnotation
		if err := json.Unmarshal([]byte(row.Payload), &a); err != nil {
			return err
		}
		annotations = append(annotations, a)
	}
	if err := validatePulledAnnotations(annotations); err != nil {
		return err
	}
	if metadataOnly {
		if len(annotations) != 0 {
			return fmt.Errorf("metadata-only index projection contains new annotations")
		}
	} else {
		a, err := json.Marshal(manifestAnnotations(annotations))
		if err != nil {
			return err
		}
		b, err := json.Marshal(manifest.Annotations)
		if err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("journal annotation projection differs from published provenance; previous backup retained")
		}
	}
	expected := p.buildCommit(manifest.VillageHost, commit.Transcript.PullDir, manifest.ServedBlobHash, commit.Transcript.LastPulledAt, &meta, annotations)
	expected.Transcript.FirstPulledAt = manifest.PulledAt
	// Metadata-only projection leaves existing annotation rows untouched.
	expected.Transcript.AnnotationCount = len(manifest.Annotations)
	a, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	b, err := json.Marshal(commit)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("journal index projection does not match published metadata and provenance; no index changes committed")
	}
	return nil
}

func (p *Pipeline) cleanAbandonedStages(root string, transcriptID schema.TranscriptID) error {
	entries, err := p.fs.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return installError(err)
	}
	for _, entry := range entries {
		if (entry.Name() == "journal.json.tmp" || entry.Name() == "previous-metadata.json.tmp") && !entry.IsDir() {
			if err := p.fs.Remove(filepath.Join(root, entry.Name())); err != nil {
				return installError(err)
			}
			continue
		}
		id, err := uuid.Parse(entry.Name())
		if err == nil && id.String() == entry.Name() && entry.IsDir() {
			owner, err := p.fs.ReadFile(filepath.Join(root, entry.Name(), stageOwnerFilename))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return installError(err)
			}
			if string(owner) != transcriptID.String() {
				continue
			}
			if err := p.fs.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return installError(err)
			}
		}
	}
	return nil
}
