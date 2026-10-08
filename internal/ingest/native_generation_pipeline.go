package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// nativeGenerationIdentity assigns the installed identity of one managed
// generation candidate. Production mints a fresh opaque single-component
// identity per attempt so a failing capture never reuses a committed name; a
// deterministic caller injects a stable source through WithNativeGenerationID.
func (p *Pipeline) nativeGenerationIdentity(session DiscoveredSession) string {
	if p.nativeGenerationIDs != nil {
		return p.nativeGenerationIDs(session)
	}
	ref, err := randomOpaqueRef("g")
	if err != nil {
		// randomOpaqueRef only fails when the operating-system entropy source
		// fails; an empty identity is then refused by every consumer rather
		// than activating a generation under a reused name.
		return ""
	}
	return ref
}

// buildNativeCandidate composes the native generation path for one session. It
// reuses the harness indexer's own production candidate exit, wiring the
// pipeline-owned prior, metadata, snapshot and identity dependencies. The bool
// reports whether this harness has a native generation exit at all; when it is
// false the caller keeps the retained parse path. Activation happens after the
// parse, in the write path, so no format target is enabled here.
func (p *Pipeline) buildNativeCandidate(ctx context.Context, im indexedMeta, input *CapturedIndexInput, indexer TranscriptIndexer) (NativeGenerationCandidate, bool, error) {
	switch concrete := indexer.(type) {
	case *CodexIndexer:
		clone := *concrete
		clone.provenanceCapture = CodexProvenanceIndexerConfig{
			Enabled:      true,
			Prior:        p.codexPriorLoader(),
			GenerationID: p.nativeGenerationIdentity,
			Allocator:    RandomRefAllocator{},
		}
		candidate, err := clone.BuildNativeGeneration(ctx, input.session)
		return candidate, true, err
	case *OpenCodeIndexer:
		// A stored layout with no readable native snapshot must not enter the
		// native lane: its candidate cannot be produced, and failing here would
		// discard an index the ordinary adapter refresh can still serve. The
		// caller keeps the retained parse path for this session.
		if !nativeGenerationSessionSupported(input.session) {
			return NativeGenerationCandidate{}, false, nil
		}
		clone := *concrete
		clone.provenanceCapture = OpenCodeProvenanceIndexerConfig{
			Enabled:      true,
			Snapshot:     p.openCodeSnapshotLoader(input),
			Metadata:     p.openCodeMetadataLoader(input),
			GenerationID: p.nativeGenerationIdentity,
			Prior:        p.openCodePriorLoader(),
			Allocator:    RandomRefAllocator{},
		}
		candidate, err := clone.BuildNativeGeneration(ctx, input.session)
		return candidate, true, err
	default:
		return NativeGenerationCandidate{}, false, nil
	}
}

// priorReader returns the store-backed prior inventory, or nil when the
// configured store cannot answer it. A store without it produces no native
// candidate work rather than guessing an empty prior.
func (p *Pipeline) priorReader() NativeGenerationPriorReader {
	if p.metricsStore == nil {
		return nil
	}
	reader, _ := p.metricsStore.(NativeGenerationPriorReader)
	return reader
}

// codexPriorLoader supplies the last-good graph metadata and reusable alias
// state a Codex refresh must retain. A session with no active generation yields
// an empty prior, so a first discovery allocates fresh identities.
func (p *Pipeline) codexPriorLoader() func(DiscoveredSession) (*schema.UnifiedMetadata, ProjectionPriorState, error) {
	return func(session DiscoveredSession) (*schema.UnifiedMetadata, ProjectionPriorState, error) {
		reader := p.priorReader()
		if reader == nil {
			return nil, NewProjectionPriorState(), nil
		}
		prior, err := reader.ReadNativeGenerationPrior(context.Background(), session.SessionID)
		if err != nil {
			return nil, NewProjectionPriorState(), fmt.Errorf("load Codex prior evidence for session %s: %w; no candidate was produced and the last good generation stays active", session.SessionID, err)
		}
		if prior == nil {
			return nil, NewProjectionPriorState(), nil
		}
		state := prior.Aliases
		if state.Entries == nil {
			state = NewProjectionPriorState()
		}
		return prior.Metadata, state, nil
	}
}

