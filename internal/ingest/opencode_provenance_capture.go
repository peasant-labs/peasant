package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// OpenCodeSnapshotOptions carries the evidence a snapshot read cannot prove
// from message rows alone: the fork anchor, the native fork pointer, the revert
// state, and the per-input subagent delivery correlation. Fork and Revert are
// caller-supplied proofs for repair paths; NativeFork is derived by
// SnapshotOpenCodeProvenance from the session row. Every field is explicit
// native evidence or nil; the reader never infers them from timestamps or text.
type OpenCodeSnapshotOptions struct {
	Fork              *OpenCodeForkProof
	NativeFork        *OpenCodeForkEvidence
	Revert            *OpenCodeRevertState
	AgentDeliveredIDs map[string]bool
	// Copied optionally supplies already-captured source rows. It exists for a
	// caller that holds the source rows from the same read view. When it is
	// empty and Fork names a source session, the reader copies that session's
	// current rows itself; a supplied set is never merged with a native read.
	Copied []OpenCodeHistoryRow
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
	snapshot := OpenCodeHistorySnapshot{
		SessionID:         sessionID,
		Fork:              opts.Fork,
		NativeFork:        opts.NativeFork,
		Revert:            opts.Revert,
		AgentDeliveredIDs: opts.AgentDeliveredIDs,
	}
	scope := OpenCodeProvenanceScope{SessionID: sessionID, Shape: OpenCodeProvenanceCurrent}
	if err := fillOpenCodeSnapshotParent(ctx, source, linkID, &snapshot, &scope); err != nil {
		return OpenCodeHistorySnapshot{}, err
	}
	own, err := readOpenCodeCurrentRows(ctx, source, currentID, scope, nil)
	if err != nil {
		return OpenCodeHistorySnapshot{}, err
	}
	snapshot.Messages = own
	if err := fillOpenCodeSnapshotForkCopies(ctx, source, opts, &snapshot); err != nil {
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
func fillOpenCodeSnapshotForkCopies(ctx context.Context, source OpenCodeSQLiteSource, opts OpenCodeSnapshotOptions, snapshot *OpenCodeHistorySnapshot) error {
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
	rows, err := readOpenCodeCurrentRows(ctx, source, currentID, OpenCodeProvenanceScope{SessionID: sourceID, Shape: OpenCodeProvenanceCurrent}, openCodeCopyReadBound(opts.Fork))
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
// neither inherited nor uncertain, so the read never decodes or hashes the
// parent's suffix.
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
// provenance row decoder. It issues SELECTs only. There is no payload-size
// bound: OpenCode rows carry no per-record size limit, so no row is ever
// refused or dropped for its size. A non-nil before bound keeps the SQL
// acquisition and decode inside the checked capture prefix.
func readOpenCodeCurrentRows(ctx context.Context, source OpenCodeSQLiteSource, currentID OpenCodeCurrentSessionID, scope OpenCodeProvenanceScope, before *OpenCodeCurrentSeq) ([]OpenCodeHistoryRow, error) {
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, &OpenCodeSnapshotError{SessionID: scope.SessionID, Step: "read message rows", Reason: fmt.Sprintf("the fixed page size is invalid: %v", err), Recovery: "retry the snapshot"}
	}
	var rows []OpenCodeHistoryRow
	var traversalPosition int64
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
			position := openCodeUnknownPosition(scope.SessionID, "current", row.ID.String(), int64(len(rows)), traversalPosition, "")
			scope.UnknownPosition = position.Public
			traversal := decodeOpenCodeCurrentTraversal([]byte(row.Data), true)
			traversalPosition += traversal.size
			decoded, settled, err := decodeOpenCodeProvenanceRow(OpenCodeProvenanceRow{
				ID:          row.ID.String(),
				SessionID:   row.SessionID.String(),
				Type:        row.Type.String(),
				TimeCreated: row.TimeCreated,
				TimeUpdated: row.TimeUpdated,
				Seq:         row.Seq.Value(),
				HasSeq:      true,
				Data:        row.Data,
			}, scope, &traversal)
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
// carry them. The whole acquisition boundary is sanitized here: an open,
// transaction, read, or close failure cannot return the candidate path, the
// SQLite URI, a raw dependency cause, or a private locator. Already-typed
// snapshot refusals keep their fixed category; every other failure is reduced
// to a fixed step and reason.
func (a *OpenCodeAdapter) SnapshotOpenCodeProvenance(ctx context.Context, candidatePath string, sessionID string, opts OpenCodeSnapshotOptions) (snapshot OpenCodeHistorySnapshot, err error) {
	options := a.candidateOptions
	options.readSnapshot = true
	source, err := a.openOpenCodeSQLiteSourceWithOptions(ctx, candidatePath, options)
	if err != nil {
		return OpenCodeHistorySnapshot{}, sanitizeOpenCodeRefusal(sessionID, "open snapshot source", "the read-only native source could not be opened", "verify the native database exists, is readable, and is not blocked by permissions, then retry without modifying it", err)
	}
	if opts.Fork == nil {
		if forkSource, ok := source.(OpenCodeForkEvidenceSource); ok {
			linkID, linkErr := NewOpenCodeSessionLinkID(sessionID)
			if linkErr != nil {
				_ = closeDeferredOpenCodeSource(ctx, source, a.candidateOptions.queryTimeout)
				return OpenCodeHistorySnapshot{}, sanitizeOpenCodeRefusal(sessionID, "read native fork evidence", "the session identity cannot be used to read the native fork pointer", "repair the session identity and retry; no partial capture is certified", linkErr)
			}
			evidence, present, evidenceErr := forkSource.SessionForkEvidence(ctx, linkID)
			if evidenceErr != nil {
				_ = closeDeferredOpenCodeSource(ctx, source, a.candidateOptions.queryTimeout)
				return OpenCodeHistorySnapshot{}, sanitizeOpenCodeRefusal(sessionID, "read native fork evidence", "the native fork pointer could not be read", "verify the native database is readable and retry; no partial capture is certified", evidenceErr)
			}
			if present {
				opts.NativeFork = &evidence
			}
		}
	}
	read, readErr := SnapshotOpenCodeHistory(ctx, source, sessionID, opts)
	closeErr := source.Close(ctx)
	if readErr != nil {
		return OpenCodeHistorySnapshot{}, sanitizeOpenCodeRefusal(sessionID, "read snapshot rows", "the snapshot read failed against the native store", "verify the source remains a supported OpenCode store and retry; no partial capture is certified", readErr)
	}
	if closeErr != nil {
		return OpenCodeHistorySnapshot{}, sanitizeOpenCodeRefusal(sessionID, "close snapshot source", "the read-only source could not be released", "retry the snapshot; a bounded close retries the release", closeErr)
	}
	return read, nil
}

// sanitizeOpenCodeRefusal reduces any failure on the snapshot or
// candidate boundary to a fixed-category OpenCodeSnapshotError. An
// already-typed snapshot or incomplete-candidate refusal passes through because
// its category is fixed and free of native content; a cancellation or deadline
// keeps its safe classification; everything else — a raw SQLite opener cause, a
// private candidate path, a URI, or a shared-projection refusal that prints a
// native key — becomes a bounded step and reason with no wrapped text.
func sanitizeOpenCodeRefusal(sessionID, step, reason, recovery string, err error) error {
	if err == nil {
		return nil
	}
	var snapshotErr *OpenCodeSnapshotError
	if errors.As(err, &snapshotErr) {
		return snapshotErr
	}
	var incomplete *OpenCodeIncompleteProvenanceError
	if errors.As(err, &incomplete) {
		return incomplete
	}
	switch {
	case errors.Is(err, context.Canceled):
		return &OpenCodeSnapshotError{SessionID: sessionID, Step: step, Reason: reason + ": the caller context was cancelled", Recovery: "retry with a live context"}
	case errors.Is(err, context.DeadlineExceeded):
		return &OpenCodeSnapshotError{SessionID: sessionID, Step: step, Reason: reason + ": the bounded deadline ended", Recovery: "retry with a live context"}
	default:
		return &OpenCodeSnapshotError{SessionID: sessionID, Step: step, Reason: reason, Recovery: recovery}
	}
}

// hasOpenCodeContextFrom reports whether the metadata already names a
// context_from target. The relationship kind occurs at most once, so a native
// pointer must not add a second one.
func hasOpenCodeContextFrom(relationships []schema.SessionRelationship) bool {
	for _, relationship := range relationships {
		if relationship.Kind == schema.SessionRelationshipContextFrom {
			return true
		}
	}
	return false
}

// BuildOpenCodeProvenanceCapture resolves one snapshot into the shared
// classified-capture contract: materialized copy proof and emission plan,
// per-message native classification with delivery correlation applied, ordered
// segments, and the declared earlier section for uncertain copies.
// The caller supplies the generation identity, the session metadata the
// pipeline owns, and the prior alias/captured-prefix evidence an earlier
// candidate or activation left behind; the builder verifies the metadata
// agrees with the snapshot and reuses the prior identities. Proven inherited
// copies are handed to the shared builder as inherited segment evidence, so they
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
	nativeForkOmitted := false
	if snapshot.NativeFork != nil && !hasOpenCodeContextFrom(metadata.Relationships) {
		if target, targetErr := schema.NewSessionID(snapshot.NativeFork.SourceSessionID.String()); targetErr == nil {
			candidate := append([]schema.SessionRelationship(nil), metadata.Relationships...)
			candidate = append(candidate, schema.SessionRelationship{
				Kind:          schema.SessionRelationshipContextFrom,
				TargetState:   schema.RelationshipTargetKnown,
				TargetLocalID: &target,
				Evidence:      schema.EvidenceNativeTyped,
				Anchor:        &schema.PublicSourceAnchor{Kind: schema.PublicSourceAnchorGeneral},
			})
			if err := schema.ValidateSessionRelationships(candidate); err == nil {
				metadata.Relationships = candidate
			} else {
				nativeForkOmitted = true
			}
		} else {
			nativeForkOmitted = true
		}
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
		Diagnostics:          append([]string(nil), materialized.Diagnostics...),
	}
	if nativeForkOmitted {
		capture.Diagnostics = append(capture.Diagnostics, "native fork pointer names a source session that cannot be represented as a local relationship; the fork link was omitted")
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
			if msg.Attribution.Inherited && msg.Emit && msg.InheritedSegment != nil && len(block.RetainedUnknown) == 0 {
				block.Inherited = true
				block.SegmentOrdinal = *msg.InheritedSegment
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
	messages, err = retainOpenCodeSemantic(session.SessionID, messages)
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
// through the same readers the stored indexer uses: the managed projection
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
		_, messages, _, err := decodeManagedOpenCodeSemanticProjection(data, expectedFormat, expectedVersion, session.SessionID)
		if err != nil {
			return nil, fmt.Errorf("read historical %s OpenCode session %q projection failed at strict envelope decode; no candidate is eligible; regenerate the managed artifact and retry", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID)
		}
		return messages, nil
	case TranscriptOriginFile:
		return loadOpenCodeJSONSemanticMessages(idx.fs, session), nil
	default:
		return nil, fmt.Errorf("read historical OpenCode session %q failed before reading: transcript origin %d is outside the supported file/legacy/current set; no candidate is eligible; return a supported origin from discovery", session.SessionID, session.TranscriptOrigin)
	}
}

// OpenCodeProvenancePrior carries the alias state, stored captured prefix, and
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
	// CaptureAuthority, when non-nil, names a stored content-capture
	// certificate that holds last-good full authority although no active
	// managed generation exists. It is the writer's own publishable-capture
	// predicate applied to the stored certificate, never a publication
	// revision, and it is preserved with the certificate's own origin and
	// format. The certificate-only lane reuses the latest attempt's aliases and
	// carries no captured-prefix proof: the bridge never fabricates a V2
	// generation.
	CaptureAuthority *StoredCaptureAuthority
}

// BuildGeneration is the OpenCode producer exit. It reads one recorded snapshot
// through the typed classification and fork proof into the shared validated
// generation contract, and returns the full captured bytes and the prior
// document activation persists. The attempt's prior source supplies the
// last-good alias state, stored captured prefix, and completeness, so
// unchanged recorded entries keep stable refs, an append reuses its identities, a
// retry reuses its allocations, and a deleted parent cannot blank the captured
// child prefix. An incomplete capture is returned to activation with its
// validated completeness: a first discovery may install it without success
// stamps, but replacing an existing complete generation with an incomplete one
// is refused with OpenCodeIncompleteProvenanceError so the last good generation
// survives. The candidate is never silently degraded to a thinner V1 result,
// and every internal failure leaves through the fixed-shape refusal with no
// source locator, raw dependency cause, or native identifier.
func (idx *OpenCodeIndexer) BuildGeneration(ctx context.Context, in GenerationBuildInput) (GenerationCandidate, error) {
	session := in.Session
	prior, err := openCodeGenerationPrior(ctx, session, in.Prior)
	if err != nil {
		return GenerationCandidate{}, err
	}
	if err := ctx.Err(); err != nil {
		return GenerationCandidate{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "run candidate before the snapshot", "the candidate was cancelled before it started", "retry the candidate with a live context", err)
	}
	if in.OpenCode == nil {
		return GenerationCandidate{}, &OpenCodeSnapshotError{SessionID: session.SessionID.String(), Step: "resolve candidate configuration", Reason: "the candidate path misses its recorded-source access", Recovery: "wire the recorded-source access before requesting a generation"}
	}
	if in.GenerationID == "" {
		return GenerationCandidate{}, &OpenCodeSnapshotError{SessionID: session.SessionID.String(), Step: "resolve candidate configuration", Reason: "the candidate path misses its generation identity", Recovery: "assign the installed generation identity before requesting a generation"}
	}
	snapshot, err := in.OpenCode.Snapshot(ctx, recordedSourceLocator(in.RecordedMetadata))
	if err != nil {
		return GenerationCandidate{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "snapshot recorded history", "the read-only recorded-source snapshot failed", "verify the recorded source and retry; no candidate was produced", err)
	}
	if snapshot.SessionID != session.SessionID.String() {
		return GenerationCandidate{}, &OpenCodeSnapshotError{SessionID: session.SessionID.String(), Step: "snapshot recorded history", Reason: "the snapshot names a different session", Recovery: "snapshot the session the attempt describes; no candidate was produced"}
	}
	metadata, err := openCodeAttemptMetadata(in.RecordedMetadata, session)
	if err != nil {
		return GenerationCandidate{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "read session metadata", "the session metadata could not be read", "repair the metadata input and retry; no candidate was produced", err)
	}
	capture, err := BuildOpenCodeProvenanceCapture(snapshot, in.GenerationID, metadata, prior)
	if err != nil {
		return GenerationCandidate{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "build provenance capture", "the captured rows could not be classified into the managed capture contract", "verify the captured source rows and retry; no candidate was produced", err)
	}
	allocator := in.Allocator
	if allocator == nil {
		allocator = RandomRefAllocator{}
	}
	built, content, err := BuildV2WithContent(capture, allocator)
	if err != nil {
		return GenerationCandidate{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "validate managed generation", "the classified capture failed shared managed-generation validation", "correct the capture or the allocator and retry; no candidate was produced and the last good generation stays active", err)
	}
	if built.Generation.Completeness != indexformat.GenerationCompletenessComplete {
		// An incomplete candidate is refused EARLY, before activation, when any
		// last-good full authority already exists: a complete V2 generation, or
		// the stored content-capture certificate the writer's own guard would
		// protect. The refusal names the unfinished message and sequence from
		// the capture's own diagnostics, so the held session reads as an
		// actionable last-good-held result instead of a later opaque store
		// refusal. The transactional guard stays in place for the race the
		// prior read cannot close.
		if prior.HasCompleteGeneration || prior.CaptureAuthority != nil {
			diagnostics := append([]string(nil), capture.Diagnostics...)
			diagnostics = append(diagnostics, "the capture proves no full generation")
			if prior.HasCompleteGeneration {
				diagnostics = append(diagnostics, "a complete last-good generation stays active")
			}
			if prior.CaptureAuthority != nil {
				authority := prior.CaptureAuthority
				diagnostics = append(diagnostics, fmt.Sprintf("a stored %s capture certificate of format %q (origin %s, source %s, failure code %q) holds last-good full authority and stays active", authority.Status, authority.CaptureFormat, authority.TranscriptOrigin, authority.SourceAuthority, authority.FailureCode))
			}
			if len(snapshot.SourceEvidenceDigest) == 0 {
				diagnostics = append(diagnostics, "the snapshot carries no evidence digest")
			}
			return GenerationCandidate{}, &OpenCodeIncompleteProvenanceError{SessionID: snapshot.SessionID, Completeness: built.Generation.Completeness, Diagnostics: diagnostics}
		}
		// First discovery: return the validated incomplete candidate to
		// activation without success stamps. Activation refuses to overwrite a
		// complete generation; a first install exposes the readable evidence.
	}
	priorEvidence, err := nextOpenCodeProvenancePrior(built, snapshot)
	if err != nil {
		return GenerationCandidate{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "persist prior evidence", "the prior document for the validated candidate could not be encoded", "correct the capture or the allocator and retry; no candidate was produced", err)
	}
	return GenerationCandidate{Result: built, Blobs: content, PriorEvidence: priorEvidence}, nil
}

// openCodeGenerationPrior resolves one attempt's prior evidence into the
// OpenCode prior the candidate consumes. A nil source yields the empty prior of
// a first discovery. With no active generation it consults the stored capture
// certificate, so an incomplete candidate is refused early against the same
// authority the content writer guards. The certificate's own origin and format
// are preserved; the certificate-only lane still reuses the latest attempt's
// aliases, and no alias beyond the attempt's own and no captured-prefix proof
// is fabricated.
func openCodeGenerationPrior(ctx context.Context, session DiscoveredSession, source GenerationPriorSource) (OpenCodeProvenancePrior, error) {
	prior := OpenCodeProvenancePrior{Aliases: NewProjectionPriorState()}
	if source == nil {
		return prior, nil
	}
	loaded, err := source.ReadGenerationPrior(ctx)
	if err != nil {
		return OpenCodeProvenancePrior{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "load prior evidence", "the last-good alias and captured-prefix evidence could not be loaded", "verify the activation-owned prior store and retry; no candidate was produced and the last good generation stays active", err)
	}
	if loaded == nil || priorCarriesNoEvidenceDocument(loaded) {
		authority, authorityErr := source.ReadStoredCaptureAuthority(ctx)
		if authorityErr != nil {
			return OpenCodeProvenancePrior{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "load stored capture authority", "the stored last-good capture certificate could not be read", "verify the stored capture store and retry; no candidate was produced and the last good generation stays active", authorityErr)
		}
		if authority != nil {
			prior.CaptureAuthority = authority
		}
		if loaded != nil {
			// The certificate-only coexistence state still reuses the latest
			// attempt's aliases; only the evidence document is absent.
			prior.Aliases = generationPriorState(loaded)
		}
		return prior, nil
	}
	translated := OpenCodeProvenancePrior{HasCompleteGeneration: loaded.HasCompleteGeneration}
	if len(loaded.PriorEvidence) > 0 {
		document, decodeErr := DecodeOpenCodeProvenancePrior(loaded.PriorEvidence)
		if decodeErr != nil {
			return OpenCodeProvenancePrior{}, sanitizeOpenCodeRefusal(session.SessionID.String(), "load prior evidence", "the retained prior document could not be decoded", "verify the activation-owned prior store and retry; no candidate was produced and the last good generation stays active", decodeErr)
		}
		translated = document
		translated.HasCompleteGeneration = loaded.HasCompleteGeneration
	}
	translated.Aliases = generationPriorState(loaded)
	return translated, nil
}

// priorCarriesNoEvidenceDocument reports the certificate-only coexistence
// state: the prior names the latest attempt's reusable aliases but carries no
// evidence document and no complete generation. The stored capture certificate
// is still the full-authority proof, so the candidate must still be judged
// against it.
func priorCarriesNoEvidenceDocument(prior *GenerationPrior) bool {
	return prior.Metadata == nil && len(prior.PriorEvidence) == 0 && !prior.HasCompleteGeneration
}

// recordedSourceLocator returns the recorded source locator the attempt was
// captured from. An absent metadata document records none, and the access seam
// refuses the empty locator rather than reading an unrelated path.
func recordedSourceLocator(metadata *schema.UnifiedMetadata) string {
	if metadata == nil {
		return ""
	}
	return metadata.Source.FilePath
}

// openCodeAttemptMetadata resolves the metadata agreement the capture is built
// from. The committed metadata is the attempt's one authority; a nil or
// mismatched document refuses rather than building a generation against another
// session's identity.
func openCodeAttemptMetadata(metadata *schema.UnifiedMetadata, session DiscoveredSession) (schema.UnifiedMetadata, error) {
	if metadata == nil {
		return schema.UnifiedMetadata{}, fmt.Errorf("the committed metadata for session %s is absent; no candidate was produced and the stored generation is unchanged", session.SessionID)
	}
	if metadata.SessionID != session.SessionID {
		return schema.UnifiedMetadata{}, fmt.Errorf("the committed metadata for session %s names session %s; no candidate was produced and the stored generation is unchanged", session.SessionID, metadata.SessionID)
	}
	return *metadata, nil
}

var _ GenerationBuilder = (*OpenCodeIndexer)(nil)

// nextOpenCodeProvenancePrior builds the prior document the activation persists
// beside the new generation: the new aliases, the stored captured prefix the
// candidate settled on, and whether a complete generation now exists.
func nextOpenCodeProvenancePrior(built indexformat.V2, snapshot OpenCodeHistorySnapshot) ([]byte, error) {
	state, err := PriorStateFromGeneration(built.Generation)
	if err != nil {
		return nil, err
	}
	return EncodeOpenCodeProvenancePrior(OpenCodeProvenancePrior{
		Aliases:               state,
		CapturedPrefix:        append([]OpenCodeHistoryRow(nil), snapshot.Copied...),
		HasCapturedPrefix:     len(snapshot.Copied) > 0,
		HasCompleteGeneration: built.Generation.Completeness == indexformat.GenerationCompletenessComplete,
	})
}
