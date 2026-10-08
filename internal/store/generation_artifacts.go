package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// Blob staging concurrency. A staged generation holds one content blob per
// part, so a large session is thousands of small files; writing them with one
// writer at a time made a single session the long pole of its batch.
// defaultBlobWriteWorkers bounds the writers inside one staging call;
// defaultBlobWriteSlots bounds how many blob writes are in flight across every
// session staging through this store, so many sessions staging at once cannot
// flood the page cache and the device queue. A test or benchmark may lower
// either knob on the concrete store to measure the serial behavior.
const (
	defaultBlobWriteWorkers = 8
	defaultBlobWriteSlots   = 16
)

// GenerationIntent is the durable record that a managed generation has been
// staged and is waiting for its one activation transaction. It carries the
// complete validated activation envelope so recovery replays the same guarded
// transaction the original activation requested: the producing indexer
// revision and time, the captured compare-and-swap state, the publication
// capture revision, the input proof and pair identity, and the content-capture
// evidence. A crash before the database commit leaves the staged candidate and
// this envelope; recovery replays them with current-state checks.
type GenerationIntent struct {
	SessionID    schema.SessionID `json:"sessionId"`
	GenerationID string           `json:"generationId"`
	ManifestPath string           `json:"manifestPath"`
	Completeness string           `json:"completeness"`
	StagedAtMs   int64            `json:"stagedAtMs"`

	IndexerVersion  int   `json:"indexerVersion,omitempty"`
	IndexedAtMs     int64 `json:"indexedAtMs,omitempty"`
	CaptureRevision int64 `json:"captureRevision,omitempty"`
	// ExplicitRebuild preserves an operator-initiated rebuild across crash
	// recovery so the replay honors the same last-good exemption the original
	// activation requested.
	ExplicitRebuild bool `json:"explicitRebuild,omitempty"`

	ExpectedState    *ingest.SessionIndexState         `json:"expectedState,omitempty"`
	ContentCapture   ingest.SessionContentCaptureWrite `json:"contentCapture"`
	IndexedInputHash *string                           `json:"indexedInputHash,omitempty"`
	ArtifactIdentity *string                           `json:"artifactIdentity,omitempty"`
	// PublicationCapture is the pipeline-certified capture agreement the replay
	// records with the same guarded transaction. Nil records none.
	PublicationCapture *ingest.PublicationCaptureWrite `json:"publicationCapture,omitempty"`
	// PriorEvidence preserves the activation-owned harness document across a
	// crash so recovery persists the same prior the successful path would have.
	PriorEvidence []byte `json:"priorEvidence,omitempty"`

	// CandidateDigest binds this envelope to the complete staged candidate: the
	// normalized whole-generation manifest plus every captured blob digest.
	// Recovery replays an envelope only when the staged bytes still produce the
	// same binding, so a rejected candidate can never be activated under a
	// different envelope.
	CandidateDigest string `json:"candidateDigest"`
}

// GenerationFootprint is the on-disk size of one owned generation directory.
// Bytes is the sum of the file sizes and Files is the number of regular files.
// A missing directory is the zero footprint, never an error.
type GenerationFootprint struct {
	Bytes int64
	Files int64
}

// Add accumulates another footprint.
func (f *GenerationFootprint) Add(other GenerationFootprint) {
	f.Bytes += other.Bytes
	f.Files += other.Files
}

// GenerationArtifactStore owns the file half of the crash protocol. The
// production implementation is root-confined through os.Root, writes the
// content blobs, fsyncs the manifest and the directory, and atomically renames
// the generation directory into place. Every binding that trusts a staged
// candidate re-reads and verifies its blobs. Tests substitute a failing
// implementation to interrupt a chosen seam.
type GenerationArtifactStore interface {
	// ReadIntent returns the recorded intent, or (nil, nil) when none exists.
	ReadIntent(context.Context, schema.SessionID) (*GenerationIntent, error)
	// ClearIntent removes the activation intent after the commit and repair.
	ClearIntent(context.Context, schema.SessionID) error
	// RemoveGeneration removes one INACTIVE owned generation directory.
	RemoveGeneration(context.Context, schema.SessionID, string) error
	// ReadManifest reads the self-contained manifest of a staged generation.
	ReadManifest(context.Context, schema.SessionID, string) (indexformat.Generation, error)
	// ReadBlob reads one immutable content blob addressed by its captured
	// relative path and verifies its length and integrity digest.
	ReadBlob(context.Context, schema.SessionID, string, indexformat.ContentRecord) ([]byte, error)
	// ReadPriorEvidence returns the persisted prior document for one
	// generation, or (nil, nil) when none was written.
	ReadPriorEvidence(context.Context, schema.SessionID, string) ([]byte, error)
	// GenerationSize reports the on-disk size of one owned generation
	// directory. A missing directory is the zero footprint, never an error, so
	// a row-first reclaim that crashed before removing files still plans
	// cleanly.
	GenerationSize(context.Context, schema.SessionID, string) (GenerationFootprint, error)
	// ListGenerationDirectories returns the installed generation directory
	// names owned by one session, sorted. It skips the owned temporary staging
	// directories. A missing session directory is an empty list, never an
	// error.
	ListGenerationDirectories(context.Context, schema.SessionID) ([]string, error)
}