// openCodePriorLoader supplies the alias state, retained captured prefix, and
// completeness a prior activation left. The persisted prior document carries
// the captured prefix; the committed generation rows remain the alias
// authority, so a document with stale aliases cannot rekey unchanged source.
func (p *Pipeline) openCodePriorLoader() func(context.Context, DiscoveredSession) (OpenCodeProvenancePrior, error) {
	return func(ctx context.Context, session DiscoveredSession) (OpenCodeProvenancePrior, error) {
		reader := p.priorReader()
		if reader == nil {
			return OpenCodeProvenancePrior{Aliases: NewProjectionPriorState()}, nil
		}
		prior, err := reader.ReadNativeGenerationPrior(ctx, session.SessionID)
		if err != nil {
			return OpenCodeProvenancePrior{}, fmt.Errorf("load OpenCode prior evidence for session %s: %w; no candidate was produced and the last good generation stays active", session.SessionID, err)
		}
		if prior == nil {
			return OpenCodeProvenancePrior{Aliases: NewProjectionPriorState()}, nil
		}
		loaded := OpenCodeProvenancePrior{Aliases: prior.Aliases, HasCompleteGeneration: prior.HasCompleteGeneration}
		if len(prior.PriorEvidence) > 0 {
			document, decodeErr := DecodeOpenCodeProvenancePrior(prior.PriorEvidence)
			if decodeErr != nil {
				return OpenCodeProvenancePrior{}, fmt.Errorf("reload OpenCode prior evidence for session %s: %w; no candidate was produced and the last good generation stays active", session.SessionID, decodeErr)
			}
			loaded = document
			loaded.Aliases = prior.Aliases
			loaded.HasCompleteGeneration = prior.HasCompleteGeneration
		}
		if loaded.Aliases.Entries == nil {
			loaded.Aliases = NewProjectionPriorState()
		}
		return loaded, nil
	}
}

// openCodeMetadataLoader reads the committed metadata of the session being
// refreshed. The native provenance snapshot needs the session's identity,
// harness and the native source locator the adapter recorded; the managed pair
// the index stage already validated is the one authority for it.
func (p *Pipeline) openCodeMetadataLoader(input *CapturedIndexInput) func(DiscoveredSession) (schema.UnifiedMetadata, error) {
	return func(session DiscoveredSession) (schema.UnifiedMetadata, error) {
		data := input.metadataData
		if len(data) == 0 {
			read, err := p.fs.ReadFile(input.metadataPath)
			if err != nil {
				return schema.UnifiedMetadata{}, fmt.Errorf("read committed metadata for the native OpenCode candidate of session %s: %w; no candidate was produced and the last good generation stays active", session.SessionID, err)
			}
			data = read
		}
		metadata, err := decodeManagedMetadata(data, input.metadataPath)
		if err != nil {
			return schema.UnifiedMetadata{}, fmt.Errorf("decode committed metadata for the native OpenCode candidate of session %s: %w; no candidate was produced and the last good generation stays active", session.SessionID, err)
		}
		if metadata.SessionID != session.SessionID {
			return schema.UnifiedMetadata{}, fmt.Errorf("committed metadata for session %s names session %s; no native candidate was produced and the stored generation is unchanged", session.SessionID, metadata.SessionID)
		}
		return *metadata, nil
	}
}

