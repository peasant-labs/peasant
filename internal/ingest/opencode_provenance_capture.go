package ingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// DefaultOpenCodeProvenanceBudgetBytes bounds one provenance snapshot read.
// OpenCode rows have no per-record size limit, so the bound covers the whole
// snapshot instead of one record; exceeding it fails the refresh and retains
// the last good generation rather than certifying a truncated capture.
const DefaultOpenCodeProvenanceBudgetBytes = 256 << 20

// OpenCodeSnapshotOptions carries the caller-supplied evidence a snapshot read
// cannot prove from message rows alone: the fork anchor, the revert state, and
// the per-input subagent delivery correlation. Every field is explicit native
// evidence or nil; the reader never infers them from timestamps or text.
type OpenCodeSnapshotOptions struct {
	Fork              *OpenCodeForkProof
	Revert            *OpenCodeRevertState
	AgentDeliveredIDs map[string]bool
	// Copied optionally supplies already-captured source rows. It exists for a
	// caller that holds the source rows from the same read view. When it is
	// empty and Fork names a source session, the reader copies that session's
	// current rows itself; a supplied set is never merged with a native read.
	Copied []OpenCodeHistoryRow
	// BudgetBytes caps the snapshot payload bytes. Zero selects the default.
	BudgetBytes int64
}

// SnapshotOpenCodeHistory reads one read-only OpenCode history snapshot over
// the existing source seam: the session row for parentage plus the current
// message rows in sequence order. With a fork proof it also copies the settled
// source rows through the same read-only source, so the captured prefix is
// self-contained and a later parent deletion cannot blank the child. It issues
// SELECTs only and never writes to the source. Rows decode through the
// provenance row decoder, so the snapshot admits exactly what the retained
// normalizer admits; newer control rows surface the shared skip sentinel and are
// counted, not failed.
func SnapshotOpenCodeHistory(ctx context.Context, source OpenCodeSQLiteSource, sessionID string, opts OpenCodeSnapshotOptions) (OpenCodeHistorySnapshot, error) {
	if strings.TrimSpace(sessionID) == "" {
		return OpenCodeHistorySnapshot{}, &OpenCodeSnapshotError{SessionID: sessionID, Step: "validate snapshot request", Reason: "session identity is empty", Recovery: "snapshot a concrete native session id"}
	}
	linkID, err := NewOpenCodeSessionLinkID(sessionID)
	if err != nil {
		return OpenCodeHistorySnapshot{}, &OpenCodeSnapshotError{SessionID: sessionID, Step: "validate snapshot request", Reason: fmt.Sprintf("session identity is invalid: %v", err), Recovery: "snapshot a concrete native session id"}
	}
	currentID, err := NewOpenCodeCurrentSessionID(sessionID)
	if err != nil {
		return OpenCodeHistorySnapshot{}, &OpenCodeSnapshotError{SessionID: sessionID, Step: "validate snapshot request", Reason: fmt.Sprintf("session identity is not a current session: %v", err), Recovery: "snapshot a session the current session_message table carries"}
	}
	budget := opts.BudgetBytes
	if budget <= 0 {
		budget = DefaultOpenCodeProvenanceBudgetBytes
	}
	snapshot := OpenCodeHistorySnapshot{
		SessionID:         sessionID,
		Fork:              opts.Fork,
		Revert:            opts.Revert,
		AgentDeliveredIDs: opts.AgentDeliveredIDs,
	}
	scope := OpenCodeProvenanceScope{SessionID: sessionID, Shape: OpenCodeProvenanceCurrent}
	if err := fillOpenCodeSnapshotParent(ctx, source, linkID, &snapshot, &scope); err != nil {
		return OpenCodeHistorySnapshot{}, err
	}
	var spent int64
	own, err := readOpenCodeCurrentRows(ctx, source, currentID, scope, budget, &spent, nil)
	if err != nil {
		return OpenCodeHistorySnapshot{}, err
	}
	snapshot.Messages = own
	if err := fillOpenCodeSnapshotForkCopies(ctx, source, opts, budget, &spent, &snapshot); err != nil {
		return OpenCodeHistorySnapshot{}, err
	}
	snapshot.SourceEvidenceDigest = DigestOpenCodeSnapshot(snapshot)
	return snapshot, nil
}

