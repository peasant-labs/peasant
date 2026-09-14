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

	ExpectedState    *ingest.SessionIndexState         `json:"expectedState,omitempty"`
	ContentCapture   ingest.SessionContentCaptureWrite `json:"contentCapture"`
	IndexedInputHash *string                           `json:"indexedInputHash,omitempty"`
	ArtifactIdentity *string                           `json:"artifactIdentity,omitempty"`

	// CandidateDigest binds this envelope to the complete staged candidate: the
	// normalized whole-generation manifest plus every captured blob digest.
	// Recovery replays an envelope only when the staged bytes still produce the
	// same binding, so a rejected candidate can never be activated under a
	// different envelope.
	CandidateDigest string `json:"candidateDigest"`
}

// GenerationArtifactStore owns the file half of the crash protocol. The
// production implementation is root-confined through os.Root and fsyncs every
// file and directory before the generation directory is atomically renamed
// into place. Tests substitute a failing implementation to interrupt a chosen
// seam.
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
	return &osGenerationArtifactStore{root: root}, nil
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

func blobName(ref schema.SourceEntryRef) string {
	sum := sha256.Sum256([]byte("peasant.generation.blob.v1\x00" + string(ref)))
	return "c_" + hex.EncodeToString(sum[:]) + ".blob"
}

func (a *osGenerationArtifactStore) Stage(ctx context.Context, generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte) (indexformat.Generation, error) {
	sessionID := generation.Metadata.SessionID
	if err := ctx.Err(); err != nil {
		return indexformat.Generation{}, err
	}
	if err := validateGenerationID(generation.ID); err != nil {
		return indexformat.Generation{}, err
	}
	if _, err := a.sessionRel(sessionID); err != nil {
		return indexformat.Generation{}, err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return indexformat.Generation{}, err
	}
	defer root.Close()
	sessionRel, genRel, err := a.generationRel(sessionID, generation.ID)
	if err != nil {
		return indexformat.Generation{}, err
	}
	parentRel := path.Join(sessionRel, "generations")
	if err := root.MkdirAll(parentRel, 0o700); err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: create generation parent in Stage for session %s generation %s: %s; no generation was staged", sessionID, generation.ID, sanitizeFSError(err))
	}
	// A previous interrupted staging may have left an owned temporary directory.
	// Activation is serialized by the exclusive session lock, so no live writer
	// owns one; removing stale temp candidates here cannot touch the active
	// generation. Listing runs through the owned root so a namespace symlink
	// cannot redirect the scan.
	if stale := listStaleTempDirs(root, parentRel); stale != nil {
		for _, dir := range stale {
			_ = root.RemoveAll(dir)
		}
	}
	tmpRel, err := makeTempGenDir(root, parentRel, generation.ID)
	if err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: create temporary generation directory in Stage for session %s generation %s: %s; no generation was staged", sessionID, generation.ID, sanitizeFSError(err))
	}
	fail := func(cause error) (indexformat.Generation, error) {
		_ = root.RemoveAll(tmpRel)
		return indexformat.Generation{}, fmt.Errorf("store: stage generation %s for session %s in Stage: %s; the temporary candidate was removed and the active generation is unchanged", generation.ID, sessionID, sanitizeFSError(cause))
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
		if err := writeRootSyncedFile(root, path.Join(tmpRel, name), payload); err != nil {
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
		return fail(fmt.Errorf("encode generation manifest: %s", sanitizeFSError(err)))
	}
	if err := writeRootSyncedFile(root, path.Join(tmpRel, "manifest.json"), manifest); err != nil {
		return fail(err)
	}
	// fsync the temporary directory so a crash cannot lose the rename target.
	if a.seam != nil {
		if err := a.seam("before-temp-fsync"); err != nil {
			return fail(err)
		}
	}
	if err := fsyncRootDir(root, tmpRel); err != nil {
		return fail(err)
	}
	if a.seam != nil {
		if err := a.seam("after-fsync-before-rename"); err != nil {
			return fail(err)
		}
	}
	// Immutable generations never move: an existing generation directory with
	// the same identifier must bind to the identical complete candidate (a
	// crash retry) or the staging is refused. The comparison covers the whole
	// manifest and every blob digest, never a partial main-preview subset, and
	// blindly deleting an existing directory would destroy committed content.
	if _, err := root.Lstat(genRel); err == nil {
		existing, readErr := root.ReadFile(path.Join(genRel, "manifest.json"))
		if readErr != nil {
			return fail(fmt.Errorf("generation %s is already installed and its manifest cannot be verified; staging was refused and the installed generation is unchanged", generation.ID))
		}
		var installed indexformat.Generation
		if err := json.Unmarshal(existing, &installed); err != nil {
			return fail(fmt.Errorf("generation %s is already installed and its manifest cannot be decoded; staging was refused and the installed generation is unchanged", generation.ID))
		}
		installedBinding, installedErr := computeActivationBinding(installed, bindingFromStaged)
		incomingBinding, incomingErr := computeActivationBinding(generation, bindingFromBlobs(blobs))
		if installedErr != nil || incomingErr != nil || installed.ID != generation.ID || installedBinding != incomingBinding {
			return fail(fmt.Errorf("generation %s is already installed with different candidate evidence; immutable identifiers cannot be reused; the installed generation is unchanged", generation.ID))
		}
		if err := root.RemoveAll(genRel); err != nil {
			return fail(fmt.Errorf("remove prior unactivated candidate for generation %s: %s", generation.ID, sanitizeFSError(err)))
		}
	}
	if err := root.Rename(tmpRel, genRel); err != nil {
		return fail(fmt.Errorf("atomically rename staged generation %s: %s", generation.ID, sanitizeFSError(err)))
	}
	if err := fsyncRootDir(root, parentRel); err != nil {
		return indexformat.Generation{}, fmt.Errorf("store: fsync generation parent in Stage for session %s generation %s: %s; the staged generation exists but was not durably recorded; activation will be refused and retried", sessionID, generation.ID, sanitizeFSError(err))
	}
	if a.seam != nil {
		if err := a.seam("after-rename-before-db"); err != nil {
			return indexformat.Generation{}, err
		}
	}
	generation.Content = filled
	return generation, nil
}

