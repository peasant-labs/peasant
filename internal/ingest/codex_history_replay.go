package ingest

// Deterministic replay of captured Codex records into an adapter-private native
// node graph. The replay applies the recognized history modes, the copied
// creation boundary, migration uncertainty, instruction rollback, surviving
// checkpoints, paginated revert, and byte/ordinal checkpoints. It classifies no
// block and allocates no durable generation rows.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// codexHistoryEnvelope is the outer envelope shared by every Codex rollout
// record.
type codexHistoryEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexHistoryReplayPayload is the union of replay-relevant fields on a Codex
// payload. It is discriminated by the envelope type and the nested payload
// type; unrelated fields stay zero.
type codexHistoryReplayPayload struct {
	Type     string          `json:"type"`
	Role     string          `json:"role"`
	ID       string          `json:"id"`
	ItemID   string          `json:"item_id"`
	CallID   string          `json:"call_id"`
	TurnID   string          `json:"turn_id"`
	Name     string          `json:"name"`
	Summary  *string         `json:"summary"`
	NumTurns *int64          `json:"num_turns"`
	Ordinal  *int64          `json:"ordinal"`
	Content  json.RawMessage `json:"content"`
	// ReplacementHistory and ReplacementMetadata are the native compaction
	// baseline arrays. When both are present their lengths must agree; a
	// mismatch or an orphan metadata array marks reconstruction incomplete and
	// no positional pair is invented.
	ReplacementHistory  json.RawMessage `json:"replacement_history"`
	ReplacementMetadata json.RawMessage `json:"replacement_history_metadata"`
}

// codexHistoryRecord is one parsed record with its native ordinal position and
// decoded byte coordinates.
type codexHistoryRecord struct {
	Ordinal          int64
	ByteStart        int64
	ByteEndExclusive int64
	EnvelopeType     string
	Payload          json.RawMessage
	// Partial marks a trailing record without a terminating newline. It is
	// deferred and never interpreted.
	Partial bool
	// Malformed marks a complete record whose bytes are not valid JSON. The
	// byte checkpoint still advances; no ordinal is assigned by line number.
	Malformed bool
}

