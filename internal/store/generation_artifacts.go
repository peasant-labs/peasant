package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// GenerationIntent is the durable record that a managed generation has been
// staged and is waiting for its one activation transaction. It is written
// before the atomic rename of the generation directory and cleared after the
// database commit and metadata repair.
type GenerationIntent struct {
	SessionID    schema.SessionID `json:"sessionId"`
	GenerationID string           `json:"generationId"`
	ManifestPath string           `json:"manifestPath"`
	Completeness string           `json:"completeness"`
	StagedAtMs   int64            `json:"stagedAtMs"`
}

// GenerationArtifactStore owns the file half of the crash protocol. The
// production implementation is root-confined and fsyncs every file and
// directory before the generation directory is atomically renamed into place.
// Tests substitute a failing implementation to interrupt a chosen seam.
type GenerationArtifactStore interface {
	// Stage writes the generation's content blobs and manifest under an owned,
	// root-confined generation directory, fsyncs every file and directory, and
	// atomically renames the complete directory into place. It returns the
	// generation with its ContentRecord relative paths, byte lengths and
	// integrity digests filled in.
	Stage(context.Context, indexformat.Generation, map[schema.SourceEntryRef][]byte) (indexformat.Generation, error)
	// WriteIntent durably records the activation intent before the DB commit.
	WriteIntent(context.Context, GenerationIntent) error
	// ReadIntent returns the recorded intent, or (nil, nil) when none exists.
	ReadIntent(context.Context, schema.SessionID) (*GenerationIntent, error)
	// ClearIntent removes the activation intent after the commit and repair.
	ClearIntent(context.Context, schema.SessionID) error
	// RepairMetadata writes the exported metadata projection after the commit.
	RepairMetadata(context.Context, schema.SessionID, []byte) error
	// RemoveGeneration removes one INACTIVE owned generation directory.
	RemoveGeneration(context.Context, schema.SessionID, string) error
	// ReadManifest reads the self-contained manifest of a staged generation.
	ReadManifest(context.Context, schema.SessionID, string) (indexformat.Generation, error)
	// ReadBlob reads one immutable content blob addressed by its captured
	// relative path and verifies its length and integrity digest.
	ReadBlob(context.Context, schema.SessionID, string, indexformat.ContentRecord) ([]byte, error)
}

// osGenerationArtifactStore is the production, root-confined implementation.
type osGenerationArtifactStore struct {
	root string
	// seam is a nil production hook that a crash-recovery test sets to fail at
	// one of the fsync/rename boundaries. Production never sets it.
	seam func(string) error
}

var _ GenerationArtifactStore = (*osGenerationArtifactStore)(nil)

// NewOSGenerationArtifactStore opens the generation namespace rooted at root.
func NewOSGenerationArtifactStore(root string) (GenerationArtifactStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("store: generation artifact root is empty; managed content cannot be stored; configure the owned-artifact root")
	}
	return &osGenerationArtifactStore{root: root}, nil
}

func (a *osGenerationArtifactStore) sessionDir(id schema.SessionID) (string, error) {
	if _, err := schema.NewSessionID(string(id)); err != nil {
		return "", fmt.Errorf("store: generation artifacts for %q: %w; no file was written; supply a canonical session identifier", id, err)
	}
	if !filepath.IsLocal(string(id)) {
		return "", fmt.Errorf("store: generation artifacts for %q are not a confined name; no file was written; supply a canonical session identifier", id)
	}
	return filepath.Join(a.root, string(id)), nil
}

