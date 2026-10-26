package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	NegotiatePull(ctx context.Context) (schema.PushContractVersion, error)
	ListPullableTranscripts(ctx context.Context, opts village.PullListOptions) (*schema.PullListResponse, error)
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
// dependencies in production (the *village.VillageClient, *FileSystem,
// *store.Store) and with MemFS + a stub reader in tests — the Pipeline itself is
// never mocked.
type Pipeline struct {
	reader              VillageReader
	fs                  ingest.FileSystem
	store               PullStore
	clock               Clock
	creds               Credentials
	pullsRoot           string // resolved village-pulls/ root (defaults.VillagePullsDirPath)
	lockInstall         func(context.Context, string, schema.TranscriptID) (func() error, error)
	syncInstall         func(string) error
	associationObserver AssociationObserver
}

// NewPipeline constructs a pull Pipeline from its dependencies. pullsRoot is the
// resolved village-pulls/ root directory (callers pass
// defaults.ResolveVillagePullsDirPath()/...With(override) so the path honors the
// XDG/--data-dir resolution).
func NewPipeline(reader VillageReader, fs ingest.FileSystem, st PullStore, clock Clock, creds Credentials, pullsRoot string, options ...PipelineOption) *Pipeline {
	p := &Pipeline{
		reader:      reader,
		fs:          fs,
		store:       st,
		clock:       clock,
		creds:       creds,
		pullsRoot:   pullsRoot,
		lockInstall: lockContentInstall,
		syncInstall: syncContentPath,
	}
	if syncing, ok := fs.(interface{ Sync(string) error }); ok {
		p.syncInstall = syncing.Sync
	}
	for _, option := range options {
		option(p)
	}
	return p
}

// PipelineOption injects a dependency into the pull pipeline.
type PipelineOption func(*Pipeline)

// AssociationObserver is the optional metadata dependency, invoked only after
// independently authorized content success. The command adapter binds it to the
// original action account through the canonical observation service.
type AssociationObserver interface {
	ObserveAssociations(context.Context, schema.TranscriptID) (schema.PullAssociationMetadataResult, error)
}

func WithAssociationObserver(observer AssociationObserver) PipelineOption {
	return func(p *Pipeline) { p.associationObserver = observer }
}

// WithInstallLock supplies filesystem ownership for an injected filesystem.
// Production defaults to the existing cross-process OS advisory locker.
func WithInstallLock(lock func(context.Context, string, schema.TranscriptID) (func() error, error)) PipelineOption {
	return func(p *Pipeline) { p.lockInstall = lock }
}