// parseCodexHistoryRecords splits bounded decoded bytes into ordered records.
// A trailing partial line is deferred; a malformed complete line keeps its
// byte checkpoint but carries no payload.
func parseCodexHistoryRecords(data []byte) []codexHistoryRecord {
	var records []codexHistoryRecord
	offset := int64(0)
	for offset < int64(len(data)) {
		relative := bytes.IndexByte(data[offset:], '\n')
		end := int64(len(data))
		partial := false
		if relative < 0 {
			partial = true
		} else {
			end = offset + int64(relative) + 1
		}
		line := data[offset:end]
		record := codexHistoryRecord{
			Ordinal:          int64(len(records)),
			ByteStart:        offset,
			ByteEndExclusive: end,
			Partial:          partial,
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && !partial {
			var env codexHistoryEnvelope
			if err := json.Unmarshal(trimmed, &env); err == nil {
				record.EnvelopeType = env.Type
				record.Payload = env.Payload
			} else {
				record.Malformed = true
			}
		}
		records = append(records, record)
		offset = end
	}
	return records
}

// codexDecodedSegment pairs one ordered segment descriptor with its decoded
// bounded bytes and parsed records.
type codexDecodedSegment struct {
	descriptor CodexReference
	isCurrent  bool
	ordinal    int
	data       []byte
	records    []codexHistoryRecord
	// readFailed is set when a bounded dependency could not be read. The
	// segment is retained as incomplete evidence; no refs are invented.
	readFailed bool
}

// effectiveInclusion returns the segment inclusion a reader should record.
func (s codexDecodedSegment) effectiveInclusion() indexformat.SegmentInclusion {
	if s.readFailed {
		return indexformat.SegmentInclusionInvalidIncomplete
	}
	if s.descriptor.Inclusion.IsValid() {
		return s.descriptor.Inclusion
	}
	if s.isCurrent {
		return indexformat.SegmentInclusionSameThreadSurvivingOwn
	}
	return indexformat.SegmentInclusionUncertainEarlierHistory
}

// boundedData returns only the decoded bytes inside the segment's native
// bounds. A parent append past the captured cutoff therefore does not change
// the child's fingerprint.
func (s codexDecodedSegment) boundedData() []byte {
	coords := s.descriptor.Coordinates
	if coords.Start == nil && coords.EndExclusive == nil {
		return s.data
	}
	start := int64(0)
	if coords.Start != nil {
		start = *coords.Start
	}
	end := int64(len(s.data))
	var first, last int64 = -1, -1
	for _, record := range s.records {
		if record.Ordinal < start {
			continue
		}
		if coords.EndExclusive != nil && record.Ordinal >= *coords.EndExclusive {
			continue
		}
		if first < 0 {
			first = record.ByteStart
		}
		last = record.ByteEndExclusive
	}
	if first < 0 {
		return nil
	}
	if last > end {
		last = end
	}
	return s.data[first:last]
}

// replayCodexHistory resolves the authority into ordered decoded segments,
// replays them, and returns the captured history.
func replayCodexHistory(ctx context.Context, source CodexReadOnlySource, authority CodexSourceAuthority, registry *CodexRefRegistry) (CodexCapturedHistory, error) {
	if !authority.Kind.IsValid() {
		return CodexCapturedHistory{}, fmt.Errorf("ingest.replayCodexHistory: authority kind %q is outside the closed set for thread %q; the current source cannot be interpreted; select a published authority kind", authority.Kind, authority.StableThreadID)
	}
	if authority.StableThreadID == "" {
		return CodexCapturedHistory{}, fmt.Errorf("ingest.replayCodexHistory: the authority names no stable session_meta.id; a Codex current source cannot be selected; provide the stable native thread identity")
	}
	mode := resolveCodexHistoryMode(authority.HistoryMode)

	decoded := make([]codexDecodedSegment, 0, len(authority.References)+1)
	diagnostics := []DiagnosticEntry{}
	seenPointers := map[string]bool{authority.CurrentPointer: true}
	for index, ref := range authority.References {
		segment := codexDecodedSegment{descriptor: ref, ordinal: index}
		if seenPointers[ref.Pointer] {
			segment.readFailed = true
			segment.descriptor.Inclusion = indexformat.SegmentInclusionInvalidIncomplete
			diagnostics = append(diagnostics, DiagnosticEntry{
				ErrorType:   "codex_reference_cycle",
				Location:    fmt.Sprintf("thread %s reference %d", authority.StableThreadID, index),
				Message:     "a bounded history dependency points back into the already-resolved source graph; no parent content was guessed or looped",
				Remediation: "Repair the native reference metadata and rerun; the last good generation is retained and no cycle was followed.",
			})
			decoded = append(decoded, segment)
			continue
		}
		seenPointers[ref.Pointer] = true
		if !codexReferenceBoundsValid(ref) {
			segment.readFailed = true
			segment.descriptor.Inclusion = indexformat.SegmentInclusionInvalidIncomplete
			diagnostics = append(diagnostics, DiagnosticEntry{
				ErrorType:   "codex_reference_bounds_invalid",
				Location:    fmt.Sprintf("thread %s reference %d", authority.StableThreadID, index),
				Message:     "a bounded history dependency declares an empty, inverted or overflowing native range; no parent content was guessed",
				Remediation: "Repair the native reference coordinates and rerun; the last good generation is retained.",
			})
			decoded = append(decoded, segment)
			continue
		}
		data, err := source.ReadCodexSource(ctx, ref.Pointer)
		if err != nil {
			segment.readFailed = true
			segment.descriptor.Inclusion = indexformat.SegmentInclusionInvalidIncomplete
			diagnostics = append(diagnostics, DiagnosticEntry{
				ErrorType:   "codex_reference_unavailable",
				Location:    fmt.Sprintf("thread %s reference %d", authority.StableThreadID, index),
				Message:     fmt.Sprintf("a bounded history dependency could not be read: %v", err),
				Remediation: "Restore the referenced native source and rerun; the capture is incomplete and no parent content was guessed or looped.",
			})
			decoded = append(decoded, segment)
			continue
		}
		segment.data = data
		segment.records = parseCodexHistoryRecords(data)
		segment.descriptor.Coordinates = codexRangeCoordinates(segment.records, ref.Coordinates)
		decoded = append(decoded, segment)
	}

	currentRef := CodexReference{
		Pointer:                 authority.CurrentPointer,
		PhysicalSourceID:        authority.PhysicalSourceID,
		Mode:                    mode,
		Inclusion:               indexformat.SegmentInclusionSameThreadSurvivingOwn,
		CopyBoundary:            authority.CopyBoundary,
		OriginalOwnershipProven: authority.OriginalOwnershipProven,
	}
	currentData, err := source.ReadCodexSource(ctx, authority.CurrentPointer)
	if err != nil {
		return CodexCapturedHistory{}, fmt.Errorf("ingest.replayCodexHistory: reading the authoritative current Codex source for thread %s failed: %w; the current pointer is missing, no older rollout was substituted, and the last good generation is retained", authority.StableThreadID, err)
	}
	current := codexDecodedSegment{descriptor: currentRef, isCurrent: true, ordinal: len(decoded), data: currentData}
	current.records = parseCodexHistoryRecords(currentData)
	current.descriptor.Coordinates = codexRangeCoordinates(current.records, indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindCodexOrdinalRange})
	decoded = append(decoded, current)

	state := &codexReplayState{
		registry:          registry,
		responseByItem:    map[string]int{},
		eventOrdinal:      map[string]int64{},
		lastNativeOrdinal: map[string]int64{},
		seenKeys:          map[string]bool{},
	}
	for _, segment := range decoded {
		if err := state.replaySegment(authority.StableThreadID, segment, mode); err != nil {
			return CodexCapturedHistory{}, err
		}
	}

	completeness := indexformat.GenerationCompletenessComplete
	if mode == CodexHistoryModeUnsupported {
		completeness = indexformat.GenerationCompletenessIncompleteNew
		diagnostics = append(diagnostics, DiagnosticEntry{
			ErrorType:   "codex_history_mode_unsupported",
			Location:    fmt.Sprintf("thread %s", authority.StableThreadID),
			Message:     "the native history_mode is null, unknown or malformed; the legacy reducer was not selected",
			Remediation: "Upgrade Peasant to a build that recognizes this Codex history mode; the capture stays incomplete and no legacy fallback was applied.",
		})
	}
	for _, segment := range decoded {
		if segment.readFailed {
			completeness = indexformat.GenerationCompletenessIncompleteNew
			continue
		}
		if segment.isCurrent && segment.descriptor.CopyBoundary != nil {
			boundary := *segment.descriptor.CopyBoundary
			if int64(len(segment.records)) < boundary {
				completeness = indexformat.GenerationCompletenessIncompleteNew
				diagnostics = append(diagnostics, DiagnosticEntry{
					ErrorType:   "codex_copied_prefix_incomplete",
					Location:    fmt.Sprintf("thread %s", authority.StableThreadID),
					Message:     fmt.Sprintf("the persisted copied prefix ends before boundary %d; the missing prefix was not invented", boundary),
					Remediation: "Let the native writer persist the complete prefix and rerun; the existing generation stays active and no success stamp is written.",
				})
			}
		}
	}

	if state.incomplete {
		completeness = indexformat.GenerationCompletenessIncompleteNew
	}

	state.diagnostics = append(diagnostics, state.diagnostics...)
	history := CodexCapturedHistory{
		StableThreadID:   authority.StableThreadID,
		AuthorityKind:    authority.Kind,
		Pointer:          authority.CurrentPointer,
		PhysicalSourceID: authority.PhysicalSourceID,
		Mode:             mode,
		Completeness:     completeness,
		Nodes:            state.nodes,
		Correlations:     state.correlations,
		RawBytes:         currentData,
		Diagnostics:      state.diagnostics,
	}
	history.Segments = buildCodexSegments(decoded, state)
	history.MainRefs = codexRefsForOwnership(state.nodes, CodexOwnershipOwn)
	history.InheritedRefs = codexRefsForOwnership(state.nodes, CodexOwnershipInherited)
	history.EarlierRefs = codexRefsForOwnership(state.nodes, CodexOwnershipUncertainEarlier)
	history.RevertedRefs = codexRefsForOwnership(state.nodes, CodexOwnershipReverted)
	history.CheckpointRefs = state.checkpointRefs(authority.StableThreadID)
	history.Fingerprint = codexFingerprint(authority, completeness, decoded)
	return history, nil
}

