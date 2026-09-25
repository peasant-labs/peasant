package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

const codexOpaqueBlock = "__peasant_retained_unknown__"

func codexNativeEventMsgKinds() []string {
	return []string{
		"token_count",
		"user_message",
		"agent_message",
		"agent_reasoning",
		"item_started",
		"item_completed",
		"turn_started",
		"task_started",
		"turn_complete",
		"task_complete",
		"thread_rolled_back",
		"turn_aborted",
	}
}

// A compatibility preview may tolerate an unreadable source shape, but it must
// never hide a failure to retain otherwise valid opaque evidence.
type codexEvidenceRetentionError struct{ cause error }

func (err *codexEvidenceRetentionError) Error() string { return err.cause.Error() }
func (err *codexEvidenceRetentionError) Unwrap() error { return err.cause }

// prepareCodexRecord separates opaque variants before decoding known unions.
// Unknown blocks are replaced only in the interpretation copy; their complete
// redacted source values retain their original JSON pointers and physical line.
func prepareCodexRecord(raw []byte, position UnknownSourcePosition, native bool) ([]byte, []RetainedUnknown, error) {
	var envelope map[string]json.RawMessage
	if err := requireJSONObject(raw); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, nil, err
	}
	var kind string
	if err := json.Unmarshal(envelope["type"], &kind); err != nil || kind == "" {
		return nil, nil, fmt.Errorf("Codex record requires a string type")
	}
	var unknown []RetainedUnknown
	traversal := codexTraversalPointers(raw)
	retain := func(namespace, kind, pointer string, value json.RawMessage) error {
		at := position
		at.JSONPointer = pointer
		if position.Public != nil {
			public := *position.Public
			if index, ok := traversal[pointer]; ok {
				public.Position += index
			} else {
				return &codexEvidenceRetentionError{cause: fmt.Errorf("retain Codex evidence: source pointer is outside the captured traversal; no evidence was stored; repair the capture traversal")}
			}
			at.Public = &public
		}
		evidence, err := NewRetainedUnknown(HarnessCodex, namespace, kind, at, value)
		if err != nil {
			return &codexEvidenceRetentionError{cause: err}
		}
		unknown = append(unknown, evidence)
		return nil
	}
	if !slices.Contains(codexStrictEnvelopeKinds(), kind) && !(native && slices.Contains(codexNativeEnvelopeKinds(), kind)) {
		err := retain("envelope", kind, "", raw)
		return nil, unknown, err
	}
	if err := requireJSONObject(envelope["payload"]); err != nil {
		return nil, nil, err
	}
	if kind != codexTypeResponse && kind != codexTypeEventMsg {
		return raw, nil, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(envelope["payload"], &payload); err != nil {
		return nil, nil, err
	}
	var variant string
	if err := json.Unmarshal(payload["type"], &variant); err != nil || variant == "" {
		return nil, nil, fmt.Errorf("Codex payload requires a string type")
	}
	known := slices.Contains(codexStrictResponsePayloadKinds(), variant)
	if kind == codexTypeEventMsg {
		known = slices.Contains(codexStrictEventMsgKinds(), variant)
	}
	if native {
		if kind == codexTypeResponse {
			_, known = codexResponseNativeType(variant)
		} else {
			switch variant {
			case "item_started", "item_completed", "turn_started", "turn_complete", "thread_rolled_back":
				known = true
			}
		}
	}
	if !known {
		namespace := kind
		if kind == codexTypeEventMsg {
			namespace = "event"
		}
		err := retain(namespace, variant, "/payload", envelope["payload"])
		return nil, unknown, err
	}
	if native && kind == codexTypeEventMsg && variant == "item_completed" && len(payload["item"]) > 0 && string(payload["item"]) != "null" {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload["item"], &header); err != nil || header.Type == "" {
			// The history reader diagnoses malformed carried items; the candidate
			// boundary refuses that diagnostic without replacing prior state.
			return raw, nil, nil
		}
		if _, _, ok := codexItemNativeType(codexItemBody{Type: header.Type}); !ok {
			if err := retain("item_body", header.Type, "/payload/item", payload["item"]); err != nil {
				return nil, nil, err
			}
			delete(payload, "item")
		} else if nativeType, role, _ := codexItemNativeType(codexItemBody{Type: header.Type}); nativeType == "message" || nativeType == "reasoning" {
			var item map[string]json.RawMessage
			if err := json.Unmarshal(payload["item"], &item); err != nil {
				return nil, nil, err
			}
			// Parse-once index of the original carried item's content/summary
			// arrays. Record-local lifetime: built once here, used to resolve
			// each retained leaf below by validated field/index, then
			// discarded. Evidence comes from these RawMessage copies, never
			// from marshaled output, so original lexical bytes survive.
			originalBlocks, err := indexCodexOriginalBlocks(payload["item"])
			if err != nil {
				return nil, nil, err
			}
			originalType, originalRole := item["type"], item["role"]
			item["type"], _ = json.Marshal(nativeType)
			if len(originalRole) == 0 && role != "" {
				item["role"], _ = json.Marshal(role)
			}
			itemRaw, err := json.Marshal(item)
			if err != nil {
				return nil, nil, err
			}
			wrapper, err := json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"response_item"`), "payload": itemRaw})
			if err != nil {
				return nil, nil, err
			}
			prepared, nested, err := prepareCodexRecord(wrapper, position, true)
			if err != nil {
				return nil, nil, err
			}
			for _, record := range nested {
				// Normalization above is an interpretation copy. Rebind every
				// retained leaf to the original item's raw value before redaction;
				// marshaling the wrapper compacts RawMessage whitespace. The
				// index above was parsed once; this loop performs no
				// whole-item/array decode, only O(1) slice resolution plus
				// the leaf retain.
				field, index, err := parseCodexOriginalPointer(record.Position.JSONPointer)
				if err != nil {
					return nil, nil, err
				}
				original, err := originalBlocks.at(field, index)
				if err != nil {
					return nil, nil, err
				}
				pointer := "/payload/item" + strings.TrimPrefix(record.Position.JSONPointer, "/payload")
				if err := retain(record.Namespace, record.Kind, pointer, original); err != nil {
					return nil, nil, err
				}
			}
			if len(nested) > 0 {
				var value map[string]json.RawMessage
				if err := json.Unmarshal(prepared, &value); err != nil {
					return nil, nil, err
				}
				if err := json.Unmarshal(value["payload"], &item); err != nil {
					return nil, nil, err
				}
				item["type"] = originalType
				if len(originalRole) == 0 {
					delete(item, "role")
				} else {
					item["role"] = originalRole
				}
				payload["item"], err = json.Marshal(item)
				if err != nil {
					return nil, nil, err
				}
			}
		}
	}
	if kind == codexTypeResponse && (variant == codexResponseMessage || variant == codexResponseReasoning) {
		if variant == codexResponseMessage {
			var role string
			if err := json.Unmarshal(payload["role"], &role); err != nil || role == "" {
				return nil, nil, fmt.Errorf("Codex message requires a string role")
			}
			if role != "developer" {
				if err := validateCaptureRole(role); err != nil {
					return nil, nil, err
				}
			}
			if len(payload["content"]) == 0 || string(payload["content"]) == "null" {
				return nil, nil, fmt.Errorf("Codex message requires content")
			}
		}
		for _, field := range []string{"content", "summary"} {
			value, exists := payload[field]
			if !exists {
				continue
			}
			var blocks []json.RawMessage
			if err := json.Unmarshal(value, &blocks); err != nil {
				return nil, nil, err
			}
			for i, block := range blocks {
				var header struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(block, &header); err != nil || header.Type == "" {
					return nil, nil, fmt.Errorf("Codex %s block requires a string type", field)
				}
				kinds := codexStrictMessageBlockKinds()
				if variant == codexResponseReasoning {
					kinds = codexStrictReasoningContentKinds()
					if field == "summary" {
						kinds = codexStrictReasoningSummaryKinds()
					}
				}
				supported := slices.Contains(kinds, header.Type)
				if native && variant == codexResponseMessage && codexMediaContentTypes()[header.Type] {
					supported = true
				}
				if supported {
					if slices.Contains(kinds, header.Type) {
						var text struct {
							Text *string `json:"text"`
						}
						if err := json.Unmarshal(block, &text); err != nil || text.Text == nil {
							return nil, nil, fmt.Errorf("Codex known text block requires string text")
						}
					}
					continue
				}
				namespace := "message_block"
				if variant == codexResponseReasoning {
					namespace = "reasoning_" + field
				}
				if err := retain(namespace, header.Type, fmt.Sprintf("/payload/%s/%d", field, i), block); err != nil {
					return nil, nil, err
				}
				// Preserve vector alignment for native provenance. The empty placeholder
				// contains no source text and cannot be mistaken for interpreted content.
				replacement := kinds[0]
				if native {
					replacement = codexOpaqueBlock
				}
				blocks[i] = json.RawMessage(fmt.Sprintf(`{"type":%q,"text":""}`, replacement))
			}
			encoded, err := json.Marshal(blocks)
			if err != nil {
				return nil, nil, err
			}
			payload[field] = encoded
		}
	}
	if len(unknown) == 0 {
		return raw, nil, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	envelope["payload"] = encoded
	encoded, err = json.Marshal(envelope)
	return encoded, unknown, err
}

// codexOriginalBlocks is the parse-once index of a canonical carried item's
// content/summary arrays. It is built once per carried item before
// normalization, resolves each retained leaf by validated field/index, and is
// discarded with the record. Slices hold RawMessage copies from that single
// parse; evidence never derives from marshaled output, so original lexical
// bytes survive. It uses the shared raw-safety helpers (requireJSONObject,
// validUnknownPointer shape checks live in the codec); it defines no new
// envelope validation.
type codexOriginalBlocks struct {
	content []json.RawMessage
	summary []json.RawMessage
}

// errCodexOriginalBlock is the single shared failure for an unaddressable
// original carried-item block. All parse-once helpers delegate so the safe
// message (what/effect/repair, no source bytes) cannot drift.
func errCodexOriginalBlock() error {
	return &codexEvidenceRetentionError{cause: fmt.Errorf("retain Codex canonical item: the original source block cannot be addressed; no evidence was certified; recapture intact source or repair the source-pointer mapping")}
}

// indexCodexOriginalBlocks parses the original carried item and its
// content/summary arrays once. It performs O(item bytes) work a single time;
// per-leaf resolution via at is O(1) plus the leaf bytes.
func indexCodexOriginalBlocks(item json.RawMessage) (codexOriginalBlocks, error) {
	fail := func() (codexOriginalBlocks, error) {
		return codexOriginalBlocks{}, errCodexOriginalBlock()
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(item, &obj); err != nil {
		return fail()
	}
	var out codexOriginalBlocks
	if raw, ok := obj["content"]; ok {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && string(trimmed) != "null" {
			var blocks []json.RawMessage
			if err := json.Unmarshal(raw, &blocks); err != nil {
				return fail()
			}
			out.content = blocks
		}
	}
	if raw, ok := obj["summary"]; ok {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && string(trimmed) != "null" {
			var blocks []json.RawMessage
			if err := json.Unmarshal(raw, &blocks); err != nil {
				return fail()
			}
			out.summary = blocks
		}
	}
	return out, nil
}

// at resolves one retained leaf by already-validated field/index without
// reparsing the carried item. It never marshals; the returned RawMessage is
// the original source block bytes from the index parse.
func (b codexOriginalBlocks) at(field string, index int) (json.RawMessage, error) {
	fail := func() (json.RawMessage, error) {
		return nil, errCodexOriginalBlock()
	}
	var blocks []json.RawMessage
	switch field {
	case "content":
		blocks = b.content
	case "summary":
		blocks = b.summary
	default:
		return fail()
	}
	if index < 0 || index >= len(blocks) {
		return fail()
	}
	return blocks[index], nil
}

// parseCodexOriginalPointer validates a nested "/payload/<field>/<index>"
// pointer from the normalized wrapper and returns the field/index for index
// resolution. The caller rebinds the pointer to "/payload/item/..." and keeps
// the outer traversal coordinates via retain.
func parseCodexOriginalPointer(pointer string) (string, int, error) {
	fail := func() (string, int, error) {
		return "", 0, errCodexOriginalBlock()
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/payload/"), "/")
	if len(parts) != 2 || (parts[0] != "content" && parts[0] != "summary") {
		return fail()
	}
	index, err := strconv.Atoi(parts[1])
	if err != nil || index < 0 {
		return fail()
	}
	return parts[0], index, nil
}

// codexTraversalPointers assigns preorder coordinates to the source envelope,
// discriminated payload, canonical item, and every content/summary block. It
// visits known and unknown values alike, before interpretation or folding.
func codexTraversalPointers(raw []byte) map[string]int64 {
	positions := map[string]int64{"": 0}
	var envelope struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return positions
	}
	var visit func(json.RawMessage, string)
	visit = func(value json.RawMessage, pointer string) {
		positions[pointer] = int64(len(positions))
		var object map[string]json.RawMessage
		if json.Unmarshal(value, &object) != nil {
			return
		}
		for _, field := range []string{"content", "summary"} {
			var blocks []json.RawMessage
			if json.Unmarshal(object[field], &blocks) != nil {
				continue
			}
			for i, block := range blocks {
				visit(block, fmt.Sprintf("%s/%s/%d", pointer, field, i))
			}
		}
		if item := object["item"]; len(item) > 0 && string(item) != "null" {
			visit(item, pointer+"/item")
		}
	}
	if envelope.Type == codexTypeResponse || envelope.Type == codexTypeEventMsg {
		visit(envelope.Payload, "/payload")
	}
	return positions
}

func codexPublicPosition(stream string, line int, position int64) *UnknownPublicPosition {
	digest := sha256.Sum256([]byte("codex\x00" + stream))
	return &UnknownPublicPosition{SourceRef: "src_" + hex.EncodeToString(digest[:]), RecordIndex: int64(line - 1), Position: position}
}

func codexNativeUnknownPosition(threadID string, segment codexDecodedSegment, record codexHistoryRecord) UnknownSourcePosition {
	stream := segment.descriptor.PhysicalSourceID
	if stream == "" {
		stream = segment.descriptor.Pointer
	}
	line := int(record.LineIndex + 1)
	return UnknownSourcePosition{SourceID: segment.descriptor.PhysicalSourceID, Line: line, Public: codexPublicPosition(threadID+"\x00"+stream, line, record.TraversalPosition)}
}

// Unknown envelopes have physical coordinates only. Membership is determined
// by the captured byte interval, never by assigning the physical line an ordinal.
func codexUnknownInBounds(segment codexDecodedSegment, record codexHistoryRecord) bool {
	if segment.isCurrent {
		return true
	}
	if segment.descriptor.Coordinates.Start == nil && segment.descriptor.Coordinates.EndExclusive == nil {
		return true
	}
	var first, last int64 = -1, -1
	for _, known := range segment.records {
		if !known.HasOrdinal || !codexRecordInBounds(segment, known, nil) {
			continue
		}
		if first < 0 {
			first = known.ByteStart
		}
		last = known.ByteEndExclusive
	}
	return first >= 0 && record.ByteStart >= first && record.ByteEndExclusive <= last
}

func codexUnknownOwnership(segment codexDecodedSegment, record codexHistoryRecord) CodexOwnership {
	if segment.descriptor.CopyBoundary == nil {
		return codexSegmentOwnership(segment, 0)
	}
	// The boundary is an ordinal, so locate its physical record before comparing
	// byte offsets. Unknown records themselves never receive an ordinal.
	for _, known := range segment.records {
		if known.HasOrdinal && known.Ordinal >= *segment.descriptor.CopyBoundary {
			if record.ByteStart < known.ByteStart {
				if segment.descriptor.OriginalOwnershipProven {
					return CodexOwnershipInherited
				}
				return CodexOwnershipUncertainEarlier
			}
			return codexSegmentOwnership(segment, known.Ordinal)
		}
	}
	if segment.descriptor.OriginalOwnershipProven {
		return CodexOwnershipInherited
	}
	return CodexOwnershipUncertainEarlier
}

func (state *codexReplayState) emitUnknown(threadID string, segment codexDecodedSegment, record codexHistoryRecord, ownership CodexOwnership, evidence []RetainedUnknown) error {
	if len(evidence) == 0 {
		return nil
	}
	key := fmt.Sprintf("codex|%s|opaque|%s|%d", threadID, segment.descriptor.PhysicalSourceID, record.ByteStart)
	if state.seenKeys[key] {
		return nil
	}
	ref, _, err := state.registry.RefFor(key)
	if err != nil {
		return err
	}
	state.seenKeys[key] = true
	ordinal := int64(-1)
	if record.HasOrdinal {
		ordinal = record.Ordinal
	}
	state.nodes = append(state.nodes, CodexCapturedNode{NativeKey: key, Ref: ref, SegmentOrdinal: segment.ordinal, Ordinal: ordinal, LineIndex: record.LineIndex, ByteStart: record.ByteStart, ByteEndExclusive: record.ByteEndExclusive, EnvelopeType: record.EnvelopeType, Ownership: ownership, RetainedUnknown: evidence})
	if codexOwnershipIsOwn(ownership) {
		state.turns = appendTurnNode(state.turns, len(state.nodes)-1)
	}
	return nil
}

// Carrier metadata is persisted inside the generation document. Inherited
// carriers also reference the owning source segment; no opaque payload becomes
// conversation text or an unreferenced blob that an exporter could discard.
func attachCodexUnknown(result *indexformat.V2, nodes []CodexCapturedNode) error {
	byAlias := map[string][]RetainedUnknown{}
	segments := map[string]int{}
	for _, node := range nodes {
		if len(node.RetainedUnknown) == 0 {
			continue
		}
		key, err := encodeBlockAliasKey(node.NativeKey + "/retained-unknown")
		if err != nil {
			return err
		}
		byAlias[key] = node.RetainedUnknown
		segments[key] = node.SegmentOrdinal
	}
	for _, alias := range result.Generation.Aliases {
		if evidence := byAlias[alias.NativeKey]; len(evidence) > 0 {
			for i := range result.Generation.Segments {
				segment := &result.Generation.Segments[i]
				if segment.Ordinal == segments[alias.NativeKey] && !slices.Contains(segment.CapturedRefs, alias.Ref) {
					segment.CapturedRefs = append(segment.CapturedRefs, alias.Ref)
				}
			}
		}
	}
	return result.Generation.Validate()
}