// fillOpenCodeSnapshotForkCopies captures the settled source rows a fork proof
// needs. A caller-supplied set from the same read view is recorded as given; a
// fork source with no supplied set is read from the same read-only source, so
// deleting the parent rows later cannot blank the child generation. Each copied
// row keeps a new child identity and records the source row and session it was
// materialized from for ownership evidence.
func fillOpenCodeSnapshotForkCopies(ctx context.Context, source OpenCodeSQLiteSource, opts OpenCodeSnapshotOptions, budget int64, spent *int64, snapshot *OpenCodeHistorySnapshot) error {
	if len(opts.Copied) > 0 {
		snapshot.Copied = append([]OpenCodeHistoryRow(nil), opts.Copied...)
		return nil
	}
	if opts.Fork == nil || strings.TrimSpace(opts.Fork.SourceSessionID) == "" {
		return nil
	}
	sourceID := opts.Fork.SourceSessionID
	currentID, err := NewOpenCodeCurrentSessionID(sourceID)
	if err != nil {
		return &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "read fork source rows", Reason: fmt.Sprintf("the fork source session identity is invalid: %v", err), Recovery: "record the native fork source session before snapshotting"}
	}
	rows, err := readOpenCodeCurrentRows(ctx, source, currentID, OpenCodeProvenanceScope{SessionID: sourceID, Shape: OpenCodeProvenanceCurrent}, budget, spent, openCodeCopyReadBound(opts.Fork))
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !row.Settled {
			// A running source row is not copy proof; the materializer would
			// drop it anyway. Omitting it here keeps the captured evidence
			// honest about what the boundary can actually copy.
			continue
		}
		row.SourceMessageID = row.MessageID
		row.SourceSessionID = sourceID
		row.MessageID = openCodeCopyMessageID(row.MessageID)
		row.Message.MessageID = row.MessageID
		row.Message.SessionID = snapshot.SessionID
		snapshot.Copied = append(snapshot.Copied, row)
	}
	return nil
}

// openCodeCopyReadBound converts a fork proof to the exclusive sequence bound
// the copy acquisition needs, or nil when the proof can place no row below a
// bound (an unproven or unfinished boundary reads every candidate and keeps it
// uncertain). The bound is the disputed-range ceiling: rows at or beyond it are
// neither inherited nor uncertain, so the read never decodes, budgets, or hashes
// the parent's suffix.
func openCodeCopyReadBound(fork *OpenCodeForkProof) *OpenCodeCurrentSeq {
	if fork == nil {
		return nil
	}
	if fork.ThroughSeq != nil && !fork.ThroughCompleted {
		return nil
	}
	_, disputeBelow, hasBound := openCodeCopyBounds(fork)
	if !hasBound {
		return nil
	}
	bound, err := NewOpenCodeCurrentSeq(disputeBelow)
	if err != nil {
		return nil
	}
	return &bound
}