// codexRangeCoordinates keeps declared native bounds and derives an ordinal
// range from the record count when the segment carries no explicit bounds.
func codexRangeCoordinates(records []codexHistoryRecord, declared indexformat.SegmentCoordinates) indexformat.SegmentCoordinates {
	switch declared.Kind {
	case indexformat.CoordinateKindSnapshotOnly, indexformat.CoordinateKindUnknown:
		return declared
	case indexformat.CoordinateKindCodexOrdinalRange, indexformat.CoordinateKindCodexReferenceRange, indexformat.CoordinateKindOpenCodeSequenceRange:
		if declared.Start != nil && declared.EndExclusive != nil {
			return declared
		}
	}
	end := int64(len(records))
	start := int64(0)
	return indexformat.SegmentCoordinates{
		Kind:         indexformat.CoordinateKindCodexOrdinalRange,
		Start:        &start,
		EndExclusive: &end,
	}
}

// buildCodexSegments projects the decoded segments and their captured refs into
// the ordered indexformat segment form.
func buildCodexSegments(decoded []codexDecodedSegment, state *codexReplayState) []indexformat.ContextSegment {
	segments := make([]indexformat.ContextSegment, 0, len(decoded))
	for _, segment := range decoded {
		refs := []schema.SourceEntryRef{}
		for _, node := range state.nodes {
			if node.SegmentOrdinal == segment.ordinal {
				refs = append(refs, node.Ref)
			}
		}
		segments = append(segments, indexformat.ContextSegment{
			Ordinal:          segment.ordinal,
			LogicalSessionID: segment.descriptor.LogicalSessionID,
			PhysicalSourceID: segment.descriptor.PhysicalSourceID,
			Coordinates:      segment.descriptor.Coordinates,
			Inclusion:        segment.effectiveInclusion(),
			CapturedRefs:     refs,
		})
	}
	return segments
}

