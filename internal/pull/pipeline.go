package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// --- Dependency interfaces (declared CONSUMER-SIDE per the project DI pattern) ---

// VillageReader is the narrow slice of the internal/village client the pull
// pipeline consumes. It is declared here, not in internal/village, so the
// pipeline owns its dependency contract and *village.VillageClient satisfies it
// structurally. NegotiatePull is the EXPLICIT NEGOTIATE stage — the pipeline
// calls it EXACTLY ONCE per run, before the FETCH stages; the four GETs are pure
// data calls that do NOT re-preflight the pull window.
type VillageReader interface {
	NegotiatePull(ctx context.Context) error
	ListPullableTranscripts(ctx context.Context, page, limit int) (*schema.PullListResponse, error)
	GetPullTranscript(ctx context.Context, id schema.TranscriptID) (*schema.PullTranscriptInfo, error)
	GetPullTranscriptContent(ctx context.Context, id schema.TranscriptID, ifNoneMatch string) (io.ReadCloser, string, error)
	GetPullTranscriptAnnotations(ctx context.Context, id schema.TranscriptID) ([]schema.PullAnnotation, error)
}

// Compile-time guard: the production client must satisfy the narrow reader the
// pipeline depends on. A signature drift in either breaks the build here.
var _ VillageReader = (*village.VillageClient)(nil)

// PullStore is the V34 persistence surface the pull pipeline consumes. It
// is declared HERE (consumer-side) referencing the store-package row shapes;
// *store.Store implements it. All transcript writes go through CommitPull in a
// SINGLE SQLite transaction; the annotation-refresh path uses
// UpsertPulledAnnotations.
type PullStore interface {
	CommitPull(ctx context.Context, commit store.PullCommit) error
	UpsertPulledAnnotations(ctx context.Context, annotations []store.PulledAnnotationRow) (created, updated, skipped int, err error)
	ListPulledTranscripts(ctx context.Context) ([]store.PulledTranscriptRow, error)
	GetPulledTranscript(ctx context.Context, villageHost string, id schema.TranscriptID) (*store.PulledTranscriptRow, error)
}

// Compile-time guard: *store.Store must satisfy the consumer-side PullStore.
var _ PullStore = (*store.Store)(nil)

// Clock abstracts the wall clock so the pipeline's timestamps (pulled_at,
// manifest pulledAt) are deterministic in tests.
type Clock interface {
	NowUnixMilli() int64
}

// Credentials is the narrow slice of auth.Credentials the pipeline needs:
// VillageURL locates the village (and yields the on-disk host namespace + the
// manifest's villageURL), and UserID drives the own-author exclusion. Declared
// consumer-side to avoid importing the auth package's full type here. An empty
// UserID (or VillageURL) means "not logged in" — the AUTH-CHECK stage fails fast.
type Credentials struct {
	UserID     string
	VillageURL string
}

// IsLoggedIn reports whether the credentials carry the minimum identity the
// village-contacting pull commands require.
func (c Credentials) IsLoggedIn() bool {
	return c.UserID != "" && c.VillageURL != ""
}

// --- Pipeline ---

// Pipeline is the system under test: it orchestrates the staged pull flow over
// injected dependencies (a VillageReader, an ingest.FileSystem, a PullStore, a
// Clock, and the requester Credentials). It is constructed with real
// dependencies in production (the *village.VillageClient, *ingest.OSFileSystem,
// *store.Store) and with MemFS + a stub reader in tests — the Pipeline itself is
// never mocked.
type Pipeline struct {
	reader    VillageReader
	fs        ingest.FileSystem
	store     PullStore
	clock     Clock
	creds     Credentials
	pullsRoot string // resolved village-pulls/ root (defaults.VillagePullsDirPath)
}

// NewPipeline constructs a pull Pipeline from its dependencies. pullsRoot is the
// resolved village-pulls/ root directory (callers pass
// defaults.ResolveVillagePullsDirPath()/...With(override) so the path honors the
// XDG/--data-dir resolution).
func NewPipeline(reader VillageReader, fs ingest.FileSystem, st PullStore, clock Clock, creds Credentials, pullsRoot string) *Pipeline {
	return &Pipeline{
		reader:    reader,
		fs:        fs,
		store:     st,
		clock:     clock,
		creds:     creds,
		pullsRoot: pullsRoot,
	}
}