// readOpenCodeCurrentRows reads one current session's message rows in sequence
// order through the same read-only source, decoding each through the pinned
// provenance row decoder. It issues SELECTs only and adds the payload bytes it
// consumed to spent, failing closed past the budget so no truncated capture is
// ever certified. A non-nil before bound keeps the SQL acquisition, decode and
// budget accounting inside the checked capture prefix.
func readOpenCodeCurrentRows(ctx context.Context, source OpenCodeSQLiteSource, currentID OpenCodeCurrentSessionID, scope OpenCodeProvenanceScope, budget int64, spent *int64, before *OpenCodeCurrentSeq) ([]OpenCodeHistoryRow, error) {
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "read message rows", Reason: fmt.Sprintf("the fixed page size is invalid: %v", err), Recovery: "retry the snapshot"}
	}
	var rows []OpenCodeHistoryRow
	request := OpenCodeCurrentPageRequest{SessionID: currentID, PageSize: pageSize, Before: before}
	for {
		if err := ctx.Err(); err != nil {
			return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "read message rows", Reason: fmt.Sprintf("the snapshot read was cancelled: %v", err), Recovery: "retry the snapshot"}
		}
		page, err := source.CurrentMessages(ctx, request)
		if err != nil {
			return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "read message rows", Reason: "the message page read failed against the native store", Recovery: "verify the source remains a supported session_message store and retry"}
		}
		for _, row := range page.Messages {
			*spent += int64(len(row.Data))
			if *spent > budget {
				return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "read message rows", Reason: fmt.Sprintf("the snapshot payload passed the %d-byte bound", budget), Recovery: "raise the snapshot budget and retry; no truncated capture is certified"}
			}
			decoded, settled, err := DecodeOpenCodeProvenanceRow(OpenCodeProvenanceRow{
				ID:          row.ID.String(),
				SessionID:   row.SessionID.String(),
				Type:        row.Type.String(),
				TimeCreated: row.TimeCreated,
				TimeUpdated: row.TimeUpdated,
				Seq:         row.Seq.Value(),
				HasSeq:      true,
				Data:        row.Data,
			}, scope)
			if err != nil {
				if err == errOpenCodeSkipControlRow {
					continue
				}
				return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "decode message row", Reason: fmt.Sprintf("the message row at sequence %d failed its typed decode", row.Seq.Value()), Recovery: "verify the source row and retry; no partial capture is certified"}
			}
			rows = append(rows, OpenCodeHistoryRow{
				MessageID:   row.ID.String(),
				Shape:       scope.Shape,
				Seq:         row.Seq.Value(),
				HasSeq:      true,
				NativeType:  row.Type.String(),
				CreatedMs:   row.TimeCreated,
				UpdatedMs:   row.TimeUpdated,
				CompletedMs: decoded.TimeCompleted,
				Settled:     settled,
				Message:     decoded,
			})
		}
		if page.Next == nil {
			return rows, nil
		}
		request.After = page.Next
	}
}

// openCodeCopyMessageID derives the bounded child identity of one copied source
// row. The source identity is recorded beside it; the child block identity must
// differ from the parent's so the two sessions never share an alias key.
func openCodeCopyMessageID(sourceMessageID string) string {
	return "copy-" + sourceMessageID
}

func fillOpenCodeSnapshotParent(ctx context.Context, source OpenCodeSQLiteSource, linkID OpenCodeSessionLinkID, snapshot *OpenCodeHistorySnapshot, scope *OpenCodeProvenanceScope) error {
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "read session row", Reason: fmt.Sprintf("the fixed page size is invalid: %v", err), Recovery: "retry the snapshot"}
	}
	page, err := source.SessionRecords(ctx, OpenCodeSessionRecordPageRequest{
		Selection: OpenCodeSessionRecordsPreferred,
		PageSize:  pageSize,
		SessionID: &linkID,
	})
	if err != nil {
		return &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "read session row", Reason: "the session row read failed against the native store", Recovery: "verify the source remains a supported session store and retry"}
	}
	for _, record := range page.Records {
		if record.SessionID.String() != snapshot.SessionID {
			continue
		}
		if record.ParentID.String() != "" {
			snapshot.ParentID = record.ParentID.String()
			snapshot.HasParent = true
		} else if page.HasParent {
			snapshot.ParentNullProven = true
		}
		scope.HasParent = snapshot.HasParent
		scope.ParentNullProven = snapshot.ParentNullProven
		return nil
	}
	return &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "read session row", Reason: "the session has no row in the selected session table", Recovery: "snapshot a session the source carries, or snapshot the legacy shape when the session lives there"}
}

