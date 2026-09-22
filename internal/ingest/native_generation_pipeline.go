package ingest

import (
	"context"
	"fmt"
	"log/slog"
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

// activateNativeGenerationResult stages and activates one validated managed
// generation through the store's activation. It reuses the captured expected
// state so a changed stored row refuses the replacement, stamps the producing
// revision together with the generation, and never claims a success stamp for
// an incomplete candidate.
func (p *Pipeline) activateNativeGenerationResult(ctx context.Context, result indexParseResult, outcome IndexOutcome, logPrefix string, writeLane *storeWriteLane) (indexedMeta, IndexLogEntry, IndexProfileSession) {
	im := result.im
	candidate := result.nativeCandidate
	entriesCount := len(candidate.Result.Generation.Main.Entries)
	fail := func(err error) (indexedMeta, IndexLogEntry, IndexProfileSession) {
		p.reportIndexRefusal(im.session.SessionID, err)
		slog.Warn(logPrefix+": store native generation", "session_id", im.session.SessionID, "error", err)
		errMsg := err.Error()
		logEntry := p.makeIndexLogEntry(im, IndexOutcomeError, entriesCount, result.startedAt, nil, &errMsg)
		return indexedMeta{session: im.session, startMs: im.startMs}, logEntry, p.makeIndexProfileSession(result, logEntry, 0)
	}
	activator, ok := p.metricsStore.(NativeGenerationActivator)
	if !ok || result.input == nil {
		return fail(fmt.Errorf("%s: session %s produced a native managed generation but the store cannot activate one; the stored generation and producer stamps are unchanged; configure a store that supports managed-generation activation", logPrefix, im.session.SessionID))
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
	// replay. Validate and count their evidence before activation, then publish
	// accounting only after the atomic generation write succeeds.
	selected := append([]schema.SessionEntry(nil), generation.Generation.Main.Entries...)
	for _, earlier := range generation.Generation.Earlier {
		selected = append(selected, earlier.Content.Entries...)
	}
	unknown, err := retainedUnknownEntries(selected)
	if err != nil {
		return fail(err)
	}
	capture := SessionContentCaptureWrite{Status: ContentCaptureComplete, SourceAuthority: contentAuthorityFor(result), TranscriptOrigin: im.session.TranscriptOrigin, CaptureFormat: ContentCaptureFormatFull, CapturedAtMs: nowMs}
	if len(unknown) > 0 {
		if _, err := ProjectRetainedUnknown(selected, im.session.Harness); err != nil {
			return fail(err)
		}
		capture.Status, capture.FailureCode = ContentCaptureIncomplete, ContentCaptureUnknownDataRetained
		capture.FailureMessage = "source payloads and positions are retained; interpretation remains partial"
		if result.input.session.ContentOmitted || generation.Generation.Completeness != indexformat.GenerationCompletenessComplete {
			capture.CaptureFormat, capture.FailureCode = ContentCaptureFormatPreviewOnly, ContentCaptureSourceRecordsOmitted
			capture.FailureMessage = "opaque evidence survives, but source capture or native history reconstruction is incomplete; recapture the complete native source"
		}
	}
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
		Capture:          managedActivationCapture(result.input, im.session, generation.Generation.Completeness),
	}
	var activationErr error
	p.runStoreWrite(writeLane, func() {
		activationErr = p.withCurrentIndexInput(ctx, result.input, func() error {
			return activator.ActivateNativeGeneration(ctx, activation)
		})
	})
	if activationErr != nil {
		return fail(fmt.Errorf("%s: activate managed generation for session %s: %w; the stored generation and producer stamps are unchanged", logPrefix, im.session.SessionID, activationErr))
	}
	logEntry := p.makeIndexLogEntry(im, outcome, entriesCount, result.startedAt, nil, nil)
	profile := p.makeIndexProfileSession(result, logEntry, 0)
	return indexedMeta{session: im.session, startMs: im.startMs, indexed: true, retainedUnknown: retainedUnknownCounts(unknown)}, logEntry, profile
}
