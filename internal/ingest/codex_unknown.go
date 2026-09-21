package ingest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

const codexOpaqueBlock = "__peasant_retained_unknown__"

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
	retain := func(namespace, kind, pointer string, value json.RawMessage) error {
		at := position
		at.JSONPointer = pointer
		evidence, err := NewRetainedUnknownFromSource(HarnessCodex, namespace, kind, at, value)
		if err == nil {
			unknown = append(unknown, evidence)
		}
		return err
	}
	if !slices.Contains(codexStrictEnvelopeKinds(), kind) && !(native && kind == "compacted") {
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
		err := retain(kind, variant, "/payload", envelope["payload"])
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
			if err := retain("item", header.Type, "/payload/item", payload["item"]); err != nil {
				return nil, nil, err
			}
			delete(payload, "item")
		} else if nativeType, role, _ := codexItemNativeType(codexItemBody{Type: header.Type}); nativeType == "message" || nativeType == "reasoning" {
			var item map[string]json.RawMessage
			if err := json.Unmarshal(payload["item"], &item); err != nil {
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
			for i := range nested {
				nested[i].Position.JSONPointer = "/payload/item" + strings.TrimPrefix(nested[i].Position.JSONPointer, "/payload")
			}
			unknown = append(unknown, nested...)
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
				if native && codexMediaContentTypes()[header.Type] {
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
				if err := retain("content_block", header.Type, fmt.Sprintf("/payload/%s/%d", field, i), block); err != nil {
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
	byRef := map[schema.SourceEntryRef][]RetainedUnknown{}
	for _, alias := range result.Generation.Aliases {
		if evidence := byAlias[alias.NativeKey]; len(evidence) > 0 {
			byRef[alias.Ref] = evidence
			for i := range result.Generation.Segments {
				segment := &result.Generation.Segments[i]
				if segment.Ordinal == segments[alias.NativeKey] && !slices.Contains(segment.CapturedRefs, alias.Ref) {
					segment.CapturedRefs = append(segment.CapturedRefs, alias.Ref)
				}
			}
		}
	}
	attach := func(entries []schema.SessionEntry) error {
		for i := range entries {
			if err := AttachRetainedUnknown(&entries[i], byRef[entries[i].SourceEntryRef]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := attach(result.Generation.Main.Entries); err != nil {
		return err
	}
	for i := range result.Generation.Earlier {
		if err := attach(result.Generation.Earlier[i].Content.Entries); err != nil {
			return err
		}
	}
	applyStrictCounts(&result.Generation, result.Generation.Completeness)
	return result.Generation.Validate()
}