// villageHost returns the on-disk namespace key for the village (the URL host).
// Falls back to the raw VillageURL when it does not parse, so a malformed but
// non-empty URL still produces a stable directory rather than panicking.
func (p *Pipeline) villageHost() string {
	if u, err := url.Parse(p.creds.VillageURL); err == nil && u.Host != "" {
		return u.Host
	}
	return p.creds.VillageURL
}

// pullDir returns the on-disk directory a transcript lands in:
// {pullsRoot}/{villageHost}/{transcriptId}.
func (p *Pipeline) pullDir(host string, id schema.TranscriptID) string {
	return filepath.Join(p.pullsRoot, host, id.String())
}

// tempDir returns the staging directory a transcript is downloaded into before
// the copy+remove publish: {pullsRoot}/{villageHost}/.tmp-{transcriptId}.
func (p *Pipeline) tempDir(host string, id schema.TranscriptID) string {
	return filepath.Join(p.pullsRoot, host, defaults.TempDirPrefix+id.String())
}

// classifyStatus maps a non-nil pipeline/client error to the corresponding PullStatus
// via the village error sentinels (errors.Is). Success statuses (pulled /
// up-to-date) are reported directly by the caller, never through here — every call
// site invokes classifyStatus only on a non-nil error path, so a nil error is a
// programmer error and panics explicitly rather than masquerading as a success
// status. Used for every village-contacting failure so the CLI keys its exit code
// off a typed status, never a string match.
func classifyStatus(err error) PullStatus {
	switch {
	case err == nil:
		panic("pull.classifyStatus called with a nil error: success statuses must be reported directly by the caller")
	case errors.Is(err, village.ErrPullNotFound):
		return PullStatusNotFound
	case errors.Is(err, village.ErrPullContractIncompatible):
		return PullStatusContractError
	default:
		return PullStatusError
	}
}