// priorEvidenceName is the fixed file name of the activation-owned prior
// document inside one generation directory. It is not a content blob and does
// not participate in the manifest binding.
const priorEvidenceName = "prior.json"

// osGenerationArtifactStore is the production, root-confined implementation.
// Every filesystem operation runs through os.Root relative paths so a symlink
// at any ancestor inside the owned root cannot redirect reads, writes,
// renames or recursive removal outside the root. No error exposes a private
// filesystem path: diagnostics carry safe session/generation identifiers, the
// failed step, a sanitized reason, the caller effect and the recovery.
type osGenerationArtifactStore struct {
	root string
	// seam is a nil production hook that a crash-recovery test sets to fail at
	// one of the fsync/rename boundaries. Production never sets it.
	seam func(string) error
	// blobWorkers is the per-staging-call blob writer count; blobSlots is the
	// store-wide in-flight bound shared by every concurrent staging call. Both
	// are set by NewOSGenerationArtifactStore; a zero value stages serially
	// without a slot bound, which is what a hand-built test store gets.
	blobWorkers int
	blobSlots   chan struct{}
}

var _ GenerationArtifactStore = (*osGenerationArtifactStore)(nil)

// NewOSGenerationArtifactStore opens the generation namespace rooted at root.
func NewOSGenerationArtifactStore(root string) (GenerationArtifactStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("store: generation artifact root is empty in NewOSGenerationArtifactStore; managed content cannot be stored; configure the owned-artifact root")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("store: create owned-artifact root in NewOSGenerationArtifactStore: %s; no file was written; fix filesystem access and retry", sanitizeFSError(err))
	}
	return &osGenerationArtifactStore{
		root:        root,
		blobWorkers: defaultBlobWriteWorkers,
		blobSlots:   make(chan struct{}, defaultBlobWriteSlots),
	}, nil
}

// NewOSGenerationArtifactStoreExisting opens the owned-artifact root for
// read-only planning. It never creates the root: a dry run promises to change
// no file, and a store without an owned tree has no staged generation to read.
// A missing root is returned as fs.ErrNotExist so the caller can fall back to
// the retained baseline, which is the target set a fresh store resolves anyway.
func NewOSGenerationArtifactStoreExisting(root string) (GenerationArtifactStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("store: generation artifact root is empty in NewOSGenerationArtifactStoreExisting; managed content cannot be read; configure the owned-artifact root")
	}
	owned, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	_ = owned.Close()
	return &osGenerationArtifactStore{
		root:        root,
		blobWorkers: defaultBlobWriteWorkers,
		blobSlots:   make(chan struct{}, defaultBlobWriteSlots),
	}, nil
}

// openOwnedRoot confines one filesystem operation to the owned root.
func (a *osGenerationArtifactStore) openOwnedRoot() (*os.Root, error) {
	root, err := os.OpenRoot(a.root)
	if err != nil {
		return nil, fmt.Errorf("store: open owned-artifact root in generation artifacts: %s; no file was written; fix filesystem access and retry", sanitizeFSError(err))
	}
	return root, nil
}

