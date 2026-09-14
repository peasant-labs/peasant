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
	own, err := readOpenCodeCurrentRows(ctx, source, currentID, scope, budget, &spent)
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
	rows, err := readOpenCodeCurrentRows(ctx, source, currentID, OpenCodeProvenanceScope{SessionID: sourceID, Shape: OpenCodeProvenanceCurrent}, budget, spent)
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

// readOpenCodeCurrentRows reads one current session's message rows in sequence
// order through the same read-only source, decoding each through the pinned
// provenance row decoder. It issues SELECTs only and adds the payload bytes it
// consumed to spent, failing closed past the budget so no truncated capture is
// ever certified.
func readOpenCodeCurrentRows(ctx context.Context, source OpenCodeSQLiteSource, currentID OpenCodeCurrentSessionID, scope OpenCodeProvenanceScope, budget int64, spent *int64) ([]OpenCodeHistoryRow, error) {
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "read message rows", Reason: fmt.Sprintf("the fixed page size is invalid: %v", err), Recovery: "retry the snapshot"}
	}
	var rows []OpenCodeHistoryRow
	request := OpenCodeCurrentPageRequest{SessionID: currentID, PageSize: pageSize}
	for {
		if err := ctx.Err(); err != nil {
			return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "read message rows", Reason: fmt.Sprintf("the snapshot read was cancelled: %v", err), Recovery: "retry the snapshot"}
		}
		page, err := source.CurrentMessages(ctx, request)
		if err != nil {
			return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "read message rows", Reason: fmt.Sprintf("the message page read failed: %v", err), Recovery: "verify the source remains a supported session_message store and retry"}
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
				return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "decode message row", Reason: fmt.Sprintf("row %q failed: %v", row.ID.String(), err), Recovery: "verify the source row and retry; no partial capture is certified"}
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
		return &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "read session row", Reason: fmt.Sprintf("the session row read failed: %v", err), Recovery: "verify the source remains a supported session store and retry"}
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
// The caller supplies the generation identity and the session metadata the
// pipeline owns; the builder verifies both agree with the snapshot. Segment
// captured refs resolve after BuildV2 through ResolveOpenCodeSegmentRefs,
// because refs allocate inside the shared builder.
func BuildOpenCodeProvenanceCapture(snapshot OpenCodeHistorySnapshot, generationID string, metadata schema.UnifiedMetadata) (ClassifiedCapture, [][]string, error) {
	if strings.TrimSpace(generationID) == "" {
		return ClassifiedCapture{}, nil, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "build provenance capture", Reason: "generation identity is empty", Recovery: "assign the installed generation id before building the capture"}
	}
	sessionID, err := NewSessionID(snapshot.SessionID)
	if err != nil {
		return ClassifiedCapture{}, nil, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "build provenance capture", Reason: fmt.Sprintf("session identity is invalid: %v", err), Recovery: "snapshot a concrete native session id"}
	}
	if metadata.SessionID != sessionID {
		return ClassifiedCapture{}, nil, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "build provenance capture", Reason: "supplied metadata names another session", Recovery: "build the capture from the session the metadata describes"}
	}
	if metadata.ModelHarness != schema.HarnessOpenCode {
		return ClassifiedCapture{}, nil, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "build provenance capture", Reason: fmt.Sprintf("supplied metadata names harness %q", metadata.ModelHarness), Recovery: "build the capture with the OpenCode harness metadata"}
	}
	if strings.TrimSpace(snapshot.SourceEvidenceDigest) == "" {
		snapshot.SourceEvidenceDigest = DigestOpenCodeSnapshot(snapshot)
	}
	materialized, err := MaterializeOpenCodeHistory(snapshot)
	if err != nil {
		return ClassifiedCapture{}, nil, err
	}
	capture := ClassifiedCapture{
		ID:                   generationID,
		SessionID:            sessionID,
		Harness:              HarnessOpenCode,
		Metadata:             metadata,
		SourceEvidenceDigest: snapshot.SourceEvidenceDigest,
		Completeness:         materialized.Completeness,
		Segments:             materialized.Segments,
		Prior:                NewProjectionPriorState(),
	}
	if needsOpenCodeUncertainSection(materialized.Messages) {
		capture.EarlierStates = []schema.EarlierHistoryState{schema.EarlierHistoryUncertainUnresolved}
	}
	segmentKeys := make([][]string, len(materialized.Segments))
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
			return ClassifiedCapture{}, nil, &OpenCodeSnapshotError{SessionID: snapshot.SessionID, Step: "classify message row", Reason: fmt.Sprintf("row %q failed: %v", msg.Row.MessageID, err), Recovery: "verify the source row and retry; no partial capture is certified"}
		}
		section := ProjectionSection{}
		if msg.Attribution.UncertainCopy {
			section = ProjectionSection{Earlier: true, Index: 1}
		}
		for _, block := range blocks {
			block.Section = section
			capture.Blocks = append(capture.Blocks, block)
			if msg.Attribution.Inherited && msg.Emit {
				for i := range capture.Segments {
					segmentKeys[i] = append(segmentKeys[i], block.NativeKey)
				}
			}
		}
	}
	return capture, segmentKeys, nil
}