func codexRefsForOwnership(nodes []CodexCapturedNode, ownership CodexOwnership) []schema.SourceEntryRef {
	refs := []schema.SourceEntryRef{}
	for _, node := range nodes {
		if node.Ownership == ownership {
			refs = append(refs, node.Ref)
		}
	}
	return refs
}

// codexReplayState accumulates the captured graph across segments.
type codexReplayState struct {
	registry          *CodexRefRegistry
	nodes             []CodexCapturedNode
	correlations      []CodexCapturedCorrelation
	diagnostics       []DiagnosticEntry
	responseByItem    map[string]int
	eventOrdinal      map[string]int64
	lastNativeOrdinal map[string]int64
	seenKeys          map[string]bool
	turns             [][]int
	checkpoints       []int
	// incomplete records replay evidence that could not be aligned or proved;
	// it forces incomplete_new without inventing a positional pair.
	incomplete bool
}

// replaySegment reduces one decoded segment into the shared state.
func (state *codexReplayState) replaySegment(threadID string, segment codexDecodedSegment, mode CodexHistoryMode) error {
	if segment.readFailed || segment.descriptor.Inclusion == indexformat.SegmentInclusionExcludedReverted {
		return nil
	}
	for _, record := range segment.records {
		if record.Partial {
			continue
		}
		if !codexRecordInBounds(segment, record) {
			continue
		}
		ownership := codexSegmentOwnership(segment, record.Ordinal)
		if ownership == CodexOwnershipReverted {
			continue
		}
		if err := state.replayRecord(threadID, segment, record, ownership, mode); err != nil {
			return err
		}
	}
	return nil
}

