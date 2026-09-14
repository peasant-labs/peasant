package ingest

// Deterministic replay of captured Codex records into an adapter-private native
// node graph. The replay applies the recognized history modes, the copied
// creation boundary, migration uncertainty, instruction rollback, surviving
// checkpoints, paginated revert, canonical paginated items and lifecycle
// state, and byte/ordinal checkpoints. It classifies no block and allocates
// no durable generation rows.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// codexHistoryEnvelope is the outer envelope shared by every Codex rollout
// record. Metadata is the history-envelope metadata recorded beside the
// payload; the classifier reads it, the replay preserves it.
type codexHistoryEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Metadata  json.RawMessage `json:"metadata"`
}

// codexDeliveryCorrelation is the explicit native inter-agent admission
// evidence the frozen legacy boundary reducer keys on: a target, a call
// correlation, or both. Absence is not negative evidence.
type codexDeliveryCorrelation struct {
	Target string `json:"target"`
	CallID string `json:"call_id"`
}

func (d *codexDeliveryCorrelation) isCorrelated() bool {
	return d != nil && (d.Target != "" || d.CallID != "")
}

// codexItemBody is the canonical paginated item carried by an item lifecycle
// event, unioned over the native TurnItem variants and the ResponseItem-shaped
// legacy items. Every payload-bearing field is raw so the captured node keeps
// the exact native bytes; the adapter-private native type is derived from the
// native discriminator, never from the presence of one text field.
type codexItemBody struct {
	Type             string                    `json:"type"`
	Role             string                    `json:"role"`
	ID               string                    `json:"id"`
	CallID           string                    `json:"call_id"`
	TurnID           string                    `json:"turn_id"`
	Name             string                    `json:"name"`
	Content          json.RawMessage           `json:"content"`
	Summary          json.RawMessage           `json:"summary"`
	SummaryText      json.RawMessage           `json:"summary_text"`
	RawContent       json.RawMessage           `json:"raw_content"`
	Output           json.RawMessage           `json:"output"`
	Arguments        json.RawMessage           `json:"arguments"`
	Input            json.RawMessage           `json:"input"`
	Command          json.RawMessage           `json:"command"`
	AggregatedOutput json.RawMessage           `json:"aggregated_output"`
	Stdout           json.RawMessage           `json:"stdout"`
	Changes          json.RawMessage           `json:"changes"`
	Delivery         *codexDeliveryCorrelation `json:"delivery"`
}

// codexItemBodyCarriesPayload reports whether an item body carries any native
// payload field. A body with a recognized native discriminator and no payload
// field is still a valid item (for example a context-compaction item) and is
// emitted from its identity; the predicate exists only to distinguish a body
// carrying its own content from an unrecognized body on the marker path.
func codexItemBodyCarriesPayload(body codexItemBody) bool {
	for _, raw := range []json.RawMessage{
		body.Content, body.Summary, body.SummaryText, body.RawContent,
		body.Output, body.Arguments, body.Input, body.Command,
		body.AggregatedOutput, body.Stdout, body.Changes,
	} {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
			return true
		}
	}
	return false
}

// codexItemNativeType maps a canonical item body discriminator to the bounded
// native node type and native role. It recognizes both the canonical TurnItem
// variants (UserMessage, AgentMessage, Reasoning, CommandExecution, ...) and
// the ResponseItem-shaped variants the same item can carry. An unrecognized
// discriminator is not a native item and returns ok=false.
func codexItemNativeType(body codexItemBody) (nativeType, role string, ok bool) {
	if mapped, known := codexResponseNativeType(body.Type); known {
		return mapped, body.Role, true
	}
	switch body.Type {
	case "UserMessage":
		return "message", "user", true
	case "AgentMessage":
		return "message", "assistant", true
	case "Reasoning":
		return "reasoning", "assistant", true
	case "FunctionCallOutput":
		return "function_call_output", "tool", true
	case "CommandExecution":
		return "command_execution", "assistant", true
	case "FileChange":
		return "file_change", "assistant", true
	case "SubAgentActivity":
		return "sub_agent_activity", "assistant", true
	case "CollabAgentToolCall":
		return "collab_agent_tool_call", "assistant", true
	case "ContextCompaction":
		return "context_compaction", "system", true
	case "Extension":
		return "extension", "assistant", true
	case "Plan":
		return "plan", "assistant", true
	case "HookPrompt":
		return "hook_prompt", "system", true
	case "WebSearch":
		return "web_search", "assistant", true
	case "ImageView":
		return "image_view", "user", true
	case "ImageGeneration":
		return "image_generation", "assistant", true
	case "McpToolCall":
		return "mcp_tool_call", "assistant", true
	case "DynamicToolCall":
		return "dynamic_tool_call", "assistant", true
	case "EnteredReviewMode":
		return "entered_review_mode", "system", true
	case "ExitedReviewMode":
		return "exited_review_mode", "system", true
	default:
		return "", "", false
	}
}

// codexItemIsAdmission reports whether an item lifecycle body is a native
// instruction admission: a user message or an inter-agent delivery. Only a
// native admission opens a legacy instruction turn; assistant/tool output stays
// inside its admission's turn.
func codexItemIsAdmission(event codexHistoryReplayPayload, body codexItemBody, nativeType string) bool {
	if nativeType == "message" && body.Role == "user" {
		return true
	}
	if body.Type == "UserMessage" {
		return true
	}
	if event.Delivery.isCorrelated() || body.Delivery.isCorrelated() {
		return true
	}
	return false
}

// codexHistoryReplayPayload is the union of replay-relevant fields on a Codex
// payload. It is discriminated by the envelope type and the nested payload
// type; unrelated fields stay zero.
type codexHistoryReplayPayload struct {
	Type     string                    `json:"type"`
	Role     string                    `json:"role"`
	ID       string                    `json:"id"`
	ItemID   string                    `json:"item_id"`
	CallID   string                    `json:"call_id"`
	TurnID   string                    `json:"turn_id"`
	Name     string                    `json:"name"`
	Summary  *string                   `json:"summary"`
	NumTurns *int64                    `json:"num_turns"`
	Ordinal  *int64                    `json:"ordinal"`
	Content  json.RawMessage           `json:"content"`
	Item     json.RawMessage           `json:"item"`
	Delivery *codexDeliveryCorrelation `json:"delivery"`
	// ReplacementHistory and ReplacementMetadata are the native compaction
	// baseline arrays. History without metadata is accepted; orphan or
	// malformed present metadata, and unequal present arrays, mark
	// reconstruction incomplete and no positional pair is invented.
	ReplacementHistory  json.RawMessage `json:"replacement_history"`
	ReplacementMetadata json.RawMessage `json:"replacement_history_metadata"`
}