// openCodeSnapshotLoader snapshots the session's history through the configured
// OpenCode adapter. The native locator is the committed metadata's recorded
// source; a missing adapter, a non-OpenCode adapter, or an absent locator
// refuses the candidate instead of reading an unrelated path.
func (p *Pipeline) openCodeSnapshotLoader(input *CapturedIndexInput) func(context.Context, DiscoveredSession) (OpenCodeHistorySnapshot, error) {
	return func(ctx context.Context, session DiscoveredSession) (OpenCodeHistorySnapshot, error) {
		factory, ok := p.adapters[HarnessOpenCode]
		if !ok {
			return OpenCodeHistorySnapshot{}, fmt.Errorf("snapshot native OpenCode source for session %s: no OpenCode adapter is registered; no candidate was produced and the stored generation is unchanged; register the adapter before refreshing", session.SessionID)
		}
		adapter, ok := factory(p.fs, p.git, p.salt).(*OpenCodeAdapter)
		if !ok {
			return OpenCodeHistorySnapshot{}, fmt.Errorf("snapshot native OpenCode source for session %s: the registered adapter is not an OpenCode adapter; no candidate was produced and the stored generation is unchanged", session.SessionID)
		}
		metadata, err := p.openCodeMetadataLoader(input)(session)
		if err != nil {
			return OpenCodeHistorySnapshot{}, err
		}
		locator := metadata.Source.FilePath
		if locator == "" {
			return OpenCodeHistorySnapshot{}, fmt.Errorf("snapshot native OpenCode source for session %s: the committed metadata records no native source locator; no candidate was produced and the stored generation is unchanged; restore the recorded source and retry", session.SessionID)
		}
		return adapter.SnapshotOpenCodeProvenance(ctx, locator, session.SessionID.String(), OpenCodeSnapshotOptions{})
	}
}

// managedActivationCapture builds the publication-capture agreement a managed
// generation activation records for one session, or nil when this run cannot
// certify one.
//
// The snapshot is the session's RECORDED managed metadata: that is the evidence
// the ordinary write path publishes, and it is the metadata the installed
// generation indexes. A generation's own projection metadata is derived from it
// and may omit the local project, host and content identity the capture
// contract requires, so it is not the snapshot.
//
// The provenance kind comes from the one rule the retained write path uses, and
// the pipeline is what certifies it: the store records exactly what is supplied
// here and never derives a kind of its own. The whole agreement is then judged by
// the ONE capture rule the store enforces (ValidatePublicationCaptureSnapshot)
// before it is offered, so a recorded metadata snapshot that cannot be certified
// -- a missing or mismatched identity, a schema this build does not currently
// certify, or a missing integrity or content digest -- is not certified: the
// activation records no capture and leaves the stored provenance exactly as it
// was. Only a snapshot that passes the store's own-validity rule is ever
// supplied, so the activation is never refused for an uncertifiable snapshot; a
// capture that disagrees with the STORED session is still refused by the store,
// because only the store can compare it against the stored row.
func managedActivationCapture(input *CapturedIndexInput, session DiscoveredSession, completeness indexformat.GenerationCompleteness) *PublicationCaptureWrite {
	if input == nil || input.metadata == nil {
		return nil
	}
	if completeness != indexformat.GenerationCompletenessComplete {
		return nil
	}
	meta := *input.metadata
	if meta.SessionID != session.SessionID || meta.ModelHarness != session.Harness {
		return nil
	}
	kind := publicationCWDProvenance(&meta, session)
	if err := ValidatePublicationCaptureSnapshot(&meta, kind); err != nil {
		return nil
	}
	return &PublicationCaptureWrite{Metadata: meta, CWDProvenance: kind}
}

// preparedNativeGeneration is one native candidate whose activation envelope is
// final: its adapter stamp, capture assessment, expected state and prior
// evidence are resolved. staged is the store's inert prepared-files handle
// when the store implements NativeGenerationStager and preparation succeeded;
// it becomes activatable only through the guarded activation below.
type preparedNativeGeneration struct {
	result       indexParseResult
	activation   NativeGenerationActivation
	staged       NativeGenerationStaged
	entriesCount int
	candidates   []RetainedUnknownKindCount
}

// nativeGenerationCommit carries either one prepared candidate or the refusal
// outcome that replaces it. A refusal is fully reported during preparation, so
// the serial loop records it without touching the store again.
type nativeGenerationCommit struct {
	prepared preparedNativeGeneration
	ready    bool
	im       indexedMeta
	logEntry IndexLogEntry
	profile  IndexProfileSession
}

