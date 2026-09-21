package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

// captureCorruptionError prevents an omission flag from turning a malformed
// known record into a successful tolerant replacement of previous good data.
type captureCorruptionError struct{ err error }

func (e *captureCorruptionError) Error() string { return e.err.Error() }
func (e *captureCorruptionError) Unwrap() error { return e.err }

// prepareUnknownJSONL separates uninterpreted evidence from known content before
// typed decoding. Unknown shapes must not be decoded into a known block struct:
// their fields may legitimately have entirely different types.
func prepareUnknownJSONL(harness Harness, raw []byte, line int) ([]byte, []RetainedUnknown, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, nil, false, fmt.Errorf("source record must be a JSON object")
	}
	var kind string
	if value := fields["type"]; value != nil {
		if err := json.Unmarshal(value, &kind); err != nil {
			return nil, nil, false, err
		}
	}
	unknown := false
	namespace := "record"
	switch harness {
	case HarnessClaudeCode:
		unknown = kind != "" && !slices.Contains(claudeStrictRecordKinds(), kind) && !isClaudeControlRecordType(kind)
		if kind == "progress" || kind == "queue-operation" || kind == "file-history-snapshot" {
			var message map[string]json.RawMessage
			if value := fields["message"]; value != nil {
				if err := json.Unmarshal(value, &message); err != nil {
					return nil, nil, false, err
				}
			}
			unknown = fields["content"] != nil || message["content"] != nil
		}
		if kind == "system" && fields["content"] == nil {
			var message map[string]json.RawMessage
			if value := fields["message"]; value != nil {
				if err := json.Unmarshal(value, &message); err != nil {
					return nil, nil, false, err
				}
			}
			if message["content"] == nil {
				var subtype string
				if err := json.Unmarshal(fields["subtype"], &subtype); err == nil && subtype != "" && !slices.Contains(claudeStrictSystemSubtypes(), subtype) {
					kind, namespace, unknown = subtype, "system_subtype", true
				}
			}
		}
	case HarnessCursor:
		// Role-shaped records are Cursor's ordinary message envelope. Only
		// the production special-record vocabulary owns type-only dispatch.
		unknown = kind != "" && !isCursorSpecialRecordKind(kind) && !slices.Contains(cursorCaptureRoleKinds(), kind)
		if unknown {
			break
		}
		if isCursorSpecialRecordKind(kind) {
			var status string
			if value := fields["status"]; value != nil {
				if err := json.Unmarshal(value, &status); err != nil {
					return nil, nil, false, err
				}
			}
			if status == "aborted" {
				return raw, nil, false, nil
			}
		}
		var role string
		if fields["role"] != nil {
			if err := json.Unmarshal(fields["role"], &role); err != nil {
				return nil, nil, false, err
			}
		}
		var message struct {
			Role string `json:"role"`
		}
		if fields["message"] != nil {
			if err := json.Unmarshal(fields["message"], &message); err != nil && kind == "" {
				return nil, nil, false, err
			}
		}
		role = firstNonEmpty(role, message.Role)
		if !unknown && role != "" && !slices.Contains(cursorCaptureRoleKinds(), role) {
			kind, namespace, unknown = role, "role", true
		}
	case HarnessStrike:
		unknown = kind != "" && !isKnownStrikeEvent(strikeEventType(kind))
	}
	if unknown {
		record, err := NewRetainedUnknownFromSource(harness, namespace, kind, UnknownSourcePosition{Line: line}, raw)
		return nil, []RetainedUnknown{record}, true, err
	}
	var records []RetainedUnknown
	filter := func(object map[string]json.RawMessage, key, pointer string) error {
		value, exists := object[key]
		if !exists {
			return nil
		}
		filtered, found, err := filterUnknownBlocks(harness, value, line, pointer)
		if err != nil {
			return err
		}
		object[key] = filtered
		records = append(records, found...)
		return nil
	}
	if harness == HarnessStrike {
		var event map[string]json.RawMessage
		if err := json.Unmarshal(fields["data"], &event); err != nil {
			return nil, nil, false, err
		}
		if event != nil {
			if err := filter(event, "content", "/data/content"); err != nil {
				return nil, nil, false, err
			}
			if err := filter(event, "message", "/data/message"); err != nil {
				return nil, nil, false, err
			}
			fields["data"], _ = json.Marshal(event)
		}
	} else {
		if err := filter(fields, "content", "/content"); err != nil {
			return nil, nil, false, err
		}
		if harness == HarnessClaudeCode && kind == "result" {
			if err := filter(fields, "result", "/result"); err != nil {
				return nil, nil, false, err
			}
		}
		if value := fields["message"]; value != nil {
			var message map[string]json.RawMessage
			if err := json.Unmarshal(value, &message); err != nil {
				return nil, nil, false, err
			}
			if err := filter(message, "content", "/message/content"); err != nil {
				return nil, nil, false, err
			}
			fields["message"], _ = json.Marshal(message)
		}
	}
	if len(records) == 0 {
		return raw, nil, false, nil
	}
	filtered, err := json.Marshal(fields)
	return filtered, records, false, err
}