// PullTranscript runs the staged pull pipeline for a single transcript:
//
//	RESOLVE → AUTH-CHECK → NEGOTIATE → FETCH-META → DIFF → [DryRun short-circuit] →
//	DOWNLOAD (temp) → FETCH-ANNOTATIONS (memory) → WRITE (stage+rename) →
//	DB-TX (CommitPull) → REPORT
//
// Error doctrine (inverted fail-open): any failure in RESOLVE…FETCH-ANNOTATIONS
// leaves ZERO local mutation (the temp dir, if created, is removed). A DB-TX
// failure AFTER the publish triggers a compensating fs.RemoveAll(pullDir); if that
// compensation itself fails the returned error names the orphan directory and
// instructs `--force` to repair (the V34 tables are a derived index of the on-disk
// manifests). An error is NEVER reported as up-to-date.
//
// KNOWN LIMITATION (mirrors ingest M11): the WRITE "publish" is a copy+remove
// (renameDir = recursive MkdirAll + per-file CopyFile + RemoveAll(src)), NOT an OS
// atomic rename. A crash mid-publish leaves a manifest-less partial pull dir with
// no DB row; the derived-index doctrine + `--force` repair cover that window.
//
// COMPENSATION CAVEAT: the compensating RemoveAll(pullDir) restores the exact
// pre-pull state only for a FIRST pull (no prior dir). On a re-pull-REPLACE the
// prior good copy was already removed before the rename, so a DB-TX failure leaves
// NO local copy while the DB still holds the OLD (stale) row — not a true restore.
// Under the cache doctrine (foreign pulled data is a re-pullable derived index)
// this is acceptable: `--force` re-downloads and overwrites the stale row. We do
// not save-old→backup; the wording above is exact for first pulls only.
func (p *Pipeline) PullTranscript(ctx context.Context, ref TranscriptRef, opts PullOptions) (*PullResult, error) {
	// --- AUTH-CHECK (RESOLVE already done by ParseTranscriptRef at the CLI) ---
	if !p.creds.IsLoggedIn() {
		return &PullResult{Ref: ref, Status: PullStatusNotLoggedIn}, fmt.Errorf(
			"peasant village pull: not logged in\n" +
				"  what: no valid village credentials are present\n" +
				"  why:  pulling a transcript contacts the village and requires authentication\n" +
				"  fix:  run `peasant village login`, then re-run the pull")
	}
	host := p.villageHost()

	// --- NEGOTIATE (exactly once, before the FETCH stages) ---
	if err := p.reader.NegotiatePull(ctx); err != nil {
		return &PullResult{Ref: ref, Status: classifyStatus(err), VillageHost: host}, err
	}

	// --- FETCH-META ---
	meta, err := p.reader.GetPullTranscript(ctx, ref.ID)
	if err != nil {
		return &PullResult{Ref: ref, Status: classifyStatus(err), VillageHost: host}, err
	}

	pullDir := p.pullDir(host, ref.ID)

	// --- DIFF (manifest RAW hash vs server RAW hash) ---
	// Load the existing local manifest (if any). The stored ServedBlobHash is the
	// RAW (unquoted) content-identity hash; meta.ContentHash from the village is
	// also RAW — so this fast-path compares RAW-vs-RAW. A non-empty match (and not
	// --force) ⇒ up-to-date without re-download. When either hash is empty we
	// cannot decide here and let the conditional GET (304) or a post-download
	// compare settle it. The stored ETag (verbatim, possibly quoted) is the
	// SEPARATE conditional-GET token echoed below.
	storedManifest, hasStored := p.storedManifest(pullDir)
	storedHash := ""
	storedETag := ""
	if hasStored {
		storedHash = storedManifest.ServedBlobHash
		storedETag = storedManifest.ServedETag
	}
	if !opts.Force && storedHash != "" && meta.ContentHash != "" && storedHash == meta.ContentHash {
		return &PullResult{
			Ref:            ref,
			Status:         PullStatusUpToDate,
			VillageHost:    host,
			PullDir:        pullDir,
			ServedBlobHash: storedHash,
			DryRun:         opts.DryRun,
			License:        meta.License,
		}, nil
	}

	// --- DRY-RUN SHORT-CIRCUIT (before any DOWNLOAD/WRITE/DB-TX) ---
	// At this point RESOLVE → AUTH-CHECK → NEGOTIATE → FETCH-META → DIFF have run
	// with zero local mutation. DryRun reports what WOULD happen: the metadata
	// fast-path above already returned up-to-date; reaching here means a re-pull
	// WOULD download + write. Report PullStatusPulled (would-pull) without any
	// network DOWNLOAD, file write, or DB-TX.
	if opts.DryRun {
		return &PullResult{
			Ref:            ref,
			Status:         PullStatusPulled,
			VillageHost:    host,
			PullDir:        pullDir,
			ServedBlobHash: meta.ContentHash, // best-known would-be hash (raw; may be empty)
			DryRun:         true,
			License:        meta.License,
		}, nil
	}

	// --- DOWNLOAD (to a temp dir) ---
	// Conditional GET: when not forcing and we hold a stored ETag, echo it VERBATIM
	// as If-None-Match so the server can match byte-for-byte and answer 304 (the
	// second DIFF path). The ETag is the transport token; the raw hash is for
	// content identity only.
	ifNoneMatch := ""
	if !opts.Force {
		ifNoneMatch = storedETag
	}
	body, etag, err := p.reader.GetPullTranscriptContent(ctx, ref.ID, ifNoneMatch)
	if err != nil {
		if errors.Is(err, village.ErrNotModified) {
			// 304: the served blob matches our stored copy — up-to-date, no write.
			return &PullResult{
				Ref:            ref,
				Status:         PullStatusUpToDate,
				VillageHost:    host,
				PullDir:        pullDir,
				ServedBlobHash: storedHash,
				License:        meta.License,
			}, nil
		}
		return &PullResult{Ref: ref, Status: classifyStatus(err), VillageHost: host}, err
	}
	blob, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil {
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, fmt.Errorf(
			"peasant village pull: read served blob for transcript %s @ %s: %w", ref.ID, host, readErr)
	}
	if closeErr != nil {
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, fmt.Errorf(
			"peasant village pull: close served-blob stream for transcript %s @ %s: %w", ref.ID, host, closeErr)
	}

	// Served-blob identity hash (RAW) vs ETag (verbatim transport token).
	// The village quotes its ETag (fmt.Sprintf("%q", hash)); strip the surrounding
	// quotes to derive the RAW content-identity hash so it compares cleanly against
	// meta.ContentHash and is persisted consistently. When the village served no
	// ETag, recompute the raw hash locally over the blob bytes. servedETag keeps
	// the verbatim token for the re-pull If-None-Match echo (empty when none).
	servedETag := etag
	servedHash := strings.Trim(etag, `"`)
	if servedHash == "" {
		servedHash = schema.ComputeTranscriptHash(blob)
	}

	// Post-download local-hash DIFF fallback: when the server advertised no hash
	// (no ETag, no metadata ContentHash) but we already hold a byte-identical copy
	// with the same recomputed hash, this is still up-to-date — avoid a needless
	// rewrite. Only applies when not forcing.
	if !opts.Force && storedHash != "" && servedHash == storedHash {
		return &PullResult{
			Ref:            ref,
			Status:         PullStatusUpToDate,
			VillageHost:    host,
			PullDir:        pullDir,
			ServedBlobHash: servedHash,
			License:        meta.License,
		}, nil
	}

	// --- FETCH-ANNOTATIONS (to memory) ---
	annotations, err := p.reader.GetPullTranscriptAnnotations(ctx, ref.ID)
	if err != nil {
		return &PullResult{Ref: ref, Status: classifyStatus(err), VillageHost: host}, err
	}
	if err := validatePulledAnnotations(annotations); err != nil {
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, err
	}

	now := p.clock.NowUnixMilli()

	// Build the manifest + the metadata snapshot + the annotation provenance.
	manifest := PullManifest{
		ManifestVersion:     PullManifestVersion,
		VillageURL:          p.creds.VillageURL,
		VillageHost:         host,
		TranscriptID:        ref.ID,
		LocalSessionID:      meta.LocalID,
		OwnerUserID:         meta.OwnerUserID,
		OwnerUsername:       meta.OwnerUsername,
		ServedETag:          servedETag,
		ServedBlobHash:      servedHash,
		BlobContractVersion: meta.ContractVersion,
		PullEnvelopeVersion: defaults.PullContractVersion,
		PulledAt:            now,
		Annotations:         manifestAnnotations(annotations),
	}

	// --- WRITE (stage into temp, then publish via copy+remove) ---
	// NOTE: this "publish" is NOT atomic (see PullTranscript doc — copy+remove, not
	// an OS rename); a crash mid-publish is repaired by --force per the derived-
	// index doctrine.
	tmpDir := p.tempDir(host, ref.ID)
	// Clear any stale temp dir from a crashed prior run before staging.
	if err := p.fs.RemoveAll(tmpDir); err != nil {
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, fmt.Errorf(
			"peasant village pull: clear stale temp dir %s: %w", tmpDir, err)
	}
	if err := p.stageFiles(tmpDir, blob, meta, manifest); err != nil {
		_ = p.fs.RemoveAll(tmpDir) // pre-WRITE failure ⇒ zero local mutation
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, err
	}

	// Publish: remove any existing pullDir then move temp into place (copy+remove,
	// NOT atomic). A re-pull-replace destroys the prior copy here BEFORE the move
	// completes; under the derived-index/cache doctrine this is acceptable — a
	// crash or DB-TX failure is repaired by --force (which re-downloads).
	if err := p.fs.RemoveAll(pullDir); err != nil {
		_ = p.fs.RemoveAll(tmpDir)
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, fmt.Errorf(
			"peasant village pull: clear existing pull dir %s before rename: %w", pullDir, err)
	}
	if err := p.renameDir(tmpDir, pullDir); err != nil {
		_ = p.fs.RemoveAll(tmpDir)
		_ = p.fs.RemoveAll(pullDir)
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, fmt.Errorf(
			"peasant village pull: atomic rename %s -> %s: %w", tmpDir, pullDir, err)
	}

	// --- DB-TX (single SQLite transaction: pulled_transcripts + pulled_annotations) ---
	commit := p.buildCommit(host, pullDir, servedHash, now, meta, annotations)
	if err := p.store.CommitPull(ctx, commit); err != nil {
		// Compensating removal: the publish succeeded but the DB-TX failed. Remove
		// the freshly-published pull dir. This restores the exact pre-pull state for
		// a FIRST pull; for a re-pull-REPLACE the prior copy is already gone, so this
		// leaves no local copy + a stale DB row — recoverable via --force (cache
		// doctrine). If THAT removal fails, name the orphan + instruct --force
		// (C-actionable-errors).
		if rmErr := p.fs.RemoveAll(pullDir); rmErr != nil {
			return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host, PullDir: pullDir}, fmt.Errorf(
				"peasant village pull: DB commit failed AND compensating cleanup failed — local state is inconsistent\n"+
					"  what: the transcript blob was written to %s but the database commit failed, and removing the directory also failed\n"+
					"  why:  DB error: %v; cleanup error: %v\n"+
					"  where: DB-TX stage, transcript %s @ %s\n"+
					"  fix:  the directory %s is an orphan (no DB row); re-run with `peasant village transcripts pull --force` to repair, or remove the directory manually",
				pullDir, err, rmErr, ref.ID, host, pullDir)
		}
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, fmt.Errorf(
			"peasant village pull: DB commit failed (local files rolled back) for transcript %s @ %s: %w", ref.ID, host, err)
	}

	// --- REPORT ---
	return &PullResult{
		Ref:             ref,
		Status:          PullStatusPulled,
		VillageHost:     host,
		PullDir:         pullDir,
		ServedBlobHash:  servedHash,
		AnnotationCount: len(annotations),
		License:         meta.License,
	}, nil
}