// nativeGenerationUnsupportedError is the single refusal for a store that
// cannot activate a managed generation.
func nativeGenerationUnsupportedError(logPrefix string, sessionID SessionID) error {
	return fmt.Errorf("%s: session %s produced a native managed generation but the store cannot activate one; the stored generation and producer stamps are unchanged; configure a store that supports managed-generation activation", logPrefix, sessionID)
}

// refuseNativeGeneration reports one refused native generation with the
// refusal, warning and log entry every native refusal path shares.
func (p *Pipeline) refuseNativeGeneration(result indexParseResult, entriesCount int, logPrefix string, err error) (indexedMeta, IndexLogEntry, IndexProfileSession) {
	im := result.im
	p.reportIndexRefusal(im.session.SessionID, err)
	slog.Warn(logPrefix+": store native generation", "session_id", im.session.SessionID, "error", err)
	errMsg := err.Error()
	logEntry := p.makeIndexLogEntry(im, IndexOutcomeError, entriesCount, result.startedAt, nil, &errMsg)
	return indexedMeta{session: im.session, startMs: im.startMs}, logEntry, p.makeIndexProfileSession(result, logEntry, 0)
}

// prepareNativeGenerationResult resolves one native candidate into its final
// activation envelope. It performs no file staging and no store transaction,
// so callers may prepare candidates concurrently. A refused candidate returns
// the same refusal outcome the serial activation would report.
func (p *Pipeline) prepareNativeGenerationResult(result indexParseResult, outcome IndexOutcome, logPrefix string) nativeGenerationCommit {
	im := result.im
	candidate := result.nativeCandidate
	entriesCount := len(candidate.Result.Generation.Main.Entries)
	if _, ok := p.metricsStore.(NativeGenerationActivator); !ok || result.input == nil {
		refused, logEntry, profile := p.refuseNativeGeneration(result, entriesCount, logPrefix, nativeGenerationUnsupportedError(logPrefix, im.session.SessionID))
		return nativeGenerationCommit{im: refused, logEntry: logEntry, profile: profile}
	}
	nowMs := time.Now().UnixMilli()
	var artifactIdentity *string
	if result.input.expected != nil && result.input.expected.ArtifactHash == nil {
		identity := result.input.artifactHash
		artifactIdentity = &identity
	}
	target := p.versionTargets()[im.session.Harness]
	generation := candidate.Result
	// Record the adapter revision that actually produced this generation. The
	// activation stamps it on the session row, so a later run sees the session
	// at target and leaves it alone instead of re-extracting it forever.
	if generation.Generation.Metadata.AdapterVersion == nil && target.AdapterVersion > 0 {
		version := target.AdapterVersion
		generation.Generation.Metadata.AdapterVersion = &version
	}
	// Native partitions are the selected history, not every source node seen by
	// replay. The one capture assessment owns the mapping from validated
	// evidence and parser coverage to the store write; native completeness is
	// carrier-independent and never depends on retained carrier count. Counts
	// stay candidates until the store outcome confirms CommittedNow.
	// Validate Main + Earlier as one set via AssessCapture, then publish
	// accounting only after the atomic generation write succeeds.
	sourceOmitted := result.input.session.ContentOmitted || outputRecordsItsOmissions(generation)
	unaccounted := result.input.session.ContentOmitted && !outputRecordsItsOmissions(generation)
	assessment, err := AssessCapture(CaptureFacts{
		Harness: im.session.Harness, Result: generation,
		Policy: CaptureFreshCandidate, Authoritative: true,
		SourceOmitted: sourceOmitted, Unaccounted: unaccounted,
	})
	if err != nil {
		refused, logEntry, profile := p.refuseNativeGeneration(result, entriesCount, logPrefix, err)
		return nativeGenerationCommit{im: refused, logEntry: logEntry, profile: profile}
	}
	capture, err := assessment.ContentCapture(contentAuthorityFor(result), im.session.TranscriptOrigin, nowMs)
	if err != nil {
		refused, logEntry, profile := p.refuseNativeGeneration(result, entriesCount, logPrefix, fmt.Errorf("%s: session %s assessment refused the store write; the stored index was preserved: %w", logPrefix, im.session.SessionID, err))
		return nativeGenerationCommit{im: refused, logEntry: logEntry, profile: profile}
	}
	candidates := assessment.CandidateCounts()
	// Operator-initiated native rebuilds opt out of the last-good refusal on
	// the same principle as entry rebuilds; ordinary activations stay guarded.
	explicit := p.config.Force || p.config.Reindex || outcome == IndexOutcomeReindexed
	activation := NativeGenerationActivation{
		Generation:       generation,
		Blobs:            candidate.Blobs,
		PriorEvidence:    candidate.PriorEvidence,
		IndexerVersion:   target.IndexerVersion,
		IndexedAtMs:      nowMs,
		ExpectedState:    result.input.expected,
		ContentCapture:   capture,
		CaptureRevision:  im.captureRevision,
		IndexedInputHash: &result.input.inputHash,
		ArtifactIdentity: artifactIdentity,
		ExplicitRebuild:  explicit,
		Capture:          managedActivationCapture(result.input, im.session, generation.Generation.Completeness),
	}
	return nativeGenerationCommit{
		prepared: preparedNativeGeneration{result: result, activation: activation, entriesCount: entriesCount, candidates: candidates},
		ready:    true,
	}
}

