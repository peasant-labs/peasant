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
	"sync"
	"sync/atomic"

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

// GenerationArtifactStore owns the file half of the crash protocol. The
// production implementation is root-confined through os.Root and fsyncs every
// file and directory before the generation directory is atomically renamed
// into place. Tests substitute a failing implementation to interrupt a chosen
// seam.
type GenerationArtifactStore interface {
	// Stage writes the generation's content blobs and manifest under an owned,
	// root-confined generation directory, fsyncs the manifest and the
	// directory, and atomically renames the complete directory into place. It
	// returns the generation with its ContentRecord relative paths, byte
	// lengths and integrity digests filled in. Content blobs are not
	// individually synced; every binding that trusts a staged candidate
	// re-reads them and verifies their digests, so a torn or lost write
	// refuses recovery instead of activating.
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
	// WritePriorEvidence writes the opaque activation-owned prior document for
	// one staged generation. It is written after the generation directory is
	// renamed into place and is fsynced, so a reopen reads the same identities.
	WritePriorEvidence(context.Context, schema.SessionID, string, []byte) error
	// ReadPriorEvidence returns the persisted prior document for one
	// generation, or (nil, nil) when none was written.
	ReadPriorEvidence(context.Context, schema.SessionID, string) ([]byte, error)
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

func blobName(ref schema.SourceEntryRef) string {
	sum := sha256.Sum256([]byte("peasant.generation.blob.v1\x00" + string(ref)))
	return "c_" + hex.EncodeToString(sum[:]) + ".blob"
}

func (a *osGenerationArtifactStore) Stage(ctx context.Context, generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte) (indexformat.Generation, error) {
	files, err := a.stageTemp(ctx, generation, blobs)
	if err != nil {
		return indexformat.Generation{}, err
	}
	return a.install(ctx, files, blobs)
}

// stagedGenerationFiles is one fully written and fsynced candidate that still
// lives in its owned temporary directory. It is not installed: no generation
// directory carries its identifier until install renames it into place.
type stagedGenerationFiles struct {
	generation indexformat.Generation
	tmpRel     string
}

// stageTemp is the file-only staging phase: it creates a unique owned
// temporary directory, writes every content blob, fsyncs the manifest, and
// fsyncs the temporary directory. Content blobs are not individually synced;
// every binding that trusts a staged candidate re-reads them and verifies
// their digests. It performs no identity check and no rename, so the result is
// invisible to activation and recovery until install runs. A failure removes
// the temporary directory.
func (a *osGenerationArtifactStore) stageTemp(ctx context.Context, generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte) (stagedGenerationFiles, error) {
	sessionID := generation.Metadata.SessionID
	if err := ctx.Err(); err != nil {
		return stagedGenerationFiles{}, err
	}
	if err := validateGenerationID(generation.ID); err != nil {
		return stagedGenerationFiles{}, err
	}
	sessionRel, err := a.sessionRel(sessionID)
	if err != nil {
		return stagedGenerationFiles{}, err
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return stagedGenerationFiles{}, err
	}
	defer root.Close()
	parentRel := path.Join(sessionRel, "generations")
	if err := root.MkdirAll(parentRel, 0o700); err != nil {
		return stagedGenerationFiles{}, fmt.Errorf("store: create generation parent in Stage for session %s generation %s: %s; no generation was staged", sessionID, generation.ID, sanitizeFSError(err))
	}
	// A previous interrupted or abandoned staging may have left an owned
	// temporary directory. Staging is serialized by the exclusive session
	// lock, so no live writer owns one; removing stale temp candidates here
	// cannot touch any installed generation. Listing runs through the owned
	// root so a namespace symlink cannot redirect the scan.
	if stale := listStaleTempDirs(root, parentRel); stale != nil {
		for _, dir := range stale {
			_ = root.RemoveAll(dir)
		}
	}
	tmpRel, err := makeTempGenDir(root, parentRel, generation.ID)
	if err != nil {
		return stagedGenerationFiles{}, fmt.Errorf("store: create temporary generation directory in Stage for session %s generation %s: %s; no generation was staged", sessionID, generation.ID, sanitizeFSError(err))
	}
	fail := func(cause error) (stagedGenerationFiles, error) {
		_ = root.RemoveAll(tmpRel)
		return stagedGenerationFiles{}, fmt.Errorf("store: stage generation %s for session %s in Stage: %s; the temporary candidate was removed and the active generation is unchanged", generation.ID, sessionID, sanitizeFSError(cause))
	}
	filled := append([]indexformat.ContentRecord(nil), generation.Content...)
	refs := make([]int, 0, len(filled))
	for i := range filled {
		refs = append(refs, i)
	}
	sort.Slice(refs, func(i, j int) bool { return string(filled[refs[i]].Ref) < string(filled[refs[j]].Ref) })
	if err := a.writeContentBlobs(ctx, root, tmpRel, filled, refs, blobs); err != nil {
		return fail(err)
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
	return stagedGenerationFiles{generation: generation, tmpRel: tmpRel}, nil
}

// install is the publishing phase: it verifies the immutable candidate
// identity against any already-installed directory with the same identifier,
// atomically renames the temporary directory into place, and fsyncs the
// parent. A failure before the rename removes the temporary directory.
func (a *osGenerationArtifactStore) install(ctx context.Context, files stagedGenerationFiles, blobs map[schema.SourceEntryRef][]byte) (indexformat.Generation, error) {
	generation := files.generation
	sessionID := generation.Metadata.SessionID
	sessionRel, genRel, err := a.generationRel(sessionID, generation.ID)
	if err != nil {
		return indexformat.Generation{}, err
	}
	parentRel := path.Join(sessionRel, "generations")
	tmpRel := files.tmpRel
	if path.Dir(tmpRel) != parentRel || !strings.HasPrefix(path.Base(tmpRel), ".tmp-gen-") {
		return indexformat.Generation{}, fmt.Errorf("store: install generation %s for session %s: the prepared temporary directory is not owned by this session; nothing was installed and the active generation is unchanged; prepare the candidate again", generation.ID, sessionID)
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return indexformat.Generation{}, err
	}
	defer root.Close()
	fail := func(cause error) (indexformat.Generation, error) {
		_ = root.RemoveAll(tmpRel)
		return indexformat.Generation{}, fmt.Errorf("store: stage generation %s for session %s in Stage: %s; the temporary candidate was removed and the active generation is unchanged", generation.ID, sessionID, sanitizeFSError(cause))
	}
	if err := ctx.Err(); err != nil {
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
		installedBinding, installedErr := computeActivationBinding(installed, verifiedBlobDigest(ctx, a, sessionID, generation.ID))
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
	return generation, nil
}

// discardTemp removes one prepared temporary directory that will never be
// installed. It is best-effort: the next staging for the session removes any
// leftover temporary directory.
func (a *osGenerationArtifactStore) discardTemp(files stagedGenerationFiles) {
	if !strings.HasPrefix(path.Base(files.tmpRel), ".tmp-gen-") {
		return
	}
	root, err := a.openOwnedRoot()
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.RemoveAll(files.tmpRel)
}

// writeContentBlobs writes every content blob of one staged generation. A
// single configured worker keeps the historical serial order; multiple workers
// overlap the writes across the ref-sorted records. The crash protocol is
// unchanged in ordering: the manifest, the directory fsync and the atomic
// rename still happen only after every blob write completes, and every later
// binding re-reads and verifies the blob bytes, so a torn or lost write
// refuses recovery instead of activating. A failed write cancels the
// remaining writers so fail() can remove the temp directory without a live
// writer inside it. Concurrent writers own distinct records.
func (a *osGenerationArtifactStore) writeContentBlobs(ctx context.Context, root *os.Root, tmpRel string, filled []indexformat.ContentRecord, refs []int, blobs map[schema.SourceEntryRef][]byte) error {
	workers := a.blobWorkers
	if workers < 1 {
		workers = 1
	}
	workers = min(workers, len(refs))
	if workers <= 1 {
		for _, i := range refs {
			if err := a.writeContentBlob(ctx, root, tmpRel, &filled[i], blobs); err != nil {
				return err
			}
		}
		return nil
	}
	stageCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		next     atomic.Int64
	)
	record := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				pos := int(next.Add(1)) - 1
				if pos >= len(refs) {
					return
				}
				if err := a.writeContentBlob(stageCtx, root, tmpRel, &filled[refs[pos]], blobs); err != nil {
					record(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// writeContentBlob writes one content blob and records its managed relative
// path, byte length and payload digest. The write is deliberately not synced:
// the activation intent, the manifest and the rename carry the crash protocol,
// and every binding that trusts a staged candidate re-reads each blob through
// ReadBlob, which verifies its length and digest. The store-wide slot pool
// bounds how many blob writes are in flight across all staging sessions.
func (a *osGenerationArtifactStore) writeContentBlob(ctx context.Context, root *os.Root, tmpRel string, record *indexformat.ContentRecord, blobs map[schema.SourceEntryRef][]byte) error {
	ref := record.Ref
	payload, ok := blobs[ref]
	if !ok {
		return fmt.Errorf("content blob for ref %q is missing; the generation is not self-contained; supply every captured blob", ref)
	}
	if a.blobSlots != nil {
		select {
		case a.blobSlots <- struct{}{}:
			defer func() { <-a.blobSlots }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	name := blobName(ref)
	if err := writeRootFile(root, path.Join(tmpRel, name), payload); err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	record.RelativeBlob = name
	record.ByteLength = int64(len(payload))
	record.Digest = hex.EncodeToString(digest[:])
	return nil
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

// errGenerationContentBlobUnverified marks a staged candidate whose blob could
// not be read and verified against the manifest. The record comes from a
// manifest that may still be untrusted, so the diagnostic names the fixed
// category only and never the reference or a path it carries.
var errGenerationContentBlobUnverified = errors.New("store: a staged content blob could not be read and verified; the generation is not self-contained; re-stage the candidate")

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

// verifiedBlobDigest returns each record's blob digest as read through
// reader.ReadBlob, which verifies the blob's length and integrity digest
// against the manifest. A binding computed through it therefore never trusts a
// manifest digest whose bytes are missing or torn: a damaged staged candidate
// refuses recovery or an identity check instead of being activated under
// digests only the manifest still carries.
//
// A failed read returns the fixed, reference-free category: the record comes
// from a manifest that may still be untrusted, so the diagnostic never echoes
// the reference or a path it carries. Context cancellation is preserved.
func verifiedBlobDigest(ctx context.Context, reader GenerationArtifactStore, sessionID schema.SessionID, generationID string) func(indexformat.ContentRecord) (string, error) {
	return func(record indexformat.ContentRecord) (string, error) {
		data, err := reader.ReadBlob(ctx, sessionID, generationID, record)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			return "", errGenerationContentBlobUnverified
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:]), nil
	}
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

// WritePriorEvidence writes the activation-owned prior document inside one
// already-staged generation directory. The write is atomic and fsynced, so a
// reopen reads the exact document the activation committed. An empty document
// is refused rather than silently deleted: absence is expressed by not calling
// this method.
func (a *osGenerationArtifactStore) WritePriorEvidence(ctx context.Context, id schema.SessionID, generationID string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("store: prior evidence for session %s generation %s is empty; nothing was written; omit the document to leave prior evidence absent", id, generationID)
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
	if err := writeRootSyncedAtomic(root, path.Join(genRel, priorEvidenceName), data, id, generationID, "prior evidence"); err != nil {
		return fmt.Errorf("store: persist prior evidence for generation %s of session %s: %w; the committed generation is unchanged and the prior is reloaded from its committed rows", generationID, id, err)
	}
	return nil
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