func filterUnknownBlocks(harness Harness, raw json.RawMessage, line int, pointer string) (json.RawMessage, []RetainedUnknown, error) {
	trimmed := bytes.TrimSpace(raw)
	if harness == HarnessStrike && len(trimmed) > 0 && trimmed[0] == '{' {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, nil, err
		}
		if value := message["content"]; value != nil {
			content, records, err := filterUnknownBlocks(harness, value, line, pointer+"/content")
			if err != nil {
				return nil, nil, err
			}
			if len(records) > 0 {
				message["content"] = content
				encoded, err := json.Marshal(message)
				return encoded, records, err
			}
		}
	}
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return raw, nil, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, nil, err
	}
	kept := make([]json.RawMessage, 0, len(blocks))
	var records []RetainedUnknown
	for i, block := range blocks {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(block, &fields); err != nil || fields == nil {
			return nil, nil, fmt.Errorf("content block must be an object")
		}
		var kind string
		if err := json.Unmarshal(fields["type"], &kind); err != nil || kind == "" {
			return nil, nil, fmt.Errorf("content block lacks a string type")
		}
		at := pointer + "/" + strconv.Itoa(i)
		if !slices.Contains(captureContentBlockKinds(harness), kind) {
			record, err := NewRetainedUnknownFromSource(harness, "content_block", kind, UnknownSourcePosition{Line: line, JSONPointer: at}, block)
			if err != nil {
				return nil, nil, err
			}
			records = append(records, record)
			continue
		}
		if kind == "tool_result" && fields["content"] != nil {
			content, nested, err := filterUnknownBlocks(harness, fields["content"], line, at+"/content")
			if err != nil {
				return nil, nil, err
			}
			if len(nested) > 0 {
				fields["content"] = content
				block, _ = json.Marshal(fields)
				records = append(records, nested...)
			}
		}
		if harness == HarnessStrike {
			one := append([]byte{'['}, block...)
			one = append(one, ']')
			if err := validateCaptureContent(harness, one, false); err != nil {
				return nil, nil, err
			}
		}
		kept = append(kept, block)
	}
	if len(records) == 0 {
		return raw, nil, nil
	}
	filtered, err := json.Marshal(kept)
	return filtered, records, err
}

// Validation still examines every known sibling. Unknown evidence does not
// short-circuit malformed known records elsewhere in the source.
func validateRetainingJSONL(ctx context.Context, session DiscoveredSession, data []byte, validate func([]byte) (*IgnoredSourceRecord, error)) ([]IgnoredSourceRecord, error) {
	omitted := session.ContentOmitted
	session.ContentOmitted = false
	sawOmission := false
	ignored, err := validateCaptureJSONL(ctx, session, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		if _, yes := parseOmittedRecordSentinel(raw); yes {
			sawOmission = true
			return nil, nil
		}
		filtered, _, whole, err := prepareUnknownJSONL(session.Harness, raw, 1)
		if err != nil || whole {
			return nil, err
		}
		return validate(filtered)
	})
	if err != nil {
		return nil, &captureCorruptionError{err: err}
	}
	if err == nil && omitted && !sawOmission {
		return nil, captureFailure(session, 0, fmt.Errorf("the retained transcript omits source records without positional evidence; restore the source before certifying capture"))
	}
	return ignored, err
}

func cursorCaptureRoleKinds() []string {
	return append(captureRoleKinds(), "human")
}