func (a *osGenerationArtifactStore) generationDir(id schema.SessionID, generationID string) (string, error) {
	dir, err := a.sessionDir(id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(generationID) == "" || !filepath.IsLocal(generationID) || strings.ContainsAny(generationID, `/\`) {
		return "", fmt.Errorf("store: generation id %q is not a confined name; no file was written; supply an installed generation id", generationID)
	}
	return filepath.Join(dir, "generations", generationID), nil
}

func blobName(ref schema.SourceEntryRef) string {
	sum := sha256.Sum256([]byte("peasant.generation.blob.v1\x00" + string(ref)))
	return "c_" + hex.EncodeToString(sum[:]) + ".blob"
}

func (a *osGenerationArtifactStore) Stage(ctx context.Context, generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte) (indexformat.Generation, error) {
	genDir, err := a.generationDir(generation.Metadata.SessionID, generation.ID)
	if err != nil {
		return indexformat.Generation{}, err
	}
	parent := filepath.Dir(genDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: create generation parent %s: %w; no generation was staged", parent, err)
	}
	// A previous interrupted staging may have left an owned temporary directory.
	// Activation is serialized by the exclusive session lock, so no live writer
	// owns one; removing stale temp candidates here cannot touch the active
	// generation.
	if stale, err := filepath.Glob(filepath.Join(parent, ".tmp-gen-*")); err == nil {
		for _, dir := range stale {
			_ = os.RemoveAll(dir)
		}
	}
	tmpDir, err := os.MkdirTemp(parent, ".tmp-gen-"+generation.ID+"-")
	if err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: create temporary generation directory under %s: %w; no generation was staged", parent, err)
	}
	fail := func(cause error) (indexformat.Generation, error) {
		if removeErr := os.RemoveAll(tmpDir); removeErr != nil {
			return indexformat.Generation{}, fmt.Errorf("store: stage generation %s for session %s: %v; additionally failed to remove the staged candidate: %w", generation.ID, generation.Metadata.SessionID, cause, removeErr)
		}
		return indexformat.Generation{}, fmt.Errorf("store: stage generation %s for session %s: %w; the temporary candidate was removed and the active generation is unchanged", generation.ID, generation.Metadata.SessionID, cause)
	}
	filled := append([]indexformat.ContentRecord(nil), generation.Content...)
	refs := make([]int, 0, len(filled))
	for i := range filled {
		refs = append(refs, i)
	}
	sort.Slice(refs, func(i, j int) bool { return string(filled[refs[i]].Ref) < string(filled[refs[j]].Ref) })
	for _, i := range refs {
		ref := filled[i].Ref
		payload, ok := blobs[ref]
		if !ok {
			return fail(fmt.Errorf("content blob for ref %q is missing; the generation is not self-contained; supply every captured blob", ref))
		}
		name := blobName(ref)
		if err := writeSyncedFile(filepath.Join(tmpDir, name), payload); err != nil {
			return fail(err)
		}
		digest := sha256.Sum256(payload)
		filled[i].RelativeBlob = name
		filled[i].ByteLength = int64(len(payload))
		filled[i].Digest = hex.EncodeToString(digest[:])
	}
	generation.Content = filled
	// The manifest is the self-contained durable projection of the generation.
	manifest, err := json.Marshal(generation)
	if err != nil {
		return fail(fmt.Errorf("encode generation manifest: %w", err))
	}
	if err := writeSyncedFile(filepath.Join(tmpDir, "manifest.json"), manifest); err != nil {
		return fail(err)
	}
	// fsync the temporary directory so a crash cannot lose the rename target.
	if a.seam != nil {
		if err := a.seam("before-temp-fsync"); err != nil {
			return fail(err)
		}
	}
	if err := fsyncDir(tmpDir); err != nil {
		return fail(err)
	}
	if a.seam != nil {
		if err := a.seam("after-fsync-before-rename"); err != nil {
			return fail(err)
		}
	}
	if err := os.RemoveAll(genDir); err != nil {
		return fail(fmt.Errorf("remove prior unactivated candidate at %s: %w", genDir, err))
	}
	if err := os.Rename(tmpDir, genDir); err != nil {
		return fail(fmt.Errorf("atomically rename staged generation into %s: %w", genDir, err))
	}
	if err := fsyncDir(parent); err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: fsync generation parent %s after rename: %w; the staged generation exists but was not durably recorded; activation will be refused and retried", parent, err)
	}
	if a.seam != nil {
		if err := a.seam("after-rename-before-db"); err != nil {
			return indexformat.Generation{}, err
		}
	}
	generation.Content = filled
	return generation, nil
}

// reorderContent restores the caller's original content ordering after the
// deterministic write pass.
func reorderContent(filled []indexformat.ContentRecord, byRef map[schema.SourceEntryRef]int) []indexformat.ContentRecord {
	_ = byRef
	return filled
}

func (a *osGenerationArtifactStore) WriteIntent(ctx context.Context, intent GenerationIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := a.sessionDir(intent.SessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create session artifact directory %s: %w; no intent was recorded", dir, err)
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("store: encode activation intent for %s: %w; no intent was recorded", intent.SessionID, err)
	}
	return writeSyncedAtomic(filepath.Join(dir, "generation-intent.json"), encoded)
}

func (a *osGenerationArtifactStore) ReadIntent(ctx context.Context, id schema.SessionID) (*GenerationIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := a.sessionDir(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "generation-intent.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read activation intent for %s: %w; the pending activation state cannot be reconciled", id, err)
	}
	var intent GenerationIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return nil, fmt.Errorf("store: decode activation intent for %s: %w; the pending activation state cannot be reconciled", id, err)
	}
	return &intent, nil
}

func (a *osGenerationArtifactStore) ClearIntent(ctx context.Context, id schema.SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := a.sessionDir(id)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "generation-intent.json")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: clear activation intent for %s: %w; the generation is active but the pending marker remains", id, err)
	}
	return nil
}

func (a *osGenerationArtifactStore) RepairMetadata(ctx context.Context, id schema.SessionID, metadata []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := a.sessionDir(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create session artifact directory %s for metadata repair: %w", dir, err)
	}
	return writeSyncedAtomic(filepath.Join(dir, "metadata.json"), metadata)
}

func (a *osGenerationArtifactStore) RemoveGeneration(ctx context.Context, id schema.SessionID, generationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	genDir, err := a.generationDir(id, generationID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(genDir); err != nil {
		return fmt.Errorf("store: remove inactive generation %s for session %s: %w; the directory was left in place", generationID, id, err)
	}
	return nil
}

// ErrStagedGenerationAbsent marks a generation directory that was never renamed
// into place. Recovery clears the stale intent for it rather than failing; the
// candidate was never durable.
var ErrStagedGenerationAbsent = errors.New("no staged generation directory exists")

func (a *osGenerationArtifactStore) ReadManifest(ctx context.Context, id schema.SessionID, generationID string) (indexformat.Generation, error) {
	if err := ctx.Err(); err != nil {
		return indexformat.Generation{}, err
	}
	genDir, err := a.generationDir(id, generationID)
	if err != nil {
		return indexformat.Generation{}, err
	}
	data, err := os.ReadFile(filepath.Join(genDir, "manifest.json"))
	if os.IsNotExist(err) {
		return indexformat.Generation{}, fmt.Errorf("%w for generation %s of session %s", ErrStagedGenerationAbsent, generationID, id)
	}
	if err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: read staged manifest for generation %s of session %s: %w; the candidate cannot be recovered", generationID, id, err)
	}
	var generation indexformat.Generation
	if err := json.Unmarshal(data, &generation); err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: decode staged manifest for generation %s of session %s: %w; the candidate cannot be recovered", generationID, id, err)
	}
	return generation, nil
}

func (a *osGenerationArtifactStore) ReadBlob(ctx context.Context, id schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := record.Validate(); err != nil {
		return nil, fmt.Errorf("store: resolve managed content for session %s: %w; no blob was read", id, err)
	}
	genDir, err := a.generationDir(id, generationID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(genDir, record.RelativeBlob))
	if err != nil {
		return nil, fmt.Errorf("store: read managed content %s of generation %s for session %s: %w; the committed artifact is missing or unreadable; run managed recovery rather than treating the content as empty", record.Ref, generationID, id, err)
	}
	if int64(len(data)) != record.ByteLength {
		return nil, fmt.Errorf("store: managed content %s of generation %s for session %s is %d bytes, recorded %d; the artifact is corrupt; run managed recovery rather than serving partial content", record.Ref, generationID, id, len(data), record.ByteLength)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != record.Digest {
		return nil, fmt.Errorf("store: managed content %s of generation %s for session %s fails its integrity digest; the artifact is corrupt; run managed recovery rather than serving altered content", record.Ref, generationID, id)
	}
	return data, nil
}

func writeSyncedFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create managed file %s: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write managed file %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("fsync managed file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close managed file %s: %w", path, err)
	}
	return nil
}

func writeSyncedAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temporary file for %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("fsync temporary file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temporary file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomically rename %s into place: %w", path, err)
	}
	return fsyncDir(dir)
}

func fsyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory %s to fsync: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fsync directory %s: %w", path, err)
	}
	return nil
}