// validateGenerationID is the central single-component generation identifier
// guard. It rejects empty names, dot and dotdot, non-local paths, separators,
// and non-canonical spellings before any filesystem operation, so "." can
// never resolve to the generations directory itself. It never echoes the
// rejected value: a rejected identifier can be an untrusted private path, so
// the refusal names the failed rule and the fix, never the offending bytes.
func validateGenerationID(generationID string) error {
	if strings.TrimSpace(generationID) == "" {
		return fmt.Errorf("store: generation identifier is empty in generation validation; no file was written; supply an installed generation identifier")
	}
	if generationID == "." || generationID == ".." {
		return fmt.Errorf("store: generation identifier is a directory reference in generation validation; no file was written; supply an installed generation identifier")
	}
	if !filepath.IsLocal(generationID) {
		return fmt.Errorf("store: generation identifier is not a confined name in generation validation; no file was written; supply an installed generation identifier")
	}
	if strings.ContainsAny(generationID, `/\`) {
		return fmt.Errorf("store: generation identifier carries a separator in generation validation; no file was written; supply a single-component generation identifier")
	}
	if cleaned := path.Clean(generationID); cleaned != generationID {
		return fmt.Errorf("store: generation identifier is not canonical in generation validation; no file was written; supply the cleaned single-component identifier")
	}
	if cleaned := filepath.Clean(generationID); cleaned != generationID {
		return fmt.Errorf("store: generation identifier is not canonical for this platform in generation validation; no file was written; supply the cleaned single-component identifier")
	}
	if strings.HasPrefix(generationID, ".tmp-gen-") {
		return fmt.Errorf("store: generation identifier uses the reserved temporary prefix in generation validation; no file was written; supply an installed generation identifier")
	}
	return nil
}

// sanitizeFSError strips private filesystem paths from OS errors. It reports
// the sanitized errno category (permission, absence, busy) without the
// PathError/LinkError path that would disclose the owned root.
func sanitizeFSError(err error) string {
	if err == nil {
		return "unknown filesystem failure"
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		if pathErr.Err != nil {
			return pathErr.Err.Error()
		}
		return "filesystem operation failed"
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		if linkErr.Err != nil {
			return linkErr.Err.Error()
		}
		return "filesystem link operation failed"
	}
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) {
		if syscallErr.Err != nil {
			return syscallErr.Err.Error()
		}
		return "filesystem operation failed"
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "no such file or directory"
	}
	if errors.Is(err, fs.ErrPermission) {
		return "permission denied"
	}
	if errors.Is(err, fs.ErrExist) {
		return "file exists"
	}
	return err.Error()
}

func (a *osGenerationArtifactStore) sessionRel(id schema.SessionID) (string, error) {
	if _, err := schema.NewSessionID(string(id)); err != nil {
		return "", fmt.Errorf("store: generation artifacts for session in session validation: %w; no file was written; supply a canonical session identifier", err)
	}
	if !filepath.IsLocal(string(id)) {
		return "", fmt.Errorf("store: generation artifacts for session in session validation are not a confined name; no file was written; supply a canonical session identifier")
	}
	return string(id), nil
}

func (a *osGenerationArtifactStore) generationRel(id schema.SessionID, generationID string) (sessionRel, genRel string, err error) {
	sessionRel, err = a.sessionRel(id)
	if err != nil {
		return "", "", err
	}
	if err := validateGenerationID(generationID); err != nil {
		return "", "", err
	}
	return sessionRel, path.Join(sessionRel, "generations", generationID), nil
}

func (a *osGenerationArtifactStore) ReadIntent(ctx context.Context, id schema.SessionID) (*GenerationIntent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessionRel, err := a.sessionRel(id)
	if err != nil {
		return nil, err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := root.ReadFile(path.Join(sessionRel, "generation-intent.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read activation intent in ReadIntent for session %s: %s; the pending activation state cannot be reconciled", id, sanitizeFSError(err))
	}
	var intent GenerationIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return nil, fmt.Errorf("store: decode activation intent in ReadIntent for session %s: %s; the pending activation state cannot be reconciled", id, sanitizeFSError(err))
	}
	return &intent, nil
}

func (a *osGenerationArtifactStore) ClearIntent(ctx context.Context, id schema.SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sessionRel, err := a.sessionRel(id)
	if err != nil {
		return err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(path.Join(sessionRel, "generation-intent.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: clear activation intent in ClearIntent for session %s: %s; the generation is active but the pending marker remains", id, sanitizeFSError(err))
	}
	return nil
}

func (a *osGenerationArtifactStore) RemoveGeneration(ctx context.Context, id schema.SessionID, generationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateGenerationID(generationID); err != nil {
		return err
	}
	_, genRel, err := a.generationRel(id, generationID)
	if err != nil {
		return err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	// Refuse a symlinked path component before any recursive removal. os.Root
	// confines an escape outside the owned root, but it still FOLLOWS a
	// relative symlink that resolves to another location INSIDE the root, so a
	// symlinked ancestor (for example this session's generations directory
	// pointing at an unrelated in-root sibling) would redirect RemoveAll. The
	// component walk is the only check that sees the link; the manifest
	// identity and validity checks below then prove the resolved target is the
	// requested owner's generation. The directory is left in place on every
	// refusal.
	info, err := noFollowRemovalPath(root, genRel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("store: inspect inactive generation %s for session %s in RemoveGeneration: %s; the directory was left in place", generationID, id, sanitizeFSError(err))
	}
	if !info.IsDir() {
		return fmt.Errorf("store: refuse to remove generation %s for session %s in RemoveGeneration: the owned target is not a generation directory; the directory was left in place", generationID, id)
	}
	if err := verifyStagedGenerationOwnership(root, genRel, id, generationID); err != nil {
		return err
	}
	if err := root.RemoveAll(genRel); err != nil {
		return fmt.Errorf("store: remove inactive generation %s for session %s in RemoveGeneration: %s; the directory was left in place", generationID, id, sanitizeFSError(err))
	}
	return nil
}

// noFollowRemovalPath walks every component of a root-relative path with a
// no-follow Lstat and refuses when any component is a symbolic link or an
// intermediate component is not a directory. It returns the final component's
// FileInfo. os.Root resolves a relative symlink that stays INSIDE the root, so
// confinement alone cannot prove a delete target is the path it names; only a
// per-component Lstat sees the link. It runs on the delete and cleanup path
// only, one Lstat per path component, and never on reads or snapshots.
func noFollowRemovalPath(root *os.Root, rel string) (fs.FileInfo, error) {
	components := strings.Split(rel, "/")
	prefix := ""
	var final fs.FileInfo
	for index, component := range components {
		if prefix == "" {
			prefix = component
		} else {
			prefix += "/" + component
		}
		info, err := root.Lstat(prefix)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("path component %d is a symbolic link; a symlinked ancestor cannot be traversed for deletion", index)
		}
		if index < len(components)-1 && !info.IsDir() {
			return nil, fmt.Errorf("path component %d is not a directory", index)
		}
		final = info
	}
	return final, nil
}

// verifyStagedGenerationOwnership proves a candidate directory is really the
// owned generation for (sessionID, generationID) before any recursive delete.
// The directory name and a decodable manifest are not ownership: the manifest
// must name this exact session and generation and must itself be a valid,
// self-contained generation. An empty {}, malformed, mismatched or unknown
// manifest is refused, so an unrelated directory is never removed. The
// manifest's own identifiers and the validator's error text are untrusted and
// may carry a private path, so a refusal reports the fixed mismatch or
// invalid-manifest category against the already-validated requested owner
// instead of echoing any manifest bytes.
func verifyStagedGenerationOwnership(root *os.Root, genRel string, sessionID schema.SessionID, generationID string) error {
	data, err := root.ReadFile(path.Join(genRel, "manifest.json"))
	if err != nil {
		return fmt.Errorf("store: refuse to remove generation %s for session %s in RemoveGeneration: its staged manifest cannot be read (%s); the directory was left in place", generationID, sessionID, sanitizeFSError(err))
	}
	var staged indexformat.Generation
	if err := json.Unmarshal(data, &staged); err != nil {
		return fmt.Errorf("store: refuse to remove generation %s for session %s in RemoveGeneration: its staged manifest cannot be decoded (%s); the directory was left in place", generationID, sessionID, sanitizeFSError(err))
	}
	if staged.ID != generationID || staged.Metadata.SessionID != sessionID {
		return fmt.Errorf("store: refuse to remove generation %s for session %s in RemoveGeneration: the staged manifest identity does not match the requested owner; the directory was left in place", generationID, sessionID)
	}
	if err := staged.Validate(); err != nil {
		return fmt.Errorf("store: refuse to remove generation %s for session %s in RemoveGeneration: the staged manifest identity matches but the generation is not valid and self-contained; the directory was left in place", generationID, sessionID)
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
	if err := validateGenerationID(generationID); err != nil {
		return indexformat.Generation{}, err
	}
	_, genRel, err := a.generationRel(id, generationID)
	if err != nil {
		return indexformat.Generation{}, err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return indexformat.Generation{}, err
	}
	defer root.Close()
	data, err := root.ReadFile(path.Join(genRel, "manifest.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return indexformat.Generation{}, fmt.Errorf("%w for generation %s of session %s", ErrStagedGenerationAbsent, generationID, id)
		}
		return indexformat.Generation{}, fmt.Errorf("store: read staged manifest in ReadManifest for generation %s of session %s: %s; the candidate cannot be recovered", generationID, id, sanitizeFSError(err))
	}
	var generation indexformat.Generation
	if err := json.Unmarshal(data, &generation); err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: decode staged manifest in ReadManifest for generation %s of session %s: %s; the candidate cannot be recovered", generationID, id, sanitizeFSError(err))
	}
	return generation, nil
}

func (a *osGenerationArtifactStore) ReadBlob(ctx context.Context, id schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := record.Validate(); err != nil {
		return nil, fmt.Errorf("store: resolve managed content in ReadBlob for session %s: %w; no blob was read", id, err)
	}
	if err := validateGenerationID(generationID); err != nil {
		return nil, err
	}
	_, genRel, err := a.generationRel(id, generationID)
	if err != nil {
		return nil, err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := root.ReadFile(path.Join(genRel, record.RelativeBlob))
	if err != nil {
		return nil, fmt.Errorf("store: read managed content %s of generation %s for session %s in ReadBlob: %s; the committed artifact is missing or corrupt and unreadable; run managed recovery rather than treating the content as empty", record.Ref, generationID, id, sanitizeFSError(err))
	}
	if int64(len(data)) != record.ByteLength {
		return nil, fmt.Errorf("store: managed content %s of generation %s for session %s in ReadBlob is %d bytes, recorded %d; the artifact is corrupt; run managed recovery rather than serving partial content", record.Ref, generationID, id, len(data), record.ByteLength)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != record.Digest {
		return nil, fmt.Errorf("store: managed content %s of generation %s for session %s in ReadBlob fails its integrity digest; the artifact is corrupt; run managed recovery rather than serving altered content", record.Ref, generationID, id)
	}
	return data, nil
}

// ReadPriorEvidence returns the persisted prior document for one generation.
// A generation that never carried a prior document returns (nil, nil); a
// missing or unreadable directory for another reason is an error, never an
// empty document that would silently rekey unchanged source.
func (a *osGenerationArtifactStore) ReadPriorEvidence(ctx context.Context, id schema.SessionID, generationID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateGenerationID(generationID); err != nil {
		return nil, err
	}
	_, genRel, err := a.generationRel(id, generationID)
	if err != nil {
		return nil, err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := root.ReadFile(path.Join(genRel, priorEvidenceName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read prior evidence for generation %s of session %s: %s; the prior cannot be loaded; restore the owned generation directory and retry", generationID, id, sanitizeFSError(err))
	}
	if len(data) == 0 {
		return nil, nil
	}
	return data, nil
}

// GenerationSize reports the on-disk footprint of one owned generation
// directory through the root-confined view. A missing directory is the zero
// footprint: a row-first reclaim that already removed the directory, or a
// generation that was never staged, is not an error.
func (a *osGenerationArtifactStore) GenerationSize(ctx context.Context, id schema.SessionID, generationID string) (GenerationFootprint, error) {
	if err := ctx.Err(); err != nil {
		return GenerationFootprint{}, err
	}
	_, genRel, err := a.generationRel(id, generationID)
	if err != nil {
		return GenerationFootprint{}, err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return GenerationFootprint{}, err
	}
	defer root.Close()
	return footprintUnderRoot(root, genRel)
}

// ListGenerationDirectories returns the installed generation directory names
// owned by one session. It reads through the owned root so a namespace symlink
// cannot redirect the scan, skips the reserved temporary staging directories,
// and refuses a name the central identifier guard rejects so a caller never
// treats an unowned entry as a generation. A missing session directory is an
// empty list.
func (a *osGenerationArtifactStore) ListGenerationDirectories(ctx context.Context, id schema.SessionID) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessionRel, err := a.sessionRel(id)
	if err != nil {
		return nil, err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(path.Join(sessionRel, "generations"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list generation directories for session %s: %s; the owned generation set cannot be read", id, sanitizeFSError(err))
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("store: list generation directories for session %s: %s; the owned generation set cannot be read", id, sanitizeFSError(err))
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".tmp-gen-") {
			continue
		}
		if err := validateGenerationID(name); err != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// footprintUnderRoot sums the regular files under one root-relative directory.
// A missing directory is the zero footprint. The walk runs through the
// root-confined filesystem view, so a symlink cannot redirect it outside the
// owned root.
func footprintUnderRoot(root *os.Root, rel string) (GenerationFootprint, error) {
	var footprint GenerationFootprint
	err := fs.WalkDir(root.FS(), rel, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		footprint.Bytes += info.Size()
		footprint.Files++
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return GenerationFootprint{}, fmt.Errorf("store: measure generation directory: %s; the footprint could not be read", sanitizeFSError(err))
	}
	return footprint, nil
}

// writeRootFile writes one managed file without a per-file sync. Content blobs
// use it: the crash protocol's durability point is the manifest, the intent and
// the rename, and every binding that trusts a staged candidate re-reads each
// blob through ReadBlob, which verifies its length and digest, so a power loss
// that loses blob bytes refuses recovery instead of activating a generation
// whose content cannot be served. The pair installer follows the same model:
// the database commit, not the file write, is the durability point.
func writeRootFile(root *os.Root, rel string, data []byte) error {
	file, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create managed file in generation staging: %s; the temporary candidate is removed and the active generation is unchanged", sanitizeFSError(err))
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write managed file in generation staging: %s; the temporary candidate is removed and the active generation is unchanged", sanitizeFSError(err))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close managed file in generation staging: %s; the temporary candidate is removed and the active generation is unchanged", sanitizeFSError(err))
	}
	return nil
}

func writeRootSyncedFile(root *os.Root, rel string, data []byte) error {
	file, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create managed file in generation staging: %s; the temporary candidate is removed and the active generation is unchanged", sanitizeFSError(err))
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write managed file in generation staging: %s; the temporary candidate is removed and the active generation is unchanged", sanitizeFSError(err))
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("fsync managed file in generation staging: %s; the temporary candidate is removed and the active generation is unchanged", sanitizeFSError(err))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close managed file in generation staging: %s; the temporary candidate is removed and the active generation is unchanged", sanitizeFSError(err))
	}
	return nil
}

func writeRootSyncedAtomic(root *os.Root, rel string, data []byte, sessionID schema.SessionID, generationID, step string) error {
	dir := path.Dir(rel)
	base := path.Base(rel)
	tmpRel := ""
	for attempt := 0; attempt < 32; attempt++ {
		candidate := path.Join(dir, fmt.Sprintf(".tmp-%s-%d-%d", base, os.Getpid(), attempt))
		file, err := root.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return fmt.Errorf("store: create temporary file in %s for session %s: %s; no file was written; fix filesystem access and retry", step, sessionID, sanitizeFSError(err))
		}
		tmpRel = candidate
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			_ = root.Remove(tmpRel)
			return fmt.Errorf("store: write temporary file in %s for session %s: %s; the previous durable file is unchanged; fix filesystem access and retry", step, sessionID, sanitizeFSError(err))
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = root.Remove(tmpRel)
			return fmt.Errorf("store: fsync temporary file in %s for session %s: %s; the previous durable file is unchanged; fix filesystem access and retry", step, sessionID, sanitizeFSError(err))
		}
		if err := file.Close(); err != nil {
			_ = root.Remove(tmpRel)
			return fmt.Errorf("store: close temporary file in %s for session %s: %s; the previous durable file is unchanged; fix filesystem access and retry", step, sessionID, sanitizeFSError(err))
		}
		break
	}
	if tmpRel == "" {
		return fmt.Errorf("store: create temporary file in %s for session %s: file exists; no file was written; clear stale temporary files and retry", step, sessionID)
	}
	_ = generationID
	if err := root.Rename(tmpRel, rel); err != nil {
		_ = root.Remove(tmpRel)
		return fmt.Errorf("store: atomically rename file in %s for session %s: %s; the previous durable file is unchanged; fix filesystem access and retry", step, sessionID, sanitizeFSError(err))
	}
	return fsyncGenerationStagingDir(root, dir)
}