// validatePulledAnnotations applies the schema's canonical response target
// validator to the complete fetched batch before the pull writes files or DB
// rows. This makes an invalid association arm an all-or-nothing boundary
// failure rather than a malformed payload that contaminates the local cache.
func validatePulledAnnotations(annotations []schema.PullAnnotation) error {
	for index := range annotations {
		if annotations[index].TargetKind != schema.TargetAssociation {
			continue
		}
		if err := annotations[index].AnnotationSummary.Validate(); err != nil {
			return fmt.Errorf("peasant village pull: validate annotation[%d] at the published schema boundary before local mutation: %w", index, err)
		}
	}
	return nil
}

// storedManifest reads and decodes an existing pull manifest in pullDir. It
// returns (manifest, true) on success, or (zero, false) when no manifest exists /
// cannot be decoded (treated as "no prior pull" — the DIFF then falls through to
// a download).
func (p *Pipeline) storedManifest(pullDir string) (PullManifest, bool) {
	data, err := p.fs.ReadFile(filepath.Join(pullDir, ManifestFilename))
	if err != nil {
		return PullManifest{}, false
	}
	var m PullManifest
	if json.Unmarshal(data, &m) != nil {
		return PullManifest{}, false
	}
	return m, true
}

// stageFiles writes the transcript blob, the village metadata snapshot, and the
// pull manifest into the staging temp dir. Any error leaves the caller to remove
// the temp dir (zero local mutation outside it).
func (p *Pipeline) stageFiles(tmpDir string, blob []byte, meta *schema.PullTranscriptInfo, manifest PullManifest) error {
	if err := p.fs.MkdirAll(tmpDir, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf("peasant village pull: create staging dir %s: %w", tmpDir, err)
	}
	if err := p.fs.WriteFile(filepath.Join(tmpDir, TranscriptFilename), blob, defaults.PrivateFilePerm); err != nil {
		return fmt.Errorf("peasant village pull: write transcript blob: %w", err)
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("peasant village pull: marshal metadata snapshot: %w", err)
	}
	if err := p.fs.WriteFile(filepath.Join(tmpDir, MetadataFilename), metaJSON, defaults.PrivateFilePerm); err != nil {
		return fmt.Errorf("peasant village pull: write metadata snapshot: %w", err)
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("peasant village pull: marshal pull manifest: %w", err)
	}
	if err := p.fs.WriteFile(filepath.Join(tmpDir, ManifestFilename), manifestJSON, defaults.PrivateFilePerm); err != nil {
		return fmt.Errorf("peasant village pull: write pull manifest: %w", err)
	}
	return nil
}