// codexReferenceBoundsValid rejects an empty, inverted or half-present native
// range before any dependency bytes are read.
func codexReferenceBoundsValid(ref CodexReference) bool {
	coords := ref.Coordinates
	switch coords.Kind {
	case indexformat.CoordinateKindCodexOrdinalRange, indexformat.CoordinateKindCodexReferenceRange, indexformat.CoordinateKindOpenCodeSequenceRange:
		if coords.Start == nil || coords.EndExclusive == nil {
			return false
		}
		if *coords.Start < 0 || *coords.EndExclusive <= *coords.Start {
			return false
		}
	}
	if (coords.DecodedByteStart == nil) != (coords.DecodedByteEndExclusive == nil) {
		return false
	}
	if coords.DecodedByteStart != nil {
		if *coords.DecodedByteStart < 0 || *coords.DecodedByteEndExclusive <= *coords.DecodedByteStart {
			return false
		}
	}
	return true
}

// codexRecordInBounds applies an explicit native segment bound. A current
// segment and a segment with snapshot-only/unknown coordinates include every
// record.
func codexRecordInBounds(segment codexDecodedSegment, record codexHistoryRecord) bool {
	coords := segment.descriptor.Coordinates
	switch coords.Kind {
	case indexformat.CoordinateKindCodexOrdinalRange, indexformat.CoordinateKindCodexReferenceRange, indexformat.CoordinateKindOpenCodeSequenceRange:
		if coords.Start != nil && record.Ordinal < *coords.Start {
			return false
		}
		if coords.EndExclusive != nil && record.Ordinal >= *coords.EndExclusive {
			return false
		}
	}
	return true
}

// codexSegmentOwnership resolves the native ownership evidence for one record.
func codexSegmentOwnership(segment codexDecodedSegment, ordinal int64) CodexOwnership {
	if segment.isCurrent {
		if segment.descriptor.CopyBoundary != nil && ordinal < *segment.descriptor.CopyBoundary {
			if segment.descriptor.OriginalOwnershipProven {
				return CodexOwnershipInherited
			}
			return CodexOwnershipUncertainEarlier
		}
		return CodexOwnershipOwn
	}
	switch segment.descriptor.Inclusion {
	case indexformat.SegmentInclusionInherited:
		return CodexOwnershipInherited
	case indexformat.SegmentInclusionSameThreadSurvivingOwn:
		return CodexOwnershipOwn
	case indexformat.SegmentInclusionUncertainEarlierHistory:
		return CodexOwnershipUncertainEarlier
	case indexformat.SegmentInclusionExcludedReverted:
		return CodexOwnershipReverted
	case indexformat.SegmentInclusionInvalidIncomplete:
		return CodexOwnershipReverted
	default:
		return CodexOwnershipUncertainEarlier
	}
}

// replayRecord dispatches one non-partial record.
func (state *codexReplayState) replayRecord(threadID string, segment codexDecodedSegment, record codexHistoryRecord, ownership CodexOwnership, mode CodexHistoryMode) error {
	if record.Malformed {
		state.diagnostics = append(state.diagnostics, DiagnosticEntry{
			ErrorType:   "codex_record_malformed",
			Location:    fmt.Sprintf("thread %s ordinal %d", threadID, record.Ordinal),
			Message:     "a complete native record is not valid JSON; its byte checkpoint advanced and no ordinal was assigned by line number",
			Remediation: "Repair the native record and rerun; later valid records are still captured once.",
		})
		return nil
	}
	if record.EnvelopeType == "" {
		if len(bytes.TrimSpace(record.Payload)) == 0 {
			return nil
		}
	}
	var payload codexHistoryReplayPayload
	if len(record.Payload) > 0 {
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			// A malformed complete line advances the byte checkpoint only.
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_record_malformed",
				Location:    fmt.Sprintf("thread %s ordinal %d", threadID, record.Ordinal),
				Message:     fmt.Sprintf("a complete record could not be decoded: %v", err),
				Remediation: "Repair the native record and rerun; the byte checkpoint advanced and no ordinal was assigned by line number.",
			})
			return nil
		}
	}

	switch record.EnvelopeType {
	case codexTypeSessionMeta, codexTypeTurnContext:
		return nil
	case codexTypeResponse:
		return state.replayResponseItem(threadID, segment, record, payload, ownership)
	case codexTypeEventMsg:
		return state.replayEventMessage(threadID, segment, record, payload, ownership, mode)
	case "compacted":
		return state.replayCompaction(threadID, segment, record, payload, ownership)
	default:
		return nil
	}
}