// codexHistoryRecord is one parsed record with its physical line position,
// its valid decoded native ordinal and its decoded byte coordinates.
// Partial, malformed and unknown records carry no decoded ordinal: they
// advance the byte checkpoint only and are never assigned a native ordinal
// by line number.
type codexHistoryRecord struct {
	LineIndex int64
	// Ordinal is the valid decoded native ordinal. HasOrdinal is false for
	// partial, malformed, blank and unknown records.
	Ordinal          int64
	HasOrdinal       bool
	Regressed        bool
	Blank            bool
	ByteStart        int64
	ByteEndExclusive int64
	EnvelopeType     string
	Payload          json.RawMessage
	Metadata         json.RawMessage
	// Partial marks a trailing record without a terminating newline. It is
	// deferred and never interpreted.
	Partial bool
	// Malformed marks a complete record whose bytes could not be decoded
	// into the replay graph. The byte checkpoint still advances.
	Malformed bool
}

// recognizedCodexEnvelopeType reports whether an envelope type advances the
// decoded native ordinal checkpoint. Session and turn headers advance the
// checkpoint even though they never become content nodes.
func recognizedCodexEnvelopeType(envelopeType string) bool {
	switch envelopeType {
	case codexTypeSessionMeta, codexTypeTurnContext, codexTypeResponse, codexTypeEventMsg, "compacted":
		return true
	default:
		return false
	}
}

// parseCodexHistoryRecords splits bounded decoded bytes into ordered records
// with valid decoded native ordinals. A trailing partial line is deferred; a
// malformed complete line, a blank line and an unknown envelope type keep
// their byte checkpoint but carry no native ordinal. An explicit numeric
// payload ordinal is honored when it advances; a regressed or duplicate one
// is flagged and skipped, and gaps are preserved, never filled.
func parseCodexHistoryRecords(data []byte) []codexHistoryRecord {
	var records []codexHistoryRecord
	var maxAssigned int64 = -1
	offset := int64(0)
	line := int64(0)
	for offset < int64(len(data)) {
		relative := bytes.IndexByte(data[offset:], '\n')
		end := int64(len(data))
		partial := false
		if relative < 0 {
			partial = true
		} else {
			end = offset + int64(relative) + 1
		}
		recordLine := data[offset:end]
		record := codexHistoryRecord{
			LineIndex:        line,
			ByteStart:        offset,
			ByteEndExclusive: end,
			Partial:          partial,
		}
		line++
		trimmed := bytes.TrimSpace(recordLine)
		switch {
		case partial:
			// Deferred: no ordinal, no interpretation.
		case len(trimmed) == 0:
			record.Blank = true
		default:
			var env codexHistoryEnvelope
			if err := json.Unmarshal(trimmed, &env); err != nil {
				record.Malformed = true
			} else if env.Type == "" && len(bytes.TrimSpace(env.Payload)) == 0 && len(bytes.TrimSpace(env.Metadata)) == 0 {
				record.Blank = true
			} else if !recognizedCodexEnvelopeType(env.Type) {
				record.EnvelopeType = env.Type
				record.Payload = env.Payload
				record.Metadata = env.Metadata
			} else {
				record.EnvelopeType = env.Type
				record.Payload = env.Payload
				record.Metadata = env.Metadata
				if explicit, present := codexExplicitRecordOrdinal(env.Payload); present {
					switch {
					case explicit < 0:
						record.Malformed = true
					case explicit <= maxAssigned:
						record.Regressed = true
					default:
						record.Ordinal = explicit
						record.HasOrdinal = true
						maxAssigned = explicit
					}
				} else {
					record.Ordinal = maxAssigned + 1
					record.HasOrdinal = true
					maxAssigned = record.Ordinal
				}
			}
		}
		records = append(records, record)
		offset = end
	}
	return records
}

