package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/schema"
)

func openCodeUnknownPosition(sessionID, stream, sourceID string, record, position int64, pointer string) UnknownSourcePosition {
	sum := sha256.Sum256([]byte("opencode\x00" + sessionID + "\x00" + stream))
	return UnknownSourcePosition{SourceID: sourceID, JSONPointer: pointer, Public: &UnknownPublicPosition{SourceRef: "src_" + hex.EncodeToString(sum[:]), RecordIndex: record, Position: position}}
}

func knownOpenCodeCurrentRow(kind string) bool {
	switch kind {
	case "user":
		return true
	case "assistant":
		return true
	case "shell":
		return true
	case "synthetic":
		return true
	case "system":
		return true
	case "skill":
		return true
	case "compaction":
		return true
	case "agent-switched":
		return true
	case "model-switched":
		return true
	default:
		return false
	}
}

// prepareOpenCodeCurrent preserves unrecognized row and nested discriminators
// before native typed normalization can discard their fields. Only the private
// interpretation copy is changed; the fallback payload is redacted in full.
func prepareOpenCodeCurrent(kind string, raw []byte, position UnknownSourcePosition) ([]byte, []RetainedUnknown, error) {
	if kind == "" {
		return nil, nil, fmt.Errorf("OpenCode row requires its native type; restore the row before capture")
	}
	if err := requireJSONObject(raw); err != nil {
		return nil, nil, err
	}
	var identity map[string]json.RawMessage
	if err := json.Unmarshal(raw, &identity); err != nil {
		return nil, nil, err
	}
	if identity["messageID"] != nil && identity["message_id"] != nil && !bytes.Equal(identity["messageID"], identity["message_id"]) {
		return nil, nil, fmt.Errorf("OpenCode row has conflicting messageID and message_id pairing identities")
	}
	retain := func(namespace, kind, pointer string, value json.RawMessage, offset int64) (RetainedUnknown, error) {
		at := position
		at.JSONPointer = pointer
		if position.Public != nil {
			public := *position.Public
			public.Position += offset
			at.Public = &public
		}
		return NewRetainedUnknownFromSource(HarnessOpenCode, namespace, kind, at, value)
	}
	if !knownOpenCodeCurrentRow(kind) {
		evidence, err := retain("row", kind, "", raw, 0)
		return nil, []RetainedUnknown{evidence}, err
	}
	if kind != "assistant" {
		return raw, nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	var content []json.RawMessage
	if err := json.Unmarshal(fields["content"], &content); err != nil {
		return nil, nil, err
	}
	kept := make([]json.RawMessage, 0, len(content))
	var unknown []RetainedUnknown
	var blockPosition int64 = 1
	for i, block := range content {
		at := blockPosition
		blockPosition += openCodeToolTraversalSize(block)
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(block, &header); err != nil || header.Type == "" {
			return nil, nil, fmt.Errorf("OpenCode assistant block requires a string type")
		}
		if header.Type == "tool" {
			position := position
			if position.Public != nil {
				public := *position.Public
				public.Position += at
				position.Public = &public
			}
			prepared, evidence, err := prepareOpenCodeToolContent(block, fmt.Sprintf("/content/%d", i), position)
			if err != nil {
				return nil, nil, err
			}
			unknown = append(unknown, evidence...)
			kept = append(kept, prepared)
			continue
		}
		if header.Type == "text" || header.Type == "reasoning" {
			kept = append(kept, block)
			continue
		}
		evidence, err := retain("assistant_content", header.Type, fmt.Sprintf("/content/%d", i), block, at)
		if err != nil {
			return nil, nil, err
		}
		unknown = append(unknown, evidence)
	}
	if len(unknown) == 0 {
		return raw, nil, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, err
	}
	fields["content"] = encoded
	encoded, err = json.Marshal(fields)
	return encoded, unknown, err
}

func openCodeCurrentTraversalSize(raw []byte) int64 {
	var row struct {
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &row) != nil {
		return 1
	}
	size := int64(1)
	for _, block := range row.Content {
		size += openCodeToolTraversalSize(block)
	}
	return size
}

func openCodeToolTraversalSize(raw []byte) int64 {
	var block struct {
		State struct {
			Content []json.RawMessage `json:"content"`
		} `json:"state"`
	}
	if json.Unmarshal(raw, &block) != nil {
		return 1
	}
	return 1 + int64(len(block.State.Content))
}

// Tool output has its own typed block namespace. Unknown children are retained
// without discarding the known tool call or its understood output siblings.
func prepareOpenCodeToolContent(raw json.RawMessage, pointer string, position UnknownSourcePosition) (json.RawMessage, []RetainedUnknown, error) {
	var tool map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tool); err != nil {
		return nil, nil, err
	}
	var state map[string]json.RawMessage
	if json.Unmarshal(tool["state"], &state) != nil {
		return raw, nil, nil
	}
	var status string
	if json.Unmarshal(state["status"], &status) != nil {
		return raw, nil, nil
	}
	historical := state["structured"] != nil || state["attachments"] != nil || state["outputPaths"] != nil || state["result"] != nil
	if !historical && status != "completed" && status != "error" {
		return raw, nil, nil
	}
	var content []json.RawMessage
	if json.Unmarshal(state["content"], &content) != nil {
		return raw, nil, nil
	}
	kept := make([]json.RawMessage, 0, len(content))
	var unknown []RetainedUnknown
	for i, block := range content {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(block, &header); err != nil || header.Type == "" {
			return nil, nil, fmt.Errorf("OpenCode tool output block requires a string type")
		}
		if header.Type == "text" || header.Type == "file" {
			kept = append(kept, block)
			continue
		}
		at := position
		at.JSONPointer = fmt.Sprintf("%s/state/content/%d", pointer, i)
		if position.Public != nil {
			public := *position.Public
			public.Position += int64(i + 1)
			at.Public = &public
		}
		evidence, err := NewRetainedUnknownFromSource(HarnessOpenCode, "tool_content", header.Type, at, block)
		if err != nil {
			return nil, nil, err
		}
		unknown = append(unknown, evidence)
	}
	if len(unknown) == 0 {
		return raw, nil, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, err
	}
	state["content"] = encoded
	if !historical && len(kept) == 0 {
		if status == "error" {
			delete(state, "content")
		} else {
			// The interpretation has no output text. Use the supported historical
			// empty-output representation; the complete native output is evidence.
			state["structured"] = json.RawMessage(`{}`)
			if metadata := state["metadata"]; metadata != nil {
				state["structured"] = metadata
			}
			state["result"] = json.RawMessage(`""`)
		}
	}
	encoded, err = json.Marshal(state)
	if err != nil {
		return nil, nil, err
	}
	tool["state"] = encoded
	encoded, err = json.Marshal(tool)
	return encoded, unknown, err
}