// buildCommit assembles the atomic PullCommit (one transcript row + its
// annotation rows) for CommitPull.
func (p *Pipeline) buildCommit(host, pullDir, servedHash string, now int64, meta *schema.PullTranscriptInfo, annotations []schema.PullAnnotation) store.PullCommit {
	rows := make([]store.PulledAnnotationRow, 0, len(annotations))
	for i := range annotations {
		rows = append(rows, p.annotationRow(host, now, meta, &annotations[i]))
	}
	return store.PullCommit{
		Transcript: store.PulledTranscriptRow{
			VillageHost:     host,
			TranscriptID:    meta.TranscriptID,
			OwnerUserID:     meta.OwnerUserID,
			OwnerUsername:   meta.OwnerUsername,
			LocalSessionID:  meta.LocalID,
			Title:           meta.Title,
			Harness:         meta.Harness,
			ProjectName:     meta.ProjectName,
			ContentHash:     servedHash,
			Visibility:      meta.Visibility,
			License:         meta.License,
			PullDir:         pullDir,
			FirstPulledAt:   now,
			LastPulledAt:    now,
			AnnotationCount: len(annotations),
		},
		Annotations: rows,
	}
}

// annotationRow projects a wire PullAnnotation into a V34 pulled_annotations row.
// The dedup key is the annotation's content-hash; the full PullAnnotation JSON is
// stored as the payload.
func (p *Pipeline) annotationRow(host string, now int64, meta *schema.PullTranscriptInfo, a *schema.PullAnnotation) store.PulledAnnotationRow {
	payload, _ := json.Marshal(a)
	return store.PulledAnnotationRow{
		VillageHost:    host,
		ContentHash:    annotationContentHash(a),
		TranscriptID:   meta.TranscriptID,
		LocalSessionID: meta.LocalID,
		AuthorUserID:   a.AuthorUserID,
		AuthorUsername: a.AuthorUsername,
		Payload:        string(payload),
		PulledAt:       now,
	}
}