// WithInstallSync supplies persistence barriers for an injected filesystem.
func WithInstallSync(sync func(string) error) PipelineOption {
	return func(p *Pipeline) { p.syncInstall = sync }
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
// Publication uses real filesystem renames, retaining a previous complete copy
// until index projection succeeds. An interrupted install is validated and
// reprojected under cross-process ownership on the next non-dry invocation.
// A database failure reports retained files and repair, never false rollback.
func (p *Pipeline) PullTranscript(ctx context.Context, ref TranscriptRef, opts PullOptions) (result *PullResult, pullErr error) {
	// Register before filesystem ownership: its release runs first, so optional
	// network metadata never holds the content publication lock.
	defer func() {
		if pullErr == nil {
			p.ObservePulledAssociations(ctx, result)
		}
	}()
	// --- AUTH-CHECK (RESOLVE already done by ParseTranscriptRef at the CLI) ---
	if !p.creds.IsLoggedIn() {
		return &PullResult{Ref: ref, Status: PullStatusNotLoggedIn}, fmt.Errorf(
			"peasant village pull: not logged in\n" +
				"  what: no valid peasant online credentials are present\n" +
				"  why:  pulling a transcript contacts peasant online and requires authentication\n" +
				"  fix:  run `peasant village login`, then re-run the pull")
	}
	host := p.villageHost()

	// --- NEGOTIATE (once per command; a bulk caller sets SkipNegotiate after its
	// own single NegotiatePull) ---
	envelope := opts.EnvelopeVersion
	if !opts.SkipNegotiate {
		negotiated, err := p.reader.NegotiatePull(ctx)
		if err != nil {
			return &PullResult{Ref: ref, Status: classifyStatus(err), VillageHost: host}, err
		}
		envelope = negotiated
	} else if opts.EnvelopeVersion != "" {
		// The caller negotiated on its own client: tell this reader which
		// envelope to decode, so it refuses the other shape.
		if reader, ok := p.reader.(interface {
			SetPullEnvelope(schema.PushContractVersion)
		}); ok {
			reader.SetPullEnvelope(opts.EnvelopeVersion)
		}
	}
	if envelope == "" {
		envelope = defaults.PullContractVersion
	}

	pullDir := p.pullDir(host, ref.ID)
	if !opts.DryRun {
		if err := p.fs.MkdirAll(filepath.Dir(pullDir), defaults.PrivateDirPerm); err != nil {
			return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, installError(err)
		}
		release, err := p.lockInstall(ctx, filepath.Dir(pullDir), ref.ID)
		if err != nil {
			return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, err
		}
		defer release()
		if err := p.syncInstallAncestors(filepath.Dir(pullDir)); err != nil {
			return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, installError(err)
		}
	}

	// --- FETCH-META (skipped when the caller passed the listing's own row) ---
	meta := opts.Meta
	if meta == nil {
		fetched, err := p.reader.GetPullTranscript(ctx, ref.ID)
		if err != nil {
			return &PullResult{Ref: ref, Status: classifyStatus(err), VillageHost: host}, err
		}
		meta = fetched
	}
	meta = readerMeta(meta)

	if !opts.DryRun {
		if err := p.recoverInstall(ctx, host, ref.ID, opts.Force); err != nil {
			return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host, PullDir: pullDir}, err
		}
	}

	// --- DIFF (manifest RAW hash vs server RAW hash) ---
	// Load the existing local manifest (if any). The stored ServedBlobHash is the
	// RAW (unquoted) content-identity hash; meta.ContentHash from the village is
	// also RAW — so this fast-path compares RAW-vs-RAW. A non-empty match (and not
	// --force) ⇒ up-to-date without re-download. When either hash is empty we
	// cannot decide here and let the conditional GET (304) or a post-download
	// compare settle it. The stored ETag (verbatim, possibly quoted) is the
	// SEPARATE conditional-GET token echoed below.
	storedManifest, hasStored := p.storedManifest(pullDir)
	if hasStored {
		_, err := p.contentDigest(pullDir, host, ref.ID)
		hasStored = err == nil
	}
	storedHash := ""
	storedETag := ""
	if hasStored {
		storedHash = storedManifest.ServedBlobHash
		storedETag = storedManifest.ServedETag
	}
	// A stored copy whose people the village now shows differently is re-pulled
	// in full instead of taking a metadata-only update, so the manifest and the
	// annotation provenance are rewritten and keep no identity the village no
	// longer shows (and pick up a renamed username):
	//   - the owner changed kind, account or username;
	//   - an annotation author stored as a profile is now shown differently.
	//     Only a fresh annotation read can tell, so a stored copy that names any
	//     author re-reads its annotations here; the read is reused below.
	refresh := opts.Force || (hasStored && storedOwnerChanged(storedManifest, meta.Owner))
	var annotations []schema.PullAnnotation
	annotationsRead := false
	if !refresh && hasStored && storedHasProfileAuthor(storedManifest) {
		read, status, err := p.readAnnotations(ctx, ref.ID, meta.Owner)
		if err != nil {
			return &PullResult{Ref: ref, Status: status, VillageHost: host}, err
		}
		annotations, annotationsRead = read, true
		refresh = storedAuthorsChanged(storedManifest, manifestAnnotations(annotations))
	}
	if !refresh &&
		storedHash != "" && meta.ContentHash != "" &&
		storedHash == meta.ContentHash {
		if err := p.updateMetadata(ctx, host, ref.ID, meta, opts.DryRun); err != nil {
			return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host, PullDir: pullDir}, err
		}
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
	if !refresh {
		ifNoneMatch = storedETag
	}
	body, etag, err := p.reader.GetPullTranscriptContent(ctx, ref.ID, ifNoneMatch)
	if err != nil {
		if errors.Is(err, village.ErrNotModified) {
			if !hasStored {
				return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host}, fmt.Errorf("peasant village pull: content endpoint returned 304 without a validated local file set; no content success can be reported; retry with --force")
			}
			if err := p.updateMetadata(ctx, host, ref.ID, meta, false); err != nil {
				return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host, PullDir: pullDir}, err
			}
			// 304: retain blob/provenance; changed metadata was projected above.
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
	if !refresh && storedHash != "" && servedHash == storedHash {
		if err := p.updateMetadata(ctx, host, ref.ID, meta, false); err != nil {
			return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host, PullDir: pullDir}, err
		}
		return &PullResult{
			Ref:            ref,
			Status:         PullStatusUpToDate,
			VillageHost:    host,
			PullDir:        pullDir,
			ServedBlobHash: servedHash,
			License:        meta.License,
		}, nil
	}

	// --- FETCH-ANNOTATIONS (to memory; reused when the DIFF already read them) ---
	if !annotationsRead {
		read, status, err := p.readAnnotations(ctx, ref.ID, meta.Owner)
		if err != nil {
			return &PullResult{Ref: ref, Status: status, VillageHost: host}, err
		}
		annotations = read
	}

	now := p.clock.NowUnixMilli()

	// Build the manifest + the metadata snapshot + the annotation provenance.
	ownerID, ownerUsername := publicPersonColumns(meta.Owner)
	manifest := PullManifest{
		ManifestVersion:     PullManifestVersion,
		VillageURL:          p.creds.VillageURL,
		VillageHost:         host,
		TranscriptID:        ref.ID,
		LocalSessionID:      meta.LocalID,
		OwnerUserID:         ownerID,
		OwnerUsername:       ownerUsername,
		ServedETag:          servedETag,
		ServedBlobHash:      servedHash,
		BlobContractVersion: meta.ContractVersion,
		PullEnvelopeVersion: envelope,
		PulledAt:            now,
		Annotations:         manifestAnnotations(annotations),
	}

	commit := p.buildCommit(host, pullDir, servedHash, now, meta, annotations)
	if err := p.installContent(ctx, host, ref.ID, blob, meta, manifest, commit, opts.Force, false); err != nil {
		return &PullResult{Ref: ref, Status: PullStatusError, VillageHost: host, PullDir: pullDir}, err
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

// ObservePulledAssociations completes independently authorized successful
// content, including the bulk skip-gate path that never calls PullTranscript.
// Optional source/persistence failure is carried only in the shared metadata
// result. Required content errors remain the responsibility of the caller.
func (p *Pipeline) ObservePulledAssociations(ctx context.Context, result *PullResult) {
	if p.associationObserver == nil || result == nil || result.DryRun {
		return
	}
	if result.Status != PullStatusPulled && result.Status != PullStatusUpToDate {
		return
	}
	metadata, err := p.associationObserver.ObserveAssociations(ctx, result.Ref.ID)
	if invalid := metadata.Validate(); invalid != nil || (err != nil && metadata.Warning == nil) {
		metadata = schema.PullAssociationMetadataResult{State: schema.PullAssociationMetadataUnknown, Warning: &schema.OperationIssue{
			Code: schema.OperationIssueContract, Operation: schema.CollectiveOperationPull,
			Message: "The optional association source returned an invalid result after the content pull succeeded; no fresh metadata can be reported.",
			Retry:   "Run peasant village collectives fetch explicitly after checking the selected account and peasant online connection.",
		}}
	}
	result.AssociationMetadata = &metadata
}

// readAnnotations fetches a transcript's annotations, validates the batch at
// the schema boundary, and returns them as this CLI may show them. On failure
// it returns the status the caller reports.
func (p *Pipeline) readAnnotations(ctx context.Context, id schema.TranscriptID, owner schema.VillagePublicOwner) ([]schema.PullAnnotation, PullStatus, error) {
	annotations, err := p.reader.GetPullTranscriptAnnotations(ctx, id)
	if err != nil {
		return nil, classifyStatus(err), err
	}
	if err := validatePulledAnnotations(annotations); err != nil {
		return nil, PullStatusError, err
	}
	return readerAnnotations(annotations, owner), "", nil
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
	ownerID, ownerUsername := publicPersonColumns(meta.Owner)
	return store.PullCommit{
		Transcript: store.PulledTranscriptRow{
			VillageHost:     host,
			TranscriptID:    meta.TranscriptID,
			OwnerUserID:     ownerID,
			OwnerUsername:   ownerUsername,
			LocalSessionID:  meta.LocalID,
			Title:           meta.Title,
			Harness:         meta.Harness,
			ProjectName:     projectNameColumn(meta.ProjectName),
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
	authorID, authorUsername := publicPersonColumns(a.Author)
	return store.PulledAnnotationRow{
		VillageHost:    host,
		ContentHash:    annotationContentHash(a),
		TranscriptID:   meta.TranscriptID,
		LocalSessionID: meta.LocalID,
		AuthorUserID:   authorID,
		AuthorUsername: authorUsername,
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
		authorID, authorUsername := publicPersonColumns(a.Author)
		out = append(out, ManifestAnnotation{
			ContentHash:    annotationContentHash(a),
			AuthorUserID:   authorID,
			AuthorUsername: authorUsername,
		})
	}
	return out
}