// replayResponseItem emits one captured node for a recognized response_item and
// pairs it with any earlier ItemCompleted event.
func (state *codexReplayState) replayResponseItem(threadID string, segment codexDecodedSegment, record codexHistoryRecord, payload codexHistoryReplayPayload, ownership CodexOwnership) error {
	nativeType, ok := codexResponseNativeType(payload.Type)
	if !ok {
		return nil
	}
	if payload.Ordinal != nil {
		if last, seen := state.lastNativeOrdinal[nativeType]; seen && *payload.Ordinal <= last {
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_ordinal_regression",
				Location:    fmt.Sprintf("thread %s ordinal %d", threadID, record.Ordinal),
				Message:     fmt.Sprintf("a decoded ordinal %d does not advance past %d; the record was skipped", *payload.Ordinal, last),
				Remediation: "Investigate the native writer; the byte checkpoint advanced and later valid items are still captured once.",
			})
			return nil
		}
		state.lastNativeOrdinal[nativeType] = *payload.Ordinal
	}
	if ownership == CodexOwnershipOwn && codexIsUserTurnStart(record.EnvelopeType, payload) {
		state.turns = append(state.turns, nil)
	}
	index, _, err := state.emitNode(threadID, segment, record, ownership, nativeType, payload.Role, payload.ID, payload.CallID, payload.TurnID)
	if err != nil {
		return err
	}
	if payload.ID == "" {
		return nil
	}
	if existing, duplicate := state.responseByItem[payload.ID]; duplicate {
		// A repeated item updates the one existing node rather than adding a
		// duplicate. The same-thread rollover case is a cross-segment reuse and
		// is recorded by emitNode.
		if state.nodes[existing].SegmentOrdinal == segment.ordinal {
			state.correlations = append(state.correlations, CodexCapturedCorrelation{
				Kind:   CodexCorrelationRepeatedItemCompleted,
				ItemID: payload.ID,
				Refs:   []schema.SourceEntryRef{state.nodes[existing].Ref},
			})
		}
		return nil
	}
	state.responseByItem[payload.ID] = index
	if eventOrdinal, paired := state.eventOrdinal[payload.ID]; paired {
		state.correlations = append(state.correlations, CodexCapturedCorrelation{
			Kind:            CodexCorrelationPairedResponseEvent,
			ItemID:          payload.ID,
			EventOrdinal:    codexInt64Ptr(eventOrdinal),
			ResponseOrdinal: codexInt64Ptr(record.Ordinal),
			Refs:            []schema.SourceEntryRef{state.nodes[index].Ref},
		})
	}
	return nil
}