// SnapshotOpenCodeProvenance snapshots one session's history through the
// adapter's existing read-only materialization source. The source opens
// read-only and every statement inside is a SELECT: native rows are never
// written. Copied parent payloads, fork anchors, revert state, and delivery
// correlation travel in opts because no additional native table is invented to
// carry them.
func (a *OpenCodeAdapter) SnapshotOpenCodeProvenance(ctx context.Context, candidatePath string, sessionID string, opts OpenCodeSnapshotOptions) (snapshot OpenCodeHistorySnapshot, err error) {
	err = a.withOpenCodeMaterializationSource(ctx, candidatePath, true, func(source OpenCodeSQLiteSource) error {
		read, readErr := SnapshotOpenCodeHistory(ctx, source, sessionID, opts)
		if readErr != nil {
			return readErr
		}
		snapshot = read
		return nil
	})
	if err != nil {
		return OpenCodeHistorySnapshot{}, err
	}
	return snapshot, nil
}

// BuildOpenCodeProvenanceCapture resolves one snapshot into the shared
// classified-capture contract: materialized copy proof and emission plan,
// per-message native classification with delivery correlation applied, ordered
// segments, and the declared earlier section for retained uncertain copies.
// The caller supplies the generation identity, the session metadata the
// pipeline owns, and the prior alias/captured-prefix evidence an earlier
// candidate or activation left behind; the builder verifies the metadata
// agrees with the snapshot and reuses the prior identities. Proven inherited
// copies are handed to the shared builder as retained segment evidence, so they
// never become child-owned main chat.
func BuildOpenCodeProvenanceCapture(snapshot OpenCodeHistorySnapshot, generationID string, metadata schema.UnifiedMetadata, prior OpenCodeProvenancePrior) (ClassifiedCapture, error) {
	if strings.TrimSpace(generationID) == "" {
		return ClassifiedCapture{}, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "build provenance capture", Reason: "generation identity is empty", Recovery: "assign the installed generation id before building the capture"}
	}
	sessionID, err := NewSessionID(snapshot.SessionID)
	if err != nil {
		return ClassifiedCapture{}, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "build provenance capture", Reason: "session identity is invalid", Recovery: "snapshot a concrete native session id"}
	}
	if metadata.SessionID != sessionID {
		return ClassifiedCapture{}, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "build provenance capture", Reason: "supplied metadata names another session", Recovery: "build the capture from the session the metadata describes"}
	}
	if metadata.ModelHarness != schema.HarnessOpenCode {
		return ClassifiedCapture{}, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "build provenance capture", Reason: "supplied metadata names another harness", Recovery: "build the capture with the OpenCode harness metadata"}
	}
	if snapshot.Fork != nil && len(snapshot.Copied) == 0 && prior.HasCapturedPrefix && len(prior.CapturedPrefix) > 0 {
		// The live fork source no longer carries the captured rows. Retain the
		// prior captured prefix rather than rebuilding the child from an empty
		// parent read, so the proven context and its identities survive.
		snapshot.Copied = append([]OpenCodeHistoryRow(nil), prior.CapturedPrefix...)
		snapshot.SourceEvidenceDigest = DigestOpenCodeSnapshot(snapshot)
	}
	if strings.TrimSpace(snapshot.SourceEvidenceDigest) == "" {
		snapshot.SourceEvidenceDigest = DigestOpenCodeSnapshot(snapshot)
	}
	materialized, err := MaterializeOpenCodeHistory(snapshot)
	if err != nil {
		return ClassifiedCapture{}, err
	}
	capture := ClassifiedCapture{
		ID:                   generationID,
		SessionID:            sessionID,
		Harness:              HarnessOpenCode,
		Metadata:             metadata,
		SourceEvidenceDigest: snapshot.SourceEvidenceDigest,
		Completeness:         materialized.Completeness,
		Segments:             materialized.Segments,
		Prior:                prior.Aliases.clone(),
	}
	if needsOpenCodeUncertainSection(materialized.Messages) {
		capture.EarlierStates = []schema.EarlierHistoryState{schema.EarlierHistoryUncertainUnresolved}
	}
	for _, msg := range materialized.Messages {
		decoded := msg.Row.Message
		decoded.SessionID = snapshot.SessionID
		decoded.ParentNullProven = snapshot.ParentNullProven
		decoded.HasParent = snapshot.HasParent
		if snapshot.AgentDeliveredIDs[msg.Row.MessageID] {
			decoded.AgentDelivered = true
		}
		blocks, err := ClassifyOpenCodeMessage(decoded, msg.Attribution)
		if err != nil {
			return ClassifiedCapture{}, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "classify message row", Reason: "a captured message row failed section 3.2 classification", Recovery: "verify the source row and retry; no partial capture is certified"}
		}
		section := ProjectionSection{}
		if msg.Attribution.UncertainCopy {
			section = ProjectionSection{Earlier: true, Index: 1}
		}
		for _, block := range blocks {
			block.Section = section
			if msg.Attribution.Inherited && msg.Emit && msg.RetainedSegment != nil {
				block.Retained = true
				block.SegmentOrdinal = *msg.RetainedSegment
				block.Section = ProjectionSection{}
				block.Uncertain = false
			}
			capture.Blocks = append(capture.Blocks, block)
		}
	}
	return capture, nil
}