// retainOpenCodeSemantic uses the actual message and part streams. Parts keep
// their native identity and array order even when no known part is renderable.
func retainOpenCodeSemantic(sessionID SessionID, messages []openCodeSemanticMessage) ([]openCodeSemanticMessage, error) {
	messages = append([]openCodeSemanticMessage(nil), messages...)
	var messagePosition int64
	for m := range messages {
		message := &messages[m]
		message.RetainedUnknown = append([]RetainedUnknown(nil), message.RetainedUnknown...)
		basePosition := messagePosition
		messagePosition++
		kept := make([]openCodeSemanticPart, 0, len(message.Parts))
		var partPosition int64
		for p, part := range message.Parts {
			at := partPosition
			partPosition += openCodeToolTraversalSize(part.Raw)
			id := part.EntryID
			if id == "" {
				id = message.EntryID
			}
			position := openCodeUnknownPosition(sessionID.String(), "parts:"+message.EntryID, id, int64(p), at, "")
			if part.Data.Type == "tool" {
				prepared, evidence, err := prepareOpenCodeToolContent(part.Raw, "", position)
				if err != nil {
					return nil, err
				}
				if len(evidence) > 0 {
					part, err = parseOpenCodeSemanticPart(part.EntryID, part.TimeCreated, prepared)
					if err != nil {
						return nil, err
					}
					message.RetainedUnknown = append(message.RetainedUnknown, evidence...)
				}
			}
			if isKnownOpenCodeSemanticPartType(part.Data.Type) || isOpenCodeCaptureControl(part.Data.Type) {
				kept = append(kept, part)
				continue
			}
			evidence, err := NewRetainedUnknownFromSource(HarnessOpenCode, "part", part.Data.Type, position, part.Raw)
			if err != nil {
				return nil, err
			}
			message.RetainedUnknown = append(message.RetainedUnknown, evidence)
		}
		message.Parts = kept
		raw := message.Data.Content
		if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			continue
		}
		var blocks []json.RawMessage
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, err
		}
		messagePosition += int64(len(blocks))
		known := make([]json.RawMessage, 0, len(blocks))
		for b, block := range blocks {
			var header struct {
				Type string  `json:"type"`
				Text *string `json:"text"`
			}
			var discriminator struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(block, &discriminator); err != nil || discriminator.Type == "" {
				return nil, fmt.Errorf("OpenCode inline block requires a string type")
			}
			if discriminator.Type == "text" {
				if err := json.Unmarshal(block, &header); err != nil || header.Text == nil {
					return nil, fmt.Errorf("OpenCode text block requires string text")
				}
				known = append(known, block)
				continue
			}
			position := openCodeUnknownPosition(sessionID.String(), "messages", message.EntryID, int64(m), basePosition+int64(b+1), fmt.Sprintf("/content/%d", b))
			evidence, err := NewRetainedUnknownFromSource(HarnessOpenCode, "content_block", discriminator.Type, position, block)
			if err != nil {
				return nil, err
			}
			message.RetainedUnknown = append(message.RetainedUnknown, evidence)
		}
		encoded, err := json.Marshal(known)
		if err != nil {
			return nil, err
		}
		message.Data.Content = encoded
	}
	return messages, nil
}

func validateOpenCodeUnknown(records []RetainedUnknown) error {
	for _, record := range records {
		if record.Harness != HarnessOpenCode {
			return fmt.Errorf("managed OpenCode evidence harness differs from owner; regenerate the capture")
		}
		if _, err := NewRetainedUnknown(record.Harness, record.Namespace, record.Kind, record.Position, record.Payload); err != nil {
			return err
		}
		if public := record.Position.Public; public != nil {
			if public.RecordIndex < 0 || public.Position < 0 || !strings.HasPrefix(public.SourceRef, "src_") || len(public.SourceRef) != 68 {
				return fmt.Errorf("managed OpenCode evidence carries invalid source traversal coordinates; regenerate the capture")
			}
			if _, err := hex.DecodeString(strings.TrimPrefix(public.SourceRef, "src_")); err != nil {
				return fmt.Errorf("managed OpenCode evidence source stream identity is not opaque; regenerate the capture")
			}
		}
	}
	return nil
}

func attachOpenCodeUnknownEntry(entry *schema.SessionEntry, evidence []RetainedUnknown) error {
	if len(evidence) == 0 {
		return nil
	}
	return AttachRetainedUnknown(entry, evidence)
}