// replayEventMessage handles the event_msg variants that participate in the
// native replay: pairing, turn boundaries and legacy instruction rollback.
func (state *codexReplayState) replayEventMessage(threadID string, segment codexDecodedSegment, record codexHistoryRecord, payload codexHistoryReplayPayload, ownership CodexOwnership, mode CodexHistoryMode) error {
	switch payload.Type {
	case "item_completed":
		if payload.ID == "" {
			return nil
		}
		state.eventOrdinal[payload.ID] = record.Ordinal
		if index, paired := state.responseByItem[payload.ID]; paired {
			state.correlations = append(state.correlations, CodexCapturedCorrelation{
				Kind:            CodexCorrelationRepeatedItemCompleted,
				ItemID:          payload.ID,
				EventOrdinal:    codexInt64Ptr(record.Ordinal),
				ResponseOrdinal: codexInt64Ptr(record.Ordinal),
				Refs:            []schema.SourceEntryRef{state.nodes[index].Ref},
			})
		}
		return nil
	case "thread_rolled_back":
		if mode != CodexHistoryModeLegacy {
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_paginated_rollback_anomaly",
				Location:    fmt.Sprintf("thread %s ordinal %d", threadID, record.Ordinal),
				Message:     "a raw rollback record appeared in a paginated rollout; the paginated projector applies no turn deletion",
				Remediation: "Use the supported paginated revert path; the active history is unchanged and the legacy reducer was not run.",
			})
			return nil
		}
		if payload.NumTurns == nil || *payload.NumTurns <= 0 {
			return nil
		}
		turns := int(*payload.NumTurns)
		if turns > len(state.turns) {
			turns = len(state.turns)
		}
		for _, turn := range state.turns[len(state.turns)-turns:] {
			for _, index := range turn {
				state.nodes[index].Ownership = CodexOwnershipReverted
			}
		}
		state.turns = state.turns[:len(state.turns)-turns]
		return nil
	case "turn_aborted":
		state.diagnostics = append(state.diagnostics, DiagnosticEntry{
			ErrorType:   "codex_turn_aborted",
			Location:    fmt.Sprintf("thread %s ordinal %d", threadID, record.Ordinal),
			Message:     "a native turn was aborted; its records are retained at their native position",
			Remediation: "No action required; the abort is a native control event.",
		})
		return nil
	default:
		return nil
	}
}

// replayCompaction retains archival turns and selects a surviving checkpoint.
// A replacement history is a model baseline, never appended duplicate chat.
func (state *codexReplayState) replayCompaction(threadID string, segment codexDecodedSegment, record codexHistoryRecord, payload codexHistoryReplayPayload, ownership CodexOwnership) error {
	if !codexReplacementHistoryAligned(payload) {
		state.incomplete = true
		state.diagnostics = append(state.diagnostics, DiagnosticEntry{
			ErrorType:   "codex_checkpoint_metadata_misaligned",
			Location:    fmt.Sprintf("thread %s ordinal %d", threadID, record.Ordinal),
			Message:     "the replacement-history and its metadata arrays do not have equal lengths; no positional pairing was invented and the reconstruction is incomplete",
			Remediation: "Repair the native checkpoint metadata and rerun; the existing active generation is retained and the checkpoint summary is not paired.",
		})
	}
	if payload.Summary == nil || *payload.Summary == "" {
		return nil
	}
	index, _, err := state.emitNode(threadID, segment, record, ownership, "compacted_summary", "system", "", "", "")
	if err != nil {
		return err
	}
	state.checkpoints = append(state.checkpoints, index)
	return nil
}

// codexReplacementHistoryAligned reports whether the native replacement-history
// and replacement-history-metadata arrays can be paired. Absent arrays are
// supported; two present arrays must have equal lengths.
func codexReplacementHistoryAligned(payload codexHistoryReplayPayload) bool {
	history, historyPresent := codexArrayLen(payload.ReplacementHistory)
	metadata, metadataPresent := codexArrayLen(payload.ReplacementMetadata)
	if !historyPresent && !metadataPresent {
		return true
	}
	if historyPresent != metadataPresent {
		return false
	}
	return history == metadata
}

// codexArrayLen returns the length of a present JSON array. A null, absent or
// non-array value is not present.
func codexArrayLen(raw json.RawMessage) (int, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return 0, false
	}
	return len(values), true
}

// emitNode allocates the stable ref for a native key and appends the node. A
// key seen before is one logical item: the node is not duplicated, and the
// reuse is recorded as the same-thread rollover correlation. The returned index
// is the node's position in the captured graph.
func (state *codexReplayState) emitNode(threadID string, segment codexDecodedSegment, record codexHistoryRecord, ownership CodexOwnership, nativeType, role, itemID, callID, turnID string) (int, bool, error) {
	key := codexNativeKey(threadID, segment, record, nativeType)
	if state.seenKeys[key] {
		ref, _, err := state.registry.RefFor(key)
		if err != nil {
			return 0, false, err
		}
		state.correlations = append(state.correlations, CodexCapturedCorrelation{
			Kind:   CodexCorrelationSameThreadRollover,
			ItemID: itemID,
			Refs:   []schema.SourceEntryRef{ref},
		})
		return state.nodeIndexForRef(ref), true, nil
	}
	ref, _, err := state.registry.RefFor(key)
	if err != nil {
		return 0, false, err
	}
	state.seenKeys[key] = true
	state.nodes = append(state.nodes, CodexCapturedNode{
		NativeKey:        key,
		Ref:              ref,
		SegmentOrdinal:   segment.ordinal,
		Ordinal:          record.Ordinal,
		ByteStart:        record.ByteStart,
		ByteEndExclusive: record.ByteEndExclusive,
		EnvelopeType:     record.EnvelopeType,
		NativeType:       nativeType,
		NativeRole:       role,
		ItemID:           itemID,
		CallID:           callID,
		TurnID:           turnID,
		Ownership:        ownership,
	})
	if codexOwnershipIsOwn(ownership) {
		state.turns = appendTurnNode(state.turns, len(state.nodes)-1)
	}
	return len(state.nodes) - 1, false, nil
}