func listStaleTempDirs(root *os.Root, parentRel string) []string {
	dir, err := root.Open(parentRel)
	if err != nil {
		return nil
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil
	}
	var stale []string
	for _, name := range names {
		if strings.HasPrefix(name, ".tmp-gen-") {
			stale = append(stale, path.Join(parentRel, name))
		}
	}
	return stale
}

func makeTempGenDir(root *os.Root, parentRel, generationID string) (string, error) {
	// Unique owned temporary name without host-tempfile path handling: the
	// directory lives under the owned parent so the atomic rename never
	// crosses filesystems and never escapes the root.
	for attempt := 0; attempt < 32; attempt++ {
		name := fmt.Sprintf(".tmp-gen-%s-%d", generationID, os.Getpid()*1000+attempt)
		// Sanitize: generationID is validated, but the temp name must also be
		// a single confined component.
		if strings.ContainsAny(name, `/\`) {
			return "", fmt.Errorf("temporary generation name is not confined")
		}
		rel := path.Join(parentRel, name)
		if err := root.Mkdir(rel, 0o700); err == nil {
			return rel, nil
		} else if !errors.Is(err, fs.ErrExist) {
			// Mkdir through Root reports existence without paths; any other
			// failure aborts.
			if _, statErr := root.Lstat(rel); statErr == nil {
				continue
			}
			return "", err
		}
	}
	return "", fmt.Errorf("temporary generation directory could not be created")
}

// reorderContent restores the caller's original content ordering after the
// deterministic write pass.
func reorderContent(filled []indexformat.ContentRecord, byRef map[schema.SourceEntryRef]int) []indexformat.ContentRecord {
	_ = byRef
	return filled
}

// activationBindingContent is one content record reduced to the evidence a
// candidate binding must carry: its ref and the integrity digest of its blob.
type activationBindingContent struct {
	Ref    schema.SourceEntryRef `json:"ref"`
	Digest string                `json:"digest"`
}

// activationBinding is the canonical, staging-independent encoding of a
// complete candidate. The whole generation manifest is included (every
// partition, segment, alias, title ref and metadata field), and the
// staging-derived blob path/length/digest fields are normalized out so the
// pre-stage candidate and the staged manifest produce the same binding.
type activationBinding struct {
	Generation indexformat.Generation     `json:"generation"`
	Content    []activationBindingContent `json:"content"`
}

// errGenerationContentBlobMissing marks a candidate content record whose
// captured blob is absent. The record reference comes from candidate data that
// may still be untrusted (an installed or staged manifest), so the diagnostic
// names the fixed category only and never the reference.
var errGenerationContentBlobMissing = errors.New("store: a captured content blob is missing; the generation is not self-contained; supply every captured blob")

// errGenerationContentDigestMissing marks a content record without an integrity
// digest. The reference may be untrusted manifest data, so the diagnostic names
// the fixed category only and never the reference.
var errGenerationContentDigestMissing = errors.New("store: a content record carries no integrity digest; the candidate binding cannot be verified; re-stage the generation")

// computeActivationBinding binds one activation envelope to the COMPLETE
// candidate. contentDigest supplies each content record's payload digest: the
// pre-stage caller hashes the blob bytes and the replay path reuses the
// manifest's verified integrity digest. It never uses a partial main-preview
// comparison, so two candidates that differ only outside the main preview still
// bind differently and an installed immutable generation is never rewritten
// from a partial equality test. A failure names a fixed category and never an
// untrusted record reference or manifest identifier.
func computeActivationBinding(generation indexformat.Generation, contentDigest func(indexformat.ContentRecord) (string, error)) (string, error) {
	normalized := generation
	normalized.Content = append([]indexformat.ContentRecord(nil), generation.Content...)
	content := make([]activationBindingContent, 0, len(normalized.Content))
	for i := range normalized.Content {
		record := normalized.Content[i]
		digest, err := contentDigest(record)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(digest) == "" {
			return "", errGenerationContentDigestMissing
		}
		content = append(content, activationBindingContent{Ref: record.Ref, Digest: digest})
		normalized.Content[i].RelativeBlob = ""
		normalized.Content[i].ByteLength = 0
		normalized.Content[i].Digest = ""
	}
	sort.Slice(content, func(i, j int) bool { return string(content[i].Ref) < string(content[j].Ref) })
	payload, err := json.Marshal(activationBinding{Generation: normalized, Content: content})
	if err != nil {
		return "", fmt.Errorf("store: encode activation binding in computeActivationBinding: %s; the candidate cannot be bound and must be re-staged", sanitizeFSError(err))
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// bindingFromBlobs hashes the captured blob bytes for a pre-stage candidate.
func bindingFromBlobs(blobs map[schema.SourceEntryRef][]byte) func(indexformat.ContentRecord) (string, error) {
	return func(record indexformat.ContentRecord) (string, error) {
		payload, ok := blobs[record.Ref]
		if !ok {
			return "", errGenerationContentBlobMissing
		}
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:]), nil
	}
}

// bindingFromStaged reuses the integrity digest already recorded in a staged
// manifest. Staging wrote each digest from the exact payload bytes, so it is
// the same evidence bindingFromBlobs computes before the rename.
func bindingFromStaged(record indexformat.ContentRecord) (string, error) {
	if strings.TrimSpace(record.Digest) == "" {
		return "", errGenerationContentDigestMissing
	}
	return record.Digest, nil
}

func (a *osGenerationArtifactStore) WriteIntent(ctx context.Context, intent GenerationIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateGenerationID(intent.GenerationID); err != nil {
		return err
	}
	sessionRel, err := a.sessionRel(intent.SessionID)
	if err != nil {
		return err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(sessionRel, 0o700); err != nil {
		return fmt.Errorf("store: create session artifact directory in WriteIntent for session %s: %s; no intent was recorded", intent.SessionID, sanitizeFSError(err))
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("store: encode activation intent in WriteIntent for session %s generation %s: %s; no intent was recorded", intent.SessionID, intent.GenerationID, sanitizeFSError(err))
	}
	return writeRootSyncedAtomic(root, path.Join(sessionRel, "generation-intent.json"), encoded, intent.SessionID, intent.GenerationID, "record the activation intent")
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

func (a *osGenerationArtifactStore) RepairMetadata(ctx context.Context, id schema.SessionID, metadata []byte) error {
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
	if err := root.MkdirAll(sessionRel, 0o700); err != nil {
		return fmt.Errorf("store: create session artifact directory in RepairMetadata for session %s: %s; the exported metadata was not repaired; the committed generation is unchanged and repair is retried on the next open or activation", id, sanitizeFSError(err))
	}
	return writeRootSyncedAtomic(root, path.Join(sessionRel, "metadata.json"), metadata, id, "", "repair the exported metadata")
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
	return fsyncRootDir(root, dir)
}

func fsyncRootDir(root *os.Root, rel string) error {
	dir, err := root.Open(rel)
	if err != nil {
		return fmt.Errorf("open directory to fsync in generation staging: %s; the staged file is not durably recorded; re-run the operation after fixing filesystem access", sanitizeFSError(err))
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fsync directory in generation staging: %s; the staged file is not durably recorded; re-run the operation after fixing filesystem access", sanitizeFSError(err))
	}
	return nil
}