// activateNativeGenerationResult stages and activates one validated managed
// generation through the store's activation. It is the inline path for stores
// without pre-staging support; stores that implement NativeGenerationStager
// stage their files in the parallel pass, so the serialized commit below pays
// only for the database transaction.
func (p *Pipeline) activateNativeGenerationResult(ctx context.Context, result indexParseResult, outcome IndexOutcome, logPrefix string, writeLane *storeWriteLane) (indexedMeta, IndexLogEntry, IndexProfileSession) {
	commit := p.prepareAndStageNativeGeneration(ctx, result, outcome, logPrefix, p.nativeGenerationStager())
	if !commit.ready {
		return commit.im, commit.logEntry, commit.profile
	}
	return p.commitNativeGenerationResult(ctx, commit.prepared, outcome, logPrefix, writeLane)
}

// nativeGenerationStager returns the store's optional pre-staging support.
func (p *Pipeline) nativeGenerationStager() NativeGenerationStager {
	stager, _ := p.metricsStore.(NativeGenerationStager)
	return stager
}

// prepareAndStageNativeGeneration prepares one candidate and, when the store
// supports it, writes its files ahead of the serial commit. Preparation is
// best-effort and records nothing activatable: a failure is not reported here
// because the serial activation then stages inline and owns the authoritative
// refusal or repair outcome.
func (p *Pipeline) prepareAndStageNativeGeneration(ctx context.Context, result indexParseResult, outcome IndexOutcome, logPrefix string, stager NativeGenerationStager) nativeGenerationCommit {
	ctx = WithWriteAdvisoryReporter(ctx, p.reportDiagnostic)
	commit := p.prepareNativeGenerationResult(result, outcome, logPrefix)
	if !commit.ready || stager == nil {
		return commit
	}
	if staged, err := stager.StageNativeGeneration(ctx, commit.prepared.activation); err == nil {
		commit.prepared.staged = staged
	}
	return commit
}

// nativeGenerationCommitJob carries one prepared (or refused) candidate from a
// staging worker to the serial commit consumer.
type nativeGenerationCommitJob struct {
	position int
	commit   nativeGenerationCommit
}