// ResolveOpenCodeSegmentRefs fills each segment's captured refs from the built
// generation's alias map. Every admitted native key must resolve: a missing
// alias means the builder dropped proven evidence, which fails closed instead
// of certifying a prefix the generation does not hold.
func ResolveOpenCodeSegmentRefs(segmentKeys [][]string, generation *indexformat.Generation) error {
	if len(segmentKeys) != len(generation.Segments) {
		return fmt.Errorf("ingest.ResolveOpenCodeSegmentRefs: %d key groups disagree with %d built segments; the prefix proof cannot be attached; rebuild the capture and resolve against its own generation", len(segmentKeys), len(generation.Segments))
	}
	byKey := make(map[string]schema.SourceEntryRef, len(generation.Aliases))
	for _, alias := range generation.Aliases {
		byKey[alias.NativeKey] = alias.Ref
	}
	for i, keys := range segmentKeys {
		seen := map[schema.SourceEntryRef]bool{}
		for _, key := range keys {
			encoded, err := encodeBlockAliasKey(key)
			if err != nil {
				return fmt.Errorf("ingest.ResolveOpenCodeSegmentRefs: segment %d native key %q cannot be encoded: %w; the prefix proof cannot be attached; rebuild the capture", i, key, err)
			}
			ref, ok := byKey[encoded]
			if !ok {
				return fmt.Errorf("ingest.ResolveOpenCodeSegmentRefs: segment %d native key %q has no allocated ref; proven prefix evidence left the generation; rebuild the capture instead of certifying a thinner prefix", i, key)
			}
			if !seen[ref] {
				seen[ref] = true
				generation.Segments[i].CapturedRefs = append(generation.Segments[i].CapturedRefs, ref)
			}
		}
	}
	return nil
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
			return nil, fmt.Errorf("classify historical OpenCode session %q message %q failed at semantic decode: %w; no partial candidate is eligible; verify the source and retry", session.SessionID, message.EntryID, err)
		}
		classified, err := ClassifyOpenCodeMessage(decoded, LocalOpenCodeAttribution())
		if err != nil {
			return nil, fmt.Errorf("classify historical OpenCode session %q message %q failed at section 3.2 classification: %w; no partial candidate is eligible; verify the source and retry", session.SessionID, message.EntryID, err)
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
			return nil, fmt.Errorf("read historical %s OpenCode session %q projection failed at %q: %w; no candidate is eligible; restore the managed artifact and retry", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID, session.SourcePath, err)
		}
		projection, err := decodeManagedOpenCodeProjection(data, expectedFormat, expectedVersion, session.SessionID)
		if err != nil {
			return nil, fmt.Errorf("read historical %s OpenCode session %q projection failed at strict envelope decode: %w; no candidate is eligible; regenerate the managed artifact and retry", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID, err)
		}
		messages, _, err := parseManagedOpenCodeSemanticMessages(projection, managedOpenCodeProjectionKind(session.TranscriptOrigin))
		if err != nil {
			return nil, fmt.Errorf("read historical %s OpenCode session %q projection failed at semantic decode: %w; no partial candidate is eligible; regenerate the managed artifact and retry", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID, err)
		}
		return messages, nil
	case TranscriptOriginFile:
		return loadOpenCodeJSONSemanticMessages(idx.fs, session), nil
	default:
		return nil, fmt.Errorf("read historical OpenCode session %q failed before reading: transcript origin %d is outside the supported file/legacy/current set; no candidate is eligible; return a supported origin from discovery", session.SessionID, session.TranscriptOrigin)
	}
}

// OpenCodeProvenanceIndexerConfig wires the provenance candidate path into an
// OpenCodeIndexer. Snapshot supplies the read-only native snapshot, Metadata
// supplies the pipeline-owned session metadata, and GenerationID assigns the
// installed generation. The path stays disabled until the managed repair
// activation enables it with real native candidates; while disabled every
// existing V1 flow is untouched.
type OpenCodeProvenanceIndexerConfig struct {
	Enabled      bool
	Snapshot     func(ctx context.Context, session DiscoveredSession) (OpenCodeHistorySnapshot, error)
	Metadata     func(session DiscoveredSession) (schema.UnifiedMetadata, error)
	GenerationID func(session DiscoveredSession) string
}

// WithOpenCodeProvenanceCapture enables the provenance candidate path with its
// injected snapshot, metadata, and generation dependencies.
func WithOpenCodeProvenanceCapture(config OpenCodeProvenanceIndexerConfig) OpenCodeIndexerOption {
	return func(idx *OpenCodeIndexer) { idx.provenanceCapture = config }
}

// IndexOpenCodeProvenanceV2 is the provenance candidate production exit: one
// read-only snapshot through native typed classification and fork proof into
// the shared validated generation contract the managed repair activation
// consumes. An incomplete capture is refused with
// OpenCodeIncompleteProvenanceError so the last good generation survives; it
// is never silently degraded to a thinner V1 result.
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
	snapshot, err := config.Snapshot(ctx, session)
	if err != nil {
		return indexformat.V2{}, err
	}
	metadata, err := config.Metadata(session)
	if err != nil {
		return indexformat.V2{}, err
	}
	generationID := config.GenerationID(session)
	capture, segmentKeys, err := BuildOpenCodeProvenanceCapture(snapshot, generationID, metadata)
	if err != nil {
		return indexformat.V2{}, err
	}
	built, err := BuildV2(capture, RandomRefAllocator{})
	if err != nil {
		return indexformat.V2{}, err
	}
	if err := ResolveOpenCodeSegmentRefs(segmentKeys, &built.Generation); err != nil {
		return indexformat.V2{}, err
	}
	if built.Generation.Completeness != indexformat.GenerationCompletenessComplete {
		diagnostics := []string{"the capture proves no full generation"}
		if len(snapshot.SourceEvidenceDigest) == 0 {
			diagnostics = append(diagnostics, "the snapshot carries no evidence digest")
		}
		return indexformat.V2{}, &OpenCodeIncompleteProvenanceError{SessionID: snapshot.SessionID, Completeness: built.Generation.Completeness, Diagnostics: diagnostics}
	}
	return built, nil
}