// annotationContentHash returns the dedup key for a pulled annotation: the V16
// push content-hash carried by the embedded AnnotationSummary. When the village
// omitted it, fall back to a recomputed SHA3-256 over the annotation's stable
// identity (id) so a row still has a non-empty, stable key (the embedded ID is
// the village-side annotation primary key).
func annotationContentHash(a *schema.PullAnnotation) string {
	if a.ContentHash != nil && *a.ContentHash != "" {
		return *a.ContentHash
	}
	// Fallback dedup key: schema.ComputeTranscriptHash is a GENERIC SHA3-256 over
	// arbitrary bytes (despite the transcript-specific name); reusing it to hash
	// the annotation's stable ID yields a non-empty, deterministic key with the
	// same algorithm the rest of the pull path uses. Not a transcript hash — just a
	// SHA3-256 of the annotation identity.
	return schema.ComputeTranscriptHash([]byte(a.ID))
}

// manifestAnnotations projects the wire annotations into per-annotation manifest
// provenance entries (content-hash + author identity).
func manifestAnnotations(annotations []schema.PullAnnotation) []ManifestAnnotation {
	out := make([]ManifestAnnotation, 0, len(annotations))
	for i := range annotations {
		a := &annotations[i]
		out = append(out, ManifestAnnotation{
			ContentHash:    annotationContentHash(a),
			AuthorUserID:   a.AuthorUserID,
			AuthorUsername: a.AuthorUsername,
		})
	}
	return out
}

// renameDir moves a directory tree from src to dst via the FileSystem interface.
// Mirrors ingest.Pipeline.renameDir: MemFS.Rename only moves the directory node,
// not its contents, so we recursively copy then remove. For OSFileSystem this is
// a portable recursive move (production could use os.Rename directly, but the
// portable path keeps one code path across FS implementations).
func (p *Pipeline) renameDir(src, dst string) error {
	if err := p.fs.MkdirAll(dst, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf("renameDir: mkdir %s: %w", dst, err)
	}
	walkErr := p.fs.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == src {
			return nil
		}
		rel := path[len(src)+1:]
		dstPath := filepath.Join(dst, rel)
		if d.IsDir() {
			return p.fs.MkdirAll(dstPath, defaults.PrivateDirPerm)
		}
		if err := p.fs.CopyFile(path, dstPath, defaults.PrivateFilePerm); err != nil {
			return fmt.Errorf("renameDir copy %s -> %s: %w", path, dstPath, err)
		}
		return nil
	})
	if walkErr != nil {
		return errors.Join(walkErr, p.fs.RemoveAll(dst))
	}
	return p.fs.RemoveAll(src)
}