// stageAndCommitNativeGenerations prepares and stages every native candidate
// in parallel, then commits each candidate through the serialized store writer
// lane as soon as its own staging completes. Committing as results arrive
// keeps a slow candidate from delaying the commits (and progress) of the
// others: the batch finishes when its slowest staging plus the commits behind
// it finish, instead of when the sum of staging and commits does. onCommit
// runs once per candidate, in completion order, on the calling goroutine.
func (p *Pipeline) stageAndCommitNativeGenerations(
	ctx context.Context,
	results []indexParseResult,
	positions []int,
	outcome IndexOutcome,
	logPrefix string,
	writeLane *storeWriteLane,
	onCommit func(position int, indexed indexedMeta, logEntry IndexLogEntry, profile IndexProfileSession),
) {
	if len(positions) == 0 {
		return
	}
	if stager, ok := p.metricsStore.(NativeGenerationBatchStager); ok {
		if activator, ok := p.metricsStore.(NativeGenerationBatchActivator); ok {
			p.stageAndCommitNativeBatches(ctx, results, positions, outcome, logPrefix, writeLane, onCommit, stager, activator)
			return
		}
	}
	stager := p.nativeGenerationStager()
	workers := 1
	if stager != nil {
		workers = min(len(positions), parallelWorkers(p.config))
	}
	jobs := make(chan nativeGenerationCommitJob, workers)

	// Producers: prepare and stage candidates with bounded parallelism, then
	// hand each one to the commit consumer in completion order.
	var producers sync.WaitGroup
	producers.Add(1)
	go func() {
		defer producers.Done()
		defer close(jobs)
		sem := make(chan struct{}, workers)
		var staging sync.WaitGroup
		for i := range positions {
			sem <- struct{}{}
			staging.Add(1)
			go func(i int) {
				defer staging.Done()
				defer func() { <-sem }()
				jobs <- nativeGenerationCommitJob{
					position: positions[i],
					commit:   p.prepareAndStageNativeGeneration(ctx, results[positions[i]], outcome, logPrefix, stager),
				}
			}(i)
		}
		staging.Wait()
	}()

	// Consumer: commit each candidate as it arrives. runStoreWrite serializes
	// the commits through the writer lane; staging continues meanwhile.
	for job := range jobs {
		commit := job.commit
		var indexed indexedMeta
		var logEntry IndexLogEntry
		var profile IndexProfileSession
		if commit.ready {
			indexed, logEntry, profile = p.commitNativeGenerationResult(ctx, commit.prepared, outcome, logPrefix, writeLane)
		} else {
			indexed, logEntry, profile = commit.im, commit.logEntry, commit.profile
		}
		if onCommit != nil {
			onCommit(job.position, indexed, logEntry, profile)
		}
	}
	producers.Wait()
}

// commitNativeGenerationResult commits one prepared native candidate. The
// current-input check runs first, so a refused candidate's prepared files are
// dropped with no activation intent ever written. When the store prepared the
// files, the activation installs them, so the writer lane pays only for the
// intent, rename and database transaction.
func (p *Pipeline) commitNativeGenerationResult(ctx context.Context, prepared preparedNativeGeneration, outcome IndexOutcome, logPrefix string, writeLane *storeWriteLane) (indexedMeta, IndexLogEntry, IndexProfileSession) {
	ctx = WithWriteAdvisoryReporter(ctx, p.reportDiagnostic)
	result := prepared.result
	im := result.im
	entriesCount := prepared.entriesCount
	fail := func(err error) (indexedMeta, IndexLogEntry, IndexProfileSession) {
		return p.refuseNativeGeneration(result, entriesCount, logPrefix, err)
	}
	activator, ok := p.metricsStore.(NativeGenerationActivator)
	if !ok || result.input == nil {
		return fail(nativeGenerationUnsupportedError(logPrefix, im.session.SessionID))
	}
	var activationOutcome ActivationOutcome
	var activationErr error
	p.runStoreWrite(writeLane, func() {
		activationErr = p.withCurrentIndexInput(ctx, result.input, func() error {
			var err error
			if staged := prepared.staged; staged != nil {
				if preparedActivator, ok := p.metricsStore.(NativeGenerationPreparedActivator); ok {
					activationOutcome, err = preparedActivator.ActivateStagedNativeGeneration(ctx, prepared.activation, staged)
					return err
				}
			}
			activationOutcome, err = activator.ActivateNativeGeneration(ctx, prepared.activation)
			return err
		})
	})
	return p.finishNativeGenerationResult(ctx, prepared, outcome, logPrefix, activationOutcome, activationErr)
}