// nodeIndexForRef returns the captured node that owns one ref.
func (state *codexReplayState) nodeIndexForRef(ref schema.SourceEntryRef) int {
	for index, node := range state.nodes {
		if node.Ref == ref {
			return index
		}
	}
	return -1
}

// appendTurnNode appends a node index to the open turn, opening one when the
// turn stream is empty.
func appendTurnNode(turns [][]int, index int) [][]int {
	if len(turns) == 0 {
		return append(turns, []int{index})
	}
	turns[len(turns)-1] = append(turns[len(turns)-1], index)
	return turns
}

func codexOwnershipIsOwn(ownership CodexOwnership) bool {
	return ownership == CodexOwnershipOwn
}

// checkpointRefs returns the refs of surviving checkpoint summaries. A
// checkpoint inside a rolled-back turn was marked reverted and is excluded.
func (state *codexReplayState) checkpointRefs(threadID string) []schema.SourceEntryRef {
	refs := []schema.SourceEntryRef{}
	for _, index := range state.checkpoints {
		node := state.nodes[index]
		if node.Ownership == CodexOwnershipOwn && node.NativeType == "compacted_summary" {
			refs = append(refs, node.Ref)
		}
	}
	_ = threadID
	return refs
}

// codexResponseNativeType maps a response_item payload type to the bounded
// native node type.
func codexResponseNativeType(payloadType string) (string, bool) {
	switch payloadType {
	case codexResponseMessage:
		return "message", true
	case codexResponseReasoning:
		return "reasoning", true
	case codexResponseFunctionCall:
		return "function_call", true
	case codexResponseCustomCall:
		return "custom_tool_call", true
	case codexResponseFunctionOut:
		return "function_call_output", true
	case codexResponseCustomCallOut:
		return "custom_tool_call_output", true
	default:
		return "", false
	}
}

// codexIsUserTurnStart reports whether a recognized record opens a new turn
// under the pinned legacy instruction-boundary rule. Inter-agent deliveries are
// their own boundaries and are handled by the classifier; the replay opens a
// turn for them too so rollback does not absorb agent deliveries into a user
// turn.
func codexIsUserTurnStart(envelopeType string, payload codexHistoryReplayPayload) bool {
	if envelopeType != codexTypeResponse {
		return false
	}
	if payload.Type != codexResponseMessage {
		return false
	}
	return payload.Role == "user"
}

// codexNativeKey builds the bounded local identity the ref registry keys on. A
// surviving native item id is scoped to the stable logical thread, never to the
// physical rollout path, so a same-thread rollover reuses the same ref.
func codexNativeKey(threadID string, segment codexDecodedSegment, record codexHistoryRecord, nativeType string) string {
	if record.Payload != nil {
		var payload codexHistoryReplayPayload
		if json.Unmarshal(record.Payload, &payload) == nil && payload.ID != "" {
			return fmt.Sprintf("codex|%s|item|%s|%s", threadID, payload.ID, nativeType)
		}
	}
	return fmt.Sprintf("codex|%s|pos|%s|%d|%s", threadID, segment.descriptor.PhysicalSourceID, record.ByteStart, nativeType)
}

func codexInt64Ptr(value int64) *int64 { return &value }