// OpenCodeIncompleteProvenanceError refuses an incomplete provenance candidate
// so the caller retains the last good generation instead of replacing proven
// content with an uncertified prefix. It names the session, the missing proof,
// and the safe recovery without carrying native content.
type OpenCodeIncompleteProvenanceError struct {
	SessionID    string
	Completeness indexformat.GenerationCompleteness
	Diagnostics  []string
}

func (e *OpenCodeIncompleteProvenanceError) Error() string {
	return fmt.Sprintf("refuse OpenCode provenance candidate for session %q: completeness %q proves no full capture (%s); the last good generation stays active; restore the missing proof and retry", e.SessionID, e.Completeness, strings.Join(e.Diagnostics, "; "))
}

// OpenCodeHistoricalProvenanceBlocks decodes one discovered legacy, semantic,
// or managed OpenCode session into classified blocks over the real semantic
// readers. Historical shapes carry no typed discriminator and prove no
// admission route, so the classifier keeps the native role and synthetic flags
// and leaves delivery unknown; the shared projection builder turns the blocks
// into the same validated generation contract the current-shape path uses. It
// is the candidate seam for a session whose only authority is a legacy or
// semantic shape.
func (idx *OpenCodeIndexer) OpenCodeHistoricalProvenanceBlocks(_ context.Context, session DiscoveredSession) ([]ClassifiedBlock, error) {
	messages, err := idx.loadOpenCodeHistoricalSemanticMessages(session)
	if err != nil {
		return nil, err
	}
	scope := OpenCodeProvenanceScope{SessionID: session.SessionID.String(), Shape: OpenCodeProvenanceSemantic}
	var blocks []ClassifiedBlock
	for _, message := range messages {
		decoded, _, err := decodeOpenCodeSemanticProvenance(message, scope)
		if err != nil {
			return nil, fmt.Errorf("classify historical OpenCode session %q failed at semantic decode; no partial candidate is eligible; verify the source and retry", session.SessionID)
		}
		classified, err := ClassifyOpenCodeMessage(decoded, LocalOpenCodeAttribution())
		if err != nil {
			return nil, fmt.Errorf("classify historical OpenCode session %q failed at section 3.2 classification; no partial candidate is eligible; verify the source and retry", session.SessionID)
		}
		blocks = append(blocks, classified...)
	}
	return blocks, nil
}