// finishNativeGenerationResult is shared by single and batched activation so
// refusal, retained accounting, skip, and post-commit sweep have one owner.
func (p *Pipeline) finishNativeGenerationResult(ctx context.Context, prepared preparedNativeGeneration, outcome IndexOutcome, logPrefix string, activationOutcome ActivationOutcome, activationErr error) (indexedMeta, IndexLogEntry, IndexProfileSession) {
	result := prepared.result
	im := result.im
	entriesCount := prepared.entriesCount
	candidates := prepared.candidates
	fail := func(err error) (indexedMeta, IndexLogEntry, IndexProfileSession) {
		return p.refuseNativeGeneration(result, entriesCount, logPrefix, err)
	}
	if activationErr != nil {
		var repairPending *GenerationRepairPendingError
		if errors.As(activationErr, &repairPending) {
			// Post-commit repair failure: authority already committed, never
			// rollback. CommittedNow counts once, AlreadyCommitted zero.
			// Report repair pending honestly, not as a capture refusal, and
			// never report duplicate successful retained occurrences.
			if activationOutcome.Disposition == ActivationCommittedNow {
				slog.Warn(logPrefix+": native generation committed with pending repair", "session_id", im.session.SessionID, "candidate", activationOutcome.CandidateID, "error", activationErr)
				logEntry := p.makeIndexLogEntry(im, outcome, entriesCount, result.startedAt, nil, nil)
				profile := p.makeIndexProfileSession(result, logEntry, 0)
				return indexedMeta{session: im.session, startMs: im.startMs, indexed: true, retainedUnknown: candidates}, logEntry, profile
			}
			if activationOutcome.Disposition == ActivationAlreadyCommitted {
				slog.Warn(logPrefix+": already-committed native generation repair pending", "session_id", im.session.SessionID, "candidate", activationOutcome.CandidateID, "error", activationErr)
				logEntry := p.makeIndexLogEntry(im, outcome, entriesCount, result.startedAt, nil, nil)
				profile := p.makeIndexProfileSession(result, logEntry, 0)
				return indexedMeta{session: im.session, startMs: im.startMs, indexed: true}, logEntry, profile
			}
		}
		return fail(fmt.Errorf("%s: activate managed generation for session %s: %w; the stored generation and producer stamps are unchanged", logPrefix, im.session.SessionID, activationErr))
	}
	// Per-invocation counting, never crash-global exactly-once: only
	// CommittedNow counts once. AlreadyCommitted is an idempotent repair with
	// zero new counts, not a failed capture. NotCommitted carries no counts
	// by construction (activationErr would be non-nil above).
	//
	// The commit is durable in both committing dispositions (or was already
	// durable for the idempotent retry), so the per-session sweep runs for
	// either: the superseded rows and orphans the staging flagged are
	// deleted and the flag is cleared. A sweep failure never fails the
	// session; the next harvest recovers it.
	if activationOutcome.Disposition == ActivationCommittedNow ||
		activationOutcome.Disposition == ActivationAlreadyCommitted {
		p.sweepCommittedSession(ctx, im.session.SessionID, logPrefix)
	}
	var committed []RetainedUnknownKindCount
	if activationOutcome.Disposition == ActivationCommittedNow {
		committed = candidates
	}
	if activationOutcome.Disposition == ActivationSkipped {
		// An identical refresh: the projection is unchanged, so the
		// bookkeeping advanced and nothing else did. The outcome reports
		// skipped with the fixed reason; per-invocation counts stay zero.
		reason := "projection unchanged"
		logEntry := p.makeIndexLogEntry(im, IndexOutcomeSkipped, entriesCount, result.startedAt, &reason, nil)
		profile := p.makeIndexProfileSession(result, logEntry, 0)
		return indexedMeta{session: im.session, startMs: im.startMs, indexed: true}, logEntry, profile
	}
	logEntry := p.makeIndexLogEntry(im, outcome, entriesCount, result.startedAt, nil, nil)
	profile := p.makeIndexProfileSession(result, logEntry, 0)
	return indexedMeta{session: im.session, startMs: im.startMs, indexed: true, retainedUnknown: committed}, logEntry, profile
}