// codexExplicitRecordOrdinal reads an explicit numeric payload ordinal when
// one is present. A non-numeric or absent ordinal is not present; only a
// numeric value participates in checkpoint advancement.
func codexExplicitRecordOrdinal(payload json.RawMessage) (int64, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	var probe struct {
		Ordinal json.RawMessage `json:"ordinal"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return 0, false
	}
	if len(bytes.TrimSpace(probe.Ordinal)) == 0 {
		return 0, false
	}
	var ordinal int64
	if err := json.Unmarshal(probe.Ordinal, &ordinal); err != nil {
		return 0, false
	}
	return ordinal, true
}

// codexRecordLocation names a record for diagnostics using its decoded
// ordinal when it has one and its physical line otherwise.
func codexRecordLocation(threadID string, record codexHistoryRecord) string {
	if record.HasOrdinal {
		return fmt.Sprintf("thread %s decoded ordinal %d (line %d)", threadID, record.Ordinal, record.LineIndex)
	}
	return fmt.Sprintf("thread %s line %d", threadID, record.LineIndex)
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
	// proofFailed is set when the dependency bytes were read but the
	// required native proof (mode, ordinal coverage, byte boundaries)
	// failed. The proven valid records still replay; the capture records
	// incompleteness and the caller retains last-good state.
	proofFailed bool
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

// codexDecodedCheckpoint is the valid decoded native checkpoint of one
// segment: how many records carry native ordinals and the highest one.
// Unknown, malformed, blank and partial lines never contribute.
type codexDecodedCheckpoint struct {
	validRecords int64
	maxOrdinal   int64
	hasValid     bool
}

// decodedCheckpoint folds the segment records into their native checkpoint.
func (s codexDecodedSegment) decodedCheckpoint() codexDecodedCheckpoint {
	var checkpoint codexDecodedCheckpoint
	checkpoint.maxOrdinal = -1
	for _, record := range s.records {
		if !record.HasOrdinal {
			continue
		}
		checkpoint.validRecords++
		if !checkpoint.hasValid || record.Ordinal > checkpoint.maxOrdinal {
			checkpoint.maxOrdinal = record.Ordinal
			checkpoint.hasValid = true
		}
	}
	return checkpoint
}

// maxOrdinalPtr returns the highest valid decoded ordinal, or nil when the
// segment proves no valid record.
func (c codexDecodedCheckpoint) maxOrdinalPtr() *int64 {
	if !c.hasValid {
		return nil
	}
	ordinal := c.maxOrdinal
	return &ordinal
}

// boundedCheckpoint folds only the records inside the segment's native
// bounds into a checkpoint. A parent append past the captured cutoff never
// moves it, so the child fingerprint is stable across parent growth.
func (s codexDecodedSegment) boundedCheckpoint() codexDecodedCheckpoint {
	var checkpoint codexDecodedCheckpoint
	checkpoint.maxOrdinal = -1
	for _, record := range s.records {
		if !record.HasOrdinal {
			continue
		}
		if !codexRecordInBounds(s, record, nil) {
			continue
		}
		checkpoint.validRecords++
		if !checkpoint.hasValid || record.Ordinal > checkpoint.maxOrdinal {
			checkpoint.maxOrdinal = record.Ordinal
			checkpoint.hasValid = true
		}
	}
	return checkpoint
}

// partialTail returns the deferred trailing partial bytes, if any.
func (s codexDecodedSegment) partialTail() []byte {
	for _, record := range s.records {
		if record.Partial {
			return s.data[record.ByteStart:record.ByteEndExclusive]
		}
	}
	return nil
}

// boundedData returns only the decoded bytes inside the segment's native
// bounds, selected by valid decoded ordinals. Malformed, unknown, blank and
// partial lines never move a bound. A parent append past the captured cutoff
// therefore does not change the child's fingerprint.
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
		if !record.HasOrdinal {
			continue
		}
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
// proves reference coverage, replays them, and returns the captured history.
func replayCodexHistory(ctx context.Context, source CodexReadOnlySource, authority CodexSourceAuthority, registry *CodexRefRegistry) (CodexCapturedHistory, error) {
	if !authority.Kind.IsValid() {
		return CodexCapturedHistory{}, fmt.Errorf("ingest.replayCodexHistory: authority kind %q is outside the closed set for thread %q; the current source cannot be interpreted; select a published authority kind", authority.Kind, authority.StableThreadID)
	}
	if authority.StableThreadID == "" {
		return CodexCapturedHistory{}, fmt.Errorf("ingest.replayCodexHistory: the authority names no stable session_meta.id; a Codex current source cannot be selected; provide the stable native thread identity")
	}
	mode := resolveCodexHistoryMode(authority.HistoryMode)

	decoded := make([]codexDecodedSegment, 0, len(authority.References)+1)
	diagnostics := append([]DiagnosticEntry(nil), authority.DerivationDiagnostics...)
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
				Message:     "a bounded history dependency could not be read; the capture is incomplete and no parent content was guessed or looped",
				Remediation: "Restore the referenced native source and rerun; the last good generation is retained.",
			})
			decoded = append(decoded, segment)
			continue
		}
		segment.data = data
		segment.records = parseCodexHistoryRecords(data)
		segment.descriptor.Coordinates = codexRangeCoordinates(segment.records, ref.Coordinates)
		if proof, ok := codexProveReference(segment, mode, authority.StableThreadID, index); !ok {
			segment.proofFailed = true
			diagnostics = append(diagnostics, proof...)
		}
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
		return CodexCapturedHistory{}, &CodexCurrentMissingError{
			StableThreadID: authority.StableThreadID,
			SourceRef:      authority.PhysicalSourceID,
		}
	}
	current := codexDecodedSegment{descriptor: currentRef, isCurrent: true, ordinal: len(decoded), data: currentData}
	current.records = parseCodexHistoryRecords(currentData)
	current.descriptor.Coordinates = codexRangeCoordinates(current.records, indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindCodexOrdinalRange})
	decoded = append(decoded, current)

	state := &codexReplayState{
		registry:       registry,
		responseByItem: map[string]int{},
		itemNodeByID:   map[string]int{},
		eventOrdinal:   map[string]int64{},
		seenKeys:       map[string]bool{},
		openTurns:      map[string]int64{},
		boundary:       &codexLegacyBoundaryReducer{pending: map[string]bool{}},
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
	if authority.DerivationIncomplete {
		completeness = indexformat.GenerationCompletenessIncompleteNew
	}
	for _, segment := range decoded {
		if segment.readFailed || segment.proofFailed {
			completeness = indexformat.GenerationCompletenessIncompleteNew
			continue
		}
		if segment.isCurrent && segment.descriptor.CopyBoundary != nil {
			if segment.decodedCheckpoint().validRecords < *segment.descriptor.CopyBoundary {
				completeness = indexformat.GenerationCompletenessIncompleteNew
				diagnostics = append(diagnostics, DiagnosticEntry{
					ErrorType:   "codex_copied_prefix_incomplete",
					Location:    fmt.Sprintf("thread %s", authority.StableThreadID),
					Message:     fmt.Sprintf("the persisted copied prefix ends before boundary %d; the missing prefix was not invented", *segment.descriptor.CopyBoundary),
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
	history.CapturedSegments = buildCodexCapturedSegments(decoded)
	history.MainRefs = codexRefsForOwnership(state.nodes, CodexOwnershipOwn)
	history.InheritedRefs = codexRefsForOwnership(state.nodes, CodexOwnershipInherited)
	history.EarlierRefs = codexRefsForOwnership(state.nodes, CodexOwnershipUncertainEarlier)
	history.RevertedRefs = codexRefsForOwnership(state.nodes, CodexOwnershipReverted)
	history.CheckpointRefs = state.checkpointRefs(authority.StableThreadID)
	history.Fingerprint = codexFingerprint(authority, completeness, decoded)
	return history, nil
}

// codexProveReference proves the required native evidence of one decoded
// dependency against its own decoded checkpoint: the referenced mode must be
// paginated under a paginated current, the ordinal bounds must land inside
// the proven valid records, and decoded-byte bounds must land on valid
// record boundaries without overflowing the decoded bytes. Missing proof
// returns the incompleteness diagnostics; the proven valid prefix still
// replays under a clamped bound.
func codexProveReference(segment codexDecodedSegment, currentMode CodexHistoryMode, threadID string, index int) ([]DiagnosticEntry, bool) {
	location := fmt.Sprintf("thread %s reference %d", threadID, index)
	if segment.descriptor.HistoryKind == "through" && !segment.descriptor.ThroughCompleted {
		return []DiagnosticEntry{{
			ErrorType:   "codex_reference_coverage_incomplete",
			Location:    location,
			Message:     "a through history dependency names a running target; only a completed target proves the through bound",
			Remediation: "Let the native writer complete the referenced target and rerun; the proven prefix replays and the last good generation is retained.",
		}}, false
	}
	if currentMode == CodexHistoryModePaginated && segment.descriptor.Mode != CodexHistoryModePaginated {
		return []DiagnosticEntry{{
			ErrorType:   "codex_reference_mode_unsupported",
			Location:    location,
			Message:     fmt.Sprintf("a bounded history dependency declares mode %q under a paginated current; the legacy dependency was not replayed as paginated history", segment.descriptor.Mode),
			Remediation: "Migrate the referenced native history to the paginated shape and rerun; the capture stays incomplete and the proven prefix replays.",
		}}, false
	}
	coords := segment.descriptor.Coordinates
	switch coords.Kind {
	case indexformat.CoordinateKindCodexOrdinalRange, indexformat.CoordinateKindCodexReferenceRange, indexformat.CoordinateKindOpenCodeSequenceRange:
		checkpoint := segment.decodedCheckpoint()
		if coords.Start == nil || coords.EndExclusive == nil {
			return []DiagnosticEntry{{
				ErrorType:   "codex_reference_coverage_incomplete",
				Location:    location,
				Message:     "a bounded history dependency declares no provable ordinal range; the missing proof was not invented",
				Remediation: "Repair the native reference coordinates and rerun; the last good generation is retained.",
			}}, false
		}
		if !checkpoint.hasValid || *coords.EndExclusive > checkpoint.maxOrdinal+1 || *coords.Start > checkpoint.maxOrdinal {
			return []DiagnosticEntry{{
				ErrorType:   "codex_reference_coverage_incomplete",
				Location:    location,
				Message:     fmt.Sprintf("a bounded history dependency requires decoded ordinals [%d,%d) but the referenced bytes prove only %d valid records; the missing records were not invented", *coords.Start, *coords.EndExclusive, checkpoint.validRecords),
				Remediation: "Let the native writer persist the complete referenced prefix and rerun; the proven prefix replays and the last good generation is retained.",
			}}, false
		}
	}
	if coords.DecodedByteStart != nil && coords.DecodedByteEndExclusive != nil {
		if *coords.DecodedByteEndExclusive > int64(len(segment.data)) {
			return []DiagnosticEntry{{
				ErrorType:   "codex_reference_byte_boundary_invalid",
				Location:    location,
				Message:     "a bounded history dependency declares a decoded-byte bound past the end of the referenced bytes; the missing bytes were not invented",
				Remediation: "Repair the native reference byte coordinates and rerun; the proven prefix replays and the last good generation is retained.",
			}}, false
		}
		if !codexByteBoundariesAlign(segment, *coords.DecodedByteStart, *coords.DecodedByteEndExclusive) {
			return []DiagnosticEntry{{
				ErrorType:   "codex_reference_byte_boundary_invalid",
				Location:    location,
				Message:     "a bounded history dependency declares decoded-byte bounds that do not land on valid decoded record boundaries; unaligned bytes were not claimed",
				Remediation: "Repair the native reference byte coordinates and rerun; the proven prefix replays and the last good generation is retained.",
			}}, false
		}
	}
	return nil, true
}

// codexByteBoundariesAlign reports whether decoded-byte bounds land on valid
// decoded record boundaries. The file start is always a boundary; any other
// bound must equal a valid record edge.
func codexByteBoundariesAlign(segment codexDecodedSegment, start, end int64) bool {
	if start != 0 && !codexIsValidRecordEdge(segment, start, true) {
		return false
	}
	if !codexIsValidRecordEdge(segment, end, false) {
		return false
	}
	return true
}

// codexIsValidRecordEdge reports whether offset is the start (or end) of a
// record that carries a valid decoded native ordinal.
func codexIsValidRecordEdge(segment codexDecodedSegment, offset int64, wantStart bool) bool {
	for _, record := range segment.records {
		if !record.HasOrdinal {
			continue
		}
		if wantStart && record.ByteStart == offset {
			return true
		}
		if !wantStart && record.ByteEndExclusive == offset {
			return true
		}
	}
	return false
}

// codexRangeCoordinates keeps declared native bounds and derives an ordinal
// range from the valid decoded checkpoint when the segment carries no
// explicit bounds.
func codexRangeCoordinates(records []codexHistoryRecord, declared indexformat.SegmentCoordinates) indexformat.SegmentCoordinates {
	switch declared.Kind {
	case indexformat.CoordinateKindSnapshotOnly, indexformat.CoordinateKindUnknown:
		return declared
	case indexformat.CoordinateKindCodexOrdinalRange, indexformat.CoordinateKindCodexReferenceRange, indexformat.CoordinateKindOpenCodeSequenceRange:
		if declared.Start != nil && declared.EndExclusive != nil {
			return declared
		}
	}
	var maxOrdinal int64 = -1
	var hasValid bool
	for _, record := range records {
		if !record.HasOrdinal {
			continue
		}
		if !hasValid || record.Ordinal > maxOrdinal {
			maxOrdinal = record.Ordinal
			hasValid = true
		}
	}
	end := maxOrdinal + 1
	if !hasValid {
		end = 0
	}
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

// buildCodexCapturedSegments exposes every decoded segment with its bounded
// bytes and per-record payload plus adjacent envelope metadata, so the
// classifier consumes the verified capture without reopening native sources.
func buildCodexCapturedSegments(decoded []codexDecodedSegment) []CodexCapturedSegment {
	segments := make([]CodexCapturedSegment, 0, len(decoded))
	for _, segment := range decoded {
		captured := CodexCapturedSegment{
			Ordinal:    segment.ordinal,
			Descriptor: segment.descriptor,
			Data:       segment.boundedData(),
		}
		for _, record := range segment.records {
			captured.Records = append(captured.Records, CodexCapturedRecord{
				LineIndex:        record.LineIndex,
				ByteStart:        record.ByteStart,
				ByteEndExclusive: record.ByteEndExclusive,
				EnvelopeType:     record.EnvelopeType,
				Payload:          record.Payload,
				Metadata:         record.Metadata,
				Partial:          record.Partial,
				Malformed:        record.Malformed,
			})
			if record.HasOrdinal {
				ordinal := record.Ordinal
				captured.Records[len(captured.Records)-1].DecodedOrdinal = &ordinal
			}
		}
		segments = append(segments, captured)
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
	registry       *CodexRefRegistry
	nodes          []CodexCapturedNode
	correlations   []CodexCapturedCorrelation
	diagnostics    []DiagnosticEntry
	responseByItem map[string]int
	itemNodeByID   map[string]int
	eventOrdinal   map[string]int64
	seenKeys       map[string]bool
	openTurns      map[string]int64
	boundary       *codexLegacyBoundaryReducer
	turns          [][]int
	checkpoints    []int
	// incomplete records replay evidence that could not be aligned or proved;
	// it forces incomplete_new without inventing a positional pair.
	incomplete bool
}

// replaySegment reduces one decoded segment into the shared state. A segment
// whose proof failed still replays its proven valid records under a clamped
// bound; records past the proven checkpoint are never claimed.
func (state *codexReplayState) replaySegment(threadID string, segment codexDecodedSegment, mode CodexHistoryMode) error {
	if segment.readFailed || segment.descriptor.Inclusion == indexformat.SegmentInclusionExcludedReverted {
		return nil
	}
	var clampEnd *int64
	if segment.proofFailed {
		checkpoint := segment.decodedCheckpoint()
		if checkpoint.hasValid {
			end := checkpoint.maxOrdinal + 1
			clampEnd = &end
		} else {
			return nil
		}
	}
	for _, record := range segment.records {
		if record.Partial || record.Blank {
			continue
		}
		if record.Malformed {
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_record_malformed",
				Location:    codexRecordLocation(threadID, record),
				Message:     "a complete native record could not be decoded into the replay graph; its byte checkpoint advanced and no native ordinal was assigned by line number",
				Remediation: "Repair the native record and rerun; later valid records are still captured once.",
			})
			continue
		}
		if record.Regressed {
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_ordinal_regression",
				Location:    codexRecordLocation(threadID, record),
				Message:     "an explicit decoded ordinal does not advance past the segment checkpoint; the record was skipped and the byte checkpoint advanced",
				Remediation: "Investigate the native writer; later valid items are still captured once.",
			})
			continue
		}
		if !record.HasOrdinal {
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_record_unknown",
				Location:    codexRecordLocation(threadID, record),
				Message:     "a complete native record has an unrecognized envelope type; its byte checkpoint advanced and no native ordinal was assigned",
				Remediation: "Upgrade Peasant to a build that recognizes this Codex record type; later valid records are still captured once.",
			})
			continue
		}
		if !codexRecordInBounds(segment, record, clampEnd) {
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

// codexRecordInBounds applies an explicit native segment bound in valid
// decoded-ordinal space, optionally clamped to the proven checkpoint. A
// current segment and a segment with snapshot-only/unknown coordinates
// include every valid record.
func codexRecordInBounds(segment codexDecodedSegment, record codexHistoryRecord, clampEnd *int64) bool {
	coords := segment.descriptor.Coordinates
	switch coords.Kind {
	case indexformat.CoordinateKindCodexOrdinalRange, indexformat.CoordinateKindCodexReferenceRange, indexformat.CoordinateKindOpenCodeSequenceRange:
		if coords.Start != nil && record.Ordinal < *coords.Start {
			return false
		}
		if coords.EndExclusive != nil && record.Ordinal >= *coords.EndExclusive {
			return false
		}
		if clampEnd != nil && record.Ordinal >= *clampEnd {
			return false
		}
	}
	return true
}

// codexSegmentOwnership resolves the native ownership evidence for one valid
// decoded ordinal.
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

// replayRecord dispatches one valid decoded record.
func (state *codexReplayState) replayRecord(threadID string, segment codexDecodedSegment, record codexHistoryRecord, ownership CodexOwnership, mode CodexHistoryMode) error {
	var payload codexHistoryReplayPayload
	if len(record.Payload) > 0 {
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			// A malformed complete line advances the byte checkpoint only.
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_record_malformed",
				Location:    codexRecordLocation(threadID, record),
				Message:     "a complete record payload could not be decoded; the byte checkpoint advanced and no native ordinal beyond its position was assigned by line number",
				Remediation: "Repair the native record and rerun; later valid records are still captured once.",
			})
			return nil
		}
	}

	switch record.EnvelopeType {
	case codexTypeSessionMeta, codexTypeTurnContext:
		return nil
	case codexTypeResponse:
		return state.replayResponseItem(threadID, segment, record, payload, ownership, mode)
	case codexTypeEventMsg:
		return state.replayEventMessage(threadID, segment, record, payload, ownership, mode)
	case "compacted":
		return state.replayCompaction(threadID, segment, record, payload, ownership)
	default:
		return nil
	}
}

// replayResponseItem emits one captured node for a recognized response_item,
// pairs it with any earlier item lifecycle event, and admits legacy
// instruction turns through the frozen boundary reducer.
func (state *codexReplayState) replayResponseItem(threadID string, segment codexDecodedSegment, record codexHistoryRecord, payload codexHistoryReplayPayload, ownership CodexOwnership, mode CodexHistoryMode) error {
	nativeType, ok := codexResponseNativeType(payload.Type)
	if !ok {
		return nil
	}
	if mode == CodexHistoryModeLegacy && ownership == CodexOwnershipOwn && state.boundary.admitResponse(payload) {
		state.turns = append(state.turns, nil)
	}
	if payload.ID == "" {
		_, _, err := state.emitNode(threadID, segment, record, ownership, nativeType, payload.Role, "", payload.CallID, payload.TurnID)
		return err
	}
	if index, known := state.itemNodeByID[payload.ID]; known {
		// An event-sourced item already owns this native identity: the raw
		// response is its correlated mirror, never a duplicate item. A
		// mirror in a later segment is same-thread survival, not a new pair.
		state.responseByItem[payload.ID] = index
		if state.nodes[index].SegmentOrdinal != segment.ordinal {
			state.correlations = append(state.correlations, CodexCapturedCorrelation{
				Kind:   CodexCorrelationSameThreadRollover,
				ItemID: payload.ID,
				Refs:   []schema.SourceEntryRef{state.nodes[index].Ref},
			})
			return nil
		}
		state.correlations = append(state.correlations, CodexCapturedCorrelation{
			Kind:            CodexCorrelationPairedResponseEvent,
			ItemID:          payload.ID,
			EventOrdinal:    codexInt64Ptr(state.nodes[index].Ordinal),
			ResponseOrdinal: codexInt64Ptr(record.Ordinal),
			Refs:            []schema.SourceEntryRef{state.nodes[index].Ref},
		})
		state.boundary.correlated(payload.ID)
		delete(state.eventOrdinal, payload.ID)
		return nil
	}
	if existing, duplicate := state.responseByItem[payload.ID]; duplicate {
		// A repeated response for one native item correlates instead of
		// duplicating. Only an item lifecycle event replaces item state.
		state.correlations = append(state.correlations, CodexCapturedCorrelation{
			Kind:            CodexCorrelationRepeatedItemCompleted,
			ItemID:          payload.ID,
			EventOrdinal:    codexInt64Ptr(state.nodes[existing].Ordinal),
			ResponseOrdinal: codexInt64Ptr(record.Ordinal),
			Refs:            []schema.SourceEntryRef{state.nodes[existing].Ref},
		})
		return nil
	}
	index, _, err := state.emitNode(threadID, segment, record, ownership, nativeType, payload.Role, payload.ID, payload.CallID, payload.TurnID)
	if err != nil {
		return err
	}
	state.responseByItem[payload.ID] = index
	state.itemNodeByID[payload.ID] = index
	if eventOrdinal, paired := state.eventOrdinal[payload.ID]; paired {
		state.correlations = append(state.correlations, CodexCapturedCorrelation{
			Kind:            CodexCorrelationPairedResponseEvent,
			ItemID:          payload.ID,
			EventOrdinal:    codexInt64Ptr(eventOrdinal),
			ResponseOrdinal: codexInt64Ptr(record.Ordinal),
			Refs:            []schema.SourceEntryRef{state.nodes[index].Ref},
		})
		state.boundary.correlated(payload.ID)
		delete(state.eventOrdinal, payload.ID)
	}
	return nil
}

// replayEventMessage handles the event_msg variants that participate in the
// native replay: canonical item lifecycle, turn lifecycle, pairing, turn
// boundaries and legacy instruction rollback.
func (state *codexReplayState) replayEventMessage(threadID string, segment codexDecodedSegment, record codexHistoryRecord, payload codexHistoryReplayPayload, ownership CodexOwnership, mode CodexHistoryMode) error {
	switch payload.Type {
	case "item_started":
		if payload.ID != "" {
			state.boundary.pendUnopened(payload.ID)
		}
		return nil
	case "item_completed":
		return state.replayItemCompleted(threadID, segment, record, payload, ownership, mode)
	case "turn_started", "task_started":
		if payload.TurnID != "" {
			state.openTurns[payload.TurnID] = record.Ordinal
		}
		return nil
	case "turn_complete", "task_complete":
		if payload.TurnID == "" {
			return nil
		}
		if _, open := state.openTurns[payload.TurnID]; open {
			delete(state.openTurns, payload.TurnID)
			return nil
		}
		state.diagnostics = append(state.diagnostics, DiagnosticEntry{
			ErrorType:   "codex_lifecycle_unbalanced",
			Location:    codexRecordLocation(threadID, record),
			Message:     "a native turn lifecycle event completes a turn that never started; the event was retained at its native position",
			Remediation: "Investigate the native writer; the active history is unchanged.",
		})
		return nil
	case "thread_rolled_back":
		if mode != CodexHistoryModeLegacy {
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_paginated_rollback_anomaly",
				Location:    codexRecordLocation(threadID, record),
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
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_rollback_beyond_history",
				Location:    codexRecordLocation(threadID, record),
				Message:     fmt.Sprintf("a native rollback removes %d turns but only %d native instruction turns are open; every open turn was excluded and later appends survive", turns, len(state.turns)),
				Remediation: "Investigate the native writer; the active history holds only the surviving prefix.",
			})
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
		if payload.TurnID != "" {
			delete(state.openTurns, payload.TurnID)
		}
		state.diagnostics = append(state.diagnostics, DiagnosticEntry{
			ErrorType:   "codex_turn_aborted",
			Location:    codexRecordLocation(threadID, record),
			Message:     "a native turn was aborted; its records are retained at their native position",
			Remediation: "No action required; the abort is a native control event.",
		})
		return nil
	default:
		return nil
	}
}

// replayItemCompleted applies the canonical paginated item authority. An
// event that carries a canonical item body emits or replaces the one native
// item, keyed by the item id inside the body (the canonical event has no
// top-level item id); a bare completion marker correlates with its response
// mirror or pends for it. Repeated completions replace completed-item state
// while preserving the native identity (ref); they never duplicate the item.
func (state *codexReplayState) replayItemCompleted(threadID string, segment codexDecodedSegment, record codexHistoryRecord, payload codexHistoryReplayPayload, ownership CodexOwnership, mode CodexHistoryMode) error {
	body, hasBody, bodyMalformed := codexDecodeItemBody(payload.Item)
	if bodyMalformed {
		state.diagnostics = append(state.diagnostics, DiagnosticEntry{
			ErrorType:   "codex_item_body_malformed",
			Location:    codexRecordLocation(threadID, record),
			Message:     "a carried canonical item could not be decoded; the completion marker still correlates but no item state was replaced",
			Remediation: "Repair the native item body and rerun; the existing item state is retained.",
		})
	}
	itemID := payload.ID
	if itemID == "" {
		itemID = payload.ItemID
	}
	if body.ID != "" {
		itemID = body.ID
	}
	if hasBody && !bodyMalformed {
		nativeType, role, recognized := codexItemNativeType(body)
		if recognized && itemID != "" {
			return state.applyItemBody(threadID, segment, record, payload, body, nativeType, role, ownership, mode)
		}
		if !recognized && codexItemBodyCarriesPayload(body) {
			state.diagnostics = append(state.diagnostics, DiagnosticEntry{
				ErrorType:   "codex_item_variant_unknown",
				Location:    codexRecordLocation(threadID, record),
				Message:     "a carried canonical item has an unrecognized discriminator; the completion marker still correlates but no native item state was projected",
				Remediation: "Upgrade Peasant to a build that recognizes this Codex item variant; the raw body is retained in the capture.",
			})
		}
	}
	if itemID == "" {
		return nil
	}
	state.eventOrdinal[itemID] = record.Ordinal
	if index, paired := state.itemNodeByID[itemID]; paired {
		state.correlations = append(state.correlations, CodexCapturedCorrelation{
			Kind:            CodexCorrelationRepeatedItemCompleted,
			ItemID:          itemID,
			EventOrdinal:    codexInt64Ptr(record.Ordinal),
			ResponseOrdinal: codexInt64Ptr(state.nodes[index].Ordinal),
			Refs:            []schema.SourceEntryRef{state.nodes[index].Ref},
		})
		return nil
	}
	if payload.Delivery.isCorrelated() {
		// An inter-agent delivery without its own decodable item body is still
		// a native admission: it opens a legacy instruction boundary once.
		if mode == CodexHistoryModeLegacy && ownership == CodexOwnershipOwn && state.boundary.admit(itemID) {
			state.turns = append(state.turns, nil)
		}
		return nil
	}
	state.boundary.pendUnopened(itemID)
	return nil
}

// applyItemBody emits or replaces the one native item for an item lifecycle
// event that carries its own canonical body. The event is the transcript item
// authority: its body wins over a raw response mirror, and a repeated
// completion (same or later segment) replaces the item state while preserving
// the native identity (ref and key). Only a native instruction admission opens
// a legacy instruction turn; assistant, reasoning, tool and other output stays
// inside its admission's turn.
func (state *codexReplayState) applyItemBody(threadID string, segment codexDecodedSegment, record codexHistoryRecord, event codexHistoryReplayPayload, body codexItemBody, nativeType, role string, ownership CodexOwnership, mode CodexHistoryMode) error {
	itemID := event.ID
	if itemID == "" {
		itemID = event.ItemID
	}
	if body.ID != "" {
		itemID = body.ID
	}
	callID := body.CallID
	turnID := body.TurnID
	if turnID == "" {
		turnID = event.TurnID
	}
	if index, known := state.itemNodeByID[itemID]; known {
		node := &state.nodes[index]
		crossSegment := node.SegmentOrdinal != segment.ordinal
		// A repeated completion replaces the completed-item state from the
		// latest native record while preserving the native identity. A
		// cross-segment repeat is additionally the same-thread rollover
		// evidence; the later body is the surviving native item, never an
		// assumed-unchanged duplicate.
		node.SegmentOrdinal = segment.ordinal
		node.Ordinal = record.Ordinal
		node.LineIndex = record.LineIndex
		node.ByteStart = record.ByteStart
		node.ByteEndExclusive = record.ByteEndExclusive
		node.EnvelopeType = record.EnvelopeType
		node.NativeType = nativeType
		node.NativeRole = role
		node.CallID = callID
		node.TurnID = turnID
		node.Payload = record.Payload
		if bodyChangedMetadata(record.Metadata) {
			node.Metadata = record.Metadata
		}
		kind := CodexCorrelationRepeatedItemCompleted
		if crossSegment {
			kind = CodexCorrelationSameThreadRollover
		}
		state.correlations = append(state.correlations, CodexCapturedCorrelation{
			Kind:            kind,
			ItemID:          itemID,
			EventOrdinal:    codexInt64Ptr(record.Ordinal),
			ResponseOrdinal: codexInt64Ptr(node.Ordinal),
			Refs:            []schema.SourceEntryRef{node.Ref},
		})
		state.boundary.correlated(itemID)
		return nil
	}
	// A native admission opens before emission so the item lands in its own
	// native instruction turn instead of the previous one. Output items do
	// not open a turn on their own.
	if mode == CodexHistoryModeLegacy && ownership == CodexOwnershipOwn && codexItemIsAdmission(event, body, nativeType) && state.boundary.admit(itemID) {
		state.turns = append(state.turns, nil)
	}
	index, _, err := state.emitNode(threadID, segment, record, ownership, nativeType, role, itemID, callID, turnID)
	if err != nil {
		return err
	}
	state.itemNodeByID[itemID] = index
	if eventOrdinal, paired := state.eventOrdinal[itemID]; paired {
		state.correlations = append(state.correlations, CodexCapturedCorrelation{
			Kind:            CodexCorrelationPairedResponseEvent,
			ItemID:          itemID,
			EventOrdinal:    codexInt64Ptr(eventOrdinal),
			ResponseOrdinal: codexInt64Ptr(record.Ordinal),
			Refs:            []schema.SourceEntryRef{state.nodes[index].Ref},
		})
		state.boundary.correlated(itemID)
		delete(state.eventOrdinal, itemID)
	}
	return nil
}

// codexDecodeItemBody decodes a carried canonical item. A missing body is
// not present; a malformed present body is present and malformed, so the
// caller can record the anomaly while still honoring the completion marker.
func codexDecodeItemBody(raw json.RawMessage) (codexItemBody, bool, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return codexItemBody{}, false, false
	}
	var body codexItemBody
	if err := json.Unmarshal(trimmed, &body); err != nil {
		return codexItemBody{}, true, true
	}
	if body.Type == "" {
		return codexItemBody{}, true, true
	}
	return body, true, false
}

func bodyChangedMetadata(metadata json.RawMessage) bool {
	return len(bytes.TrimSpace(metadata)) > 0 && !bytes.Equal(bytes.TrimSpace(metadata), []byte("null"))
}

// replayCompaction retains archival turns and selects a surviving checkpoint.
// A replacement history is a model baseline, never appended duplicate chat.
// History without metadata is accepted; orphan or malformed present metadata,
// and unequal present arrays, mark reconstruction incomplete.
func (state *codexReplayState) replayCompaction(threadID string, segment codexDecodedSegment, record codexHistoryRecord, payload codexHistoryReplayPayload, ownership CodexOwnership) error {
	if !codexReplacementHistoryAligned(payload) {
		state.incomplete = true
		state.diagnostics = append(state.diagnostics, DiagnosticEntry{
			ErrorType:   "codex_checkpoint_metadata_misaligned",
			Location:    codexRecordLocation(threadID, record),
			Message:     "the replacement-history and its metadata arrays cannot be paired; no positional pairing was invented and the reconstruction is incomplete",
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
// supported, including history without metadata. Two present arrays must have
// equal lengths; orphan metadata and malformed present arrays are misaligned.
func codexReplacementHistoryAligned(payload codexHistoryReplayPayload) bool {
	historyLen, historyPresent, historyMalformed := codexArrayState(payload.ReplacementHistory)
	metadataLen, metadataPresent, metadataMalformed := codexArrayState(payload.ReplacementMetadata)
	if historyMalformed || metadataMalformed {
		return false
	}
	if !historyPresent && !metadataPresent {
		return true
	}
	if historyPresent && !metadataPresent {
		return true
	}
	if !historyPresent && metadataPresent {
		return false
	}
	return historyLen == metadataLen
}

// codexArrayState describes a present JSON array. Absent and null values are
// not present; a present non-array value is malformed.
func codexArrayState(raw json.RawMessage) (length int, present bool, malformed bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false, false
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return 0, true, true
	}
	return len(values), true, false
}

// codexLegacyBoundaryReducer is the pinned frozen native
// instruction-boundary predicate for legacy rollback. It keys on native
// item identity and delivery correlation, never on presentation role or
// projected input counts. admit opens at most one turn per native
// admission: the first of a response/event pair opens, the second
// correlates.
type codexLegacyBoundaryReducer struct {
	// pending maps an instruction identity to whether its turn is already
	// open. False means an event pended for a response that has not
	// arrived; true means the turn opened and the pair must correlate.
	pending map[string]bool
}

// admit records one native instruction admission. It reports whether the
// caller must open a new turn.
func (r *codexLegacyBoundaryReducer) admit(id string) bool {
	if id == "" {
		return true
	}
	if opened, known := r.pending[id]; known {
		delete(r.pending, id)
		return !opened
	}
	r.pending[id] = true
	return true
}

// admitResponse applies the reducer to a raw response item: a user
// instruction or a delivery-correlated message is an admission; any other
// response kind is not a turn boundary on its own.
func (r *codexLegacyBoundaryReducer) admitResponse(payload codexHistoryReplayPayload) bool {
	if payload.Type != codexResponseMessage {
		return false
	}
	if payload.Role == "user" || payload.Delivery.isCorrelated() {
		return r.admit(payload.ID)
	}
	return false
}

// pendUnopened records an event awaiting its response mirror without
// opening a turn.
func (r *codexLegacyBoundaryReducer) pendUnopened(id string) {
	if id == "" {
		return
	}
	if _, known := r.pending[id]; !known {
		r.pending[id] = false
	}
}

// correlated drops the pending admission once its pair proved.
func (r *codexLegacyBoundaryReducer) correlated(id string) {
	if id == "" {
		return
	}
	delete(r.pending, id)
}

// emitNode allocates the stable ref for a native key and appends the node,
// capturing the raw payload plus the adjacent envelope metadata. A key seen
// before is one logical item: the node is not duplicated, and the reuse is
// recorded as the same-thread rollover correlation. The returned index is
// the node's position in the captured graph.
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
		LineIndex:        record.LineIndex,
		ByteStart:        record.ByteStart,
		ByteEndExclusive: record.ByteEndExclusive,
		EnvelopeType:     record.EnvelopeType,
		NativeType:       nativeType,
		NativeRole:       role,
		ItemID:           itemID,
		CallID:           callID,
		TurnID:           turnID,
		Ownership:        ownership,
		Payload:          record.Payload,
		Metadata:         record.Metadata,
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
	case codexResponseAgentMessage:
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

// codexNativeKey builds the bounded local identity the ref registry keys on. A
// surviving native item id is scoped to the stable logical thread, never to the
// physical rollout path, so a same-thread rollover reuses the same ref. An item
// lifecycle event carries its native identity inside the nested item body, so
// that id is read as well as the legacy top-level id.
func codexNativeKey(threadID string, segment codexDecodedSegment, record codexHistoryRecord, nativeType string) string {
	if record.Payload != nil {
		var payload struct {
			ID     string `json:"id"`
			ItemID string `json:"item_id"`
			Item   struct {
				ID string `json:"id"`
			} `json:"item"`
		}
		if json.Unmarshal(record.Payload, &payload) == nil {
			itemID := payload.ID
			if itemID == "" {
				itemID = payload.ItemID
			}
			if itemID == "" {
				itemID = payload.Item.ID
			}
			if itemID != "" {
				return fmt.Sprintf("codex|%s|item|%s|%s", threadID, itemID, nativeType)
			}
		}
	}
	return fmt.Sprintf("codex|%s|pos|%s|%d|%s", threadID, segment.descriptor.PhysicalSourceID, record.ByteStart, nativeType)
}

func codexInt64Ptr(value int64) *int64 { return &value }