// loadOpenCodeHistoricalSemanticMessages reads a legacy or semantic session
// through the same readers the retained indexer uses: the managed projection
// for a managed origin, and the message/part files for a file origin. It never
// reparses a current typed row, and it never writes to the source.
func (idx *OpenCodeIndexer) loadOpenCodeHistoricalSemanticMessages(session DiscoveredSession) ([]openCodeSemanticMessage, error) {
	switch session.TranscriptOrigin {
	case TranscriptOriginOpenCodeLegacySQLite, TranscriptOriginOpenCodeCurrentSQLite:
		projectionPath := session.SourcePath.String()
		if err := refuseOpenCodeProviderDatabase(idx.fs, session, projectionPath); err != nil {
			return nil, err
		}
		expectedFormat, expectedVersion, err := managedOpenCodeProjectionFormat(session.TranscriptOrigin)
		if err != nil {
			return nil, err
		}
		data, err := idx.fs.ReadFile(projectionPath)
		if err != nil {
			return nil, fmt.Errorf("read historical %s OpenCode session %q projection failed while opening the managed artifact; no candidate is eligible; restore the managed artifact and retry", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID)
		}
		projection, err := decodeManagedOpenCodeProjection(data, expectedFormat, expectedVersion, session.SessionID)
		if err != nil {
			return nil, fmt.Errorf("read historical %s OpenCode session %q projection failed at strict envelope decode; no candidate is eligible; regenerate the managed artifact and retry", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID)
		}
		messages, _, err := parseManagedOpenCodeSemanticMessages(projection, managedOpenCodeProjectionKind(session.TranscriptOrigin))
		if err != nil {
			return nil, fmt.Errorf("read historical %s OpenCode session %q projection failed at semantic decode; no partial candidate is eligible; regenerate the managed artifact and retry", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID)
		}
		return messages, nil
	case TranscriptOriginFile:
		return loadOpenCodeJSONSemanticMessages(idx.fs, session), nil
	default:
		return nil, fmt.Errorf("read historical OpenCode session %q failed before reading: transcript origin %d is outside the supported file/legacy/current set; no candidate is eligible; return a supported origin from discovery", session.SessionID, session.TranscriptOrigin)
	}
}

// OpenCodeProvenancePrior carries the alias state, retained captured prefix, and
// generation completeness a prior candidate or activation left behind. The
// indexer consults it before allocating, so a re-run, an append, a retry, or a
// reopen reuses every identity and every inherited block it already proved.
// The activation owns persisting it; the indexer never invents it.
type OpenCodeProvenancePrior struct {
	// Aliases reuses the block and submission identities a prior generation
	// allocated for the same native keys.
	Aliases ProjectionPriorState
	// CapturedPrefix retains the settled source rows a prior capture copied.
	// The indexer falls back to them when the live fork source no longer
	// carries those rows, so deleting the parent cannot blank the child.
	CapturedPrefix []OpenCodeHistoryRow
	// HasCapturedPrefix marks CapturedPrefix as authoritative prior evidence.
	HasCapturedPrefix bool
	// HasCompleteGeneration marks that a complete last-good generation exists.
	// An incomplete candidate may install only when it is the first
	// discovery; replacing a complete generation with an incomplete one is
	// refused.
	HasCompleteGeneration bool
}

// OpenCodeProvenanceIndexerConfig wires the provenance candidate path into an
// OpenCodeIndexer. Snapshot supplies the read-only native snapshot, Metadata
// supplies the pipeline-owned session metadata, and GenerationID assigns the
// installed generation. Prior optionally supplies the last-good alias state,
// retained captured prefix, and completeness so unchanged native entries keep
// their identities. Allocator optionally overrides the opaque-ref allocator
// (production uses RandomRefAllocator). The path stays disabled until the
// managed repair activation enables it with real native candidates; while
// disabled every existing V1 flow is untouched.
type OpenCodeProvenanceIndexerConfig struct {
	Enabled      bool
	Snapshot     func(ctx context.Context, session DiscoveredSession) (OpenCodeHistorySnapshot, error)
	Metadata     func(session DiscoveredSession) (schema.UnifiedMetadata, error)
	GenerationID func(session DiscoveredSession) string
	Prior        func(ctx context.Context, session DiscoveredSession) (OpenCodeProvenancePrior, error)
	Allocator    RefAllocator
}

