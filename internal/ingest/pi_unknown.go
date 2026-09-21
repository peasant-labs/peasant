package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/schema"
)

// piDiscriminator classifies an open native vocabulary before a closed known
// variant decoder sees it. Missing, null and non-string tags remain corruption.
func piDiscriminator(raw json.RawMessage, key string, known json.Unmarshaler) (string, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", false, err
	}
	var kind string
	if err := json.Unmarshal(fields[key], &kind); err != nil {
		return "", false, err
	}
	if strings.TrimSpace(kind) == "" {
		return "", false, fmt.Errorf("native %s must be a nonempty string", key)
	}
	return kind, known.UnmarshalJSON(fields[key]) == nil, nil
}

func decodePiEntry(raw json.RawMessage, line int) (piEntry, error) {
	var entry piEntry
	kind, known, err := piDiscriminator(raw, "type", &entry.Type)
	if err != nil {
		return entry, err
	}
	if known {
		err = json.Unmarshal(raw, &entry)
	} else {
		// Unknown variants share only the tree envelope. Variant-owned fields
		// must not be forced into another variant's shapes.
		var envelope struct {
			ID        string          `json:"id"`
			ParentID  *string         `json:"parentId"`
			Timestamp json.RawMessage `json:"timestamp"`
		}
		err = json.Unmarshal(raw, &envelope)
		entry.ID, entry.ParentID, entry.Timestamp = envelope.ID, envelope.ParentID, envelope.Timestamp
		entry.unknownKind = kind
	}
	entry.raw, entry.line = append(json.RawMessage(nil), raw...), line
	return entry, err
}

// Native usage is an open object, unlike our persisted PiExtra. Validate known
// fields without allowing a newly added, unconsumed field to reject a recording.
func piKnownUsageFields(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("native usage must be an object, not null")
	}
	for key := range fields {
		switch key {
		case "input", "output", "cacheRead", "cacheWrite", "cacheWrite1h", "reasoning", "totalTokens":
		case "cost":
			var costs map[string]json.RawMessage
			if err := json.Unmarshal(fields[key], &costs); err != nil {
				return nil, err
			}
			if costs == nil {
				return nil, fmt.Errorf("native cost must be an object, not null")
			}
			for name := range costs {
				switch name {
				case "input", "output", "cacheRead", "cacheWrite", "total":
				default:
					delete(costs, name)
				}
			}
			encoded, err := json.Marshal(costs)
			if err != nil {
				return nil, err
			}
			fields[key] = encoded
		default:
			delete(fields, key)
		}
	}
	return json.Marshal(fields)
}

// preparePiUnknown preserves evidence only for the selected active entry. It
// removes unknown blocks from interpretation, never from the retained payload,
// and leaves every known sibling for the normal validating decoder.
func preparePiUnknown(entry piEntry, sessionID SessionID) (piEntry, []RetainedUnknown, bool, error) {
	var records []RetainedUnknown
	retain := func(namespace, kind, pointer string, raw json.RawMessage) error {
		position, ok := entry.positions[pointer]
		if !ok {
			return fmt.Errorf("Pi retention has no captured traversal position for source line %d; no evidence was stored; repair the source traversal before retrying", entry.line)
		}
		record, err := NewRetainedUnknownFromSource(HarnessPi, namespace, kind, UnknownSourcePosition{
			SourceEntryRef: schema.SourceEntryRef(PiPublicRef(sessionID.String(), "entry", entry.ID)),
			SourceID:       entry.ID, Line: entry.line, Sequence: entry.sequence, JSONPointer: pointer,
			Public: &UnknownPublicPosition{SourceRef: PiPublicRef(sessionID.String(), "stream", "recording"), RecordIndex: int64(entry.sequence - 1), Position: position},
		}, raw)
		if err == nil {
			records = append(records, record)
		}
		return err
	}
	if entry.unknownKind != "" {
		err := retain("entry", entry.unknownKind, "", entry.raw)
		return entry, records, true, err
	}
	filter := func(raw json.RawMessage, pointer string) (json.RawMessage, error) {
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			return raw, nil // normal content decoder validates text and bad shapes
		}
		var blocks []json.RawMessage
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, err
		}
		kept := make([]json.RawMessage, 0, len(blocks))
		for index, block := range blocks {
			var tag piBlockType
			kind, known, err := piDiscriminator(block, "type", &tag)
			if err != nil {
				return nil, err
			}
			if known {
				kept = append(kept, block)
			} else if err := retain("content_block", kind, fmt.Sprintf("%s/%d", pointer, index), block); err != nil {
				return nil, err
			}
		}
		return json.Marshal(kept)
	}
	var err error
	switch entry.Type {
	case piMessage:
		var role piMessageRole
		kind, known, err := piDiscriminator(entry.Message, "role", &role)
		if err != nil {
			return entry, nil, false, err
		}
		if !known {
			err := retain("message_role", kind, "/message", entry.Message)
			return entry, records, true, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry.Message, &fields); err != nil {
			return entry, nil, false, err
		}
		if raw, present := fields["content"]; present {
			fields["content"], err = filter(raw, "/message/content")
			if err != nil {
				return entry, nil, false, err
			}
		}
		entry.Message, err = json.Marshal(fields)
	case piCustomMessage:
		entry.Content, err = filter(entry.Content, "/content")
	}
	return entry, records, false, err
}

// assignPiPositions walks each source record before active-path selection or
// content folding. Records, message envelopes and content blocks each occupy
// one traversal slot, including known nodes and nodes on inactive branches.
// Blank physical lines are not records. Opaque variant payloads are not guessed
// to contain additional native nodes. Semantic validation stays in project.
func (entry *piEntry) assignPiPositions(next *int64) {
	entry.positions = make(map[string]int64)
	take := func(pointer string) {
		entry.positions[pointer] = *next
		*next++
	}
	take("")
	var content json.RawMessage
	var pointer string
	switch entry.Type {
	case piMessage:
		take("/message")
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry.Message, &fields) != nil {
			return
		}
		content, pointer = fields["content"], "/message/content"
	case piCustomMessage:
		content, pointer = entry.Content, "/content"
	default:
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for index := range blocks {
		take(fmt.Sprintf("%s/%d", pointer, index))
	}
}