// WithOpenCodeProvenanceCapture enables the provenance candidate path with its
// injected snapshot, metadata, and generation dependencies.
func WithOpenCodeProvenanceCapture(config OpenCodeProvenanceIndexerConfig) OpenCodeIndexerOption {
	return func(idx *OpenCodeIndexer) { idx.provenanceCapture = config }
}

// IndexOpenCodeProvenanceV2 is the provenance candidate production exit: one
// read-only snapshot through native typed classification and fork proof into
// the shared validated generation contract the managed repair activation
// consumes. The prior dependency supplies the last-good alias state, retained
// captured prefix, and completeness, so unchanged native entries keep stable
// refs, an append reuses its identities, a retry reuses its allocations, and a
// deleted parent cannot blank the captured child prefix. An incomplete capture
// is returned to activation with its validated completeness: a first
// discovery may install it without success stamps, but replacing an existing
// complete generation with an incomplete one is refused with
// OpenCodeIncompleteProvenanceError so the last good generation survives. The
// candidate is never silently degraded to a thinner V1 result.
func (idx *OpenCodeIndexer) IndexOpenCodeProvenanceV2(ctx context.Context, session DiscoveredSession) (indexformat.V2, error) {
	config := idx.provenanceCapture
	if !config.Enabled {
		return indexformat.V2{}, fmt.Errorf("index OpenCode provenance for session %q failed before the snapshot: the provenance candidate path is disabled; enable it with a real snapshot before requesting a V2 candidate", session.SessionID)
	}
	if config.Snapshot == nil || config.Metadata == nil || config.GenerationID == nil {
		return indexformat.V2{}, fmt.Errorf("index OpenCode provenance for session %q failed before the snapshot: the candidate path misses its snapshot, metadata, or generation dependency; wire all three before requesting a V2 candidate", session.SessionID)
	}
	if err := ctx.Err(); err != nil {
		return indexformat.V2{}, err
	}
	prior := OpenCodeProvenancePrior{Aliases: NewProjectionPriorState()}
	if config.Prior != nil {
		loaded, err := config.Prior(ctx, session)
		if err != nil {
			return indexformat.V2{}, err
		}
		prior = loaded
		if prior.Aliases.Entries == nil {
			prior.Aliases = NewProjectionPriorState()
		}
	}
	snapshot, err := config.Snapshot(ctx, session)
	if err != nil {
		return indexformat.V2{}, err
	}
	metadata, err := config.Metadata(session)
	if err != nil {
		return indexformat.V2{}, err
	}
	generationID := config.GenerationID(session)
	capture, err := BuildOpenCodeProvenanceCapture(snapshot, generationID, metadata, prior)
	if err != nil {
		return indexformat.V2{}, err
	}
	allocator := config.Allocator
	if allocator == nil {
		allocator = RandomRefAllocator{}
	}
	built, err := BuildV2(capture, allocator)
	if err != nil {
		return indexformat.V2{}, err
	}
	if built.Generation.Completeness != indexformat.GenerationCompletenessComplete {
		if prior.HasCompleteGeneration {
			diagnostics := []string{"the capture proves no full generation", "a complete last-good generation stays active"}
			if len(snapshot.SourceEvidenceDigest) == 0 {
				diagnostics = append(diagnostics, "the snapshot carries no evidence digest")
			}
			return indexformat.V2{}, &OpenCodeIncompleteProvenanceError{SessionID: snapshot.SessionID, Completeness: built.Generation.Completeness, Diagnostics: diagnostics}
		}
		// First discovery: return the validated incomplete candidate to
		// activation without success stamps. Activation refuses to overwrite a
		// complete generation; a first install exposes the readable evidence.
	}
	return built, nil
}
