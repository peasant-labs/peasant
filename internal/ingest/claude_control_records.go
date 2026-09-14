package ingest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// claudeControlRecordTypes is the closed set of Claude record kinds whose
// payload is retained as a represented control entry instead of refused. The
// kinds carry harness state rather than conversation content, so a transcript
// holding them can still be certified complete.
//
// The artifact- prefix family is matched by prefix, not enumerated.
var claudeControlRecordTypes = map[string]bool{
	"agent-setting":      true,
	"permission-mode":    true,
	"mode":               true,
	"agent-name":         true,
	"ai-title":           true,
	"atis-latch":         true,
	"attachment":         true,
	"bridge-session":     true,
	"started":            true,
	"cost-state":         true,
	"custom-title":       true,
	"file-history-delta": true,
	"frame-link":         true,
	"fork-context-ref":   true,
	"worktree-state":     true,
	"launched":           true,
	"pr-link":            true,
}

// claudeControlRecordEnvelopeKeys describe the transcript envelope around a
// record rather than the record's own state. They are dropped from the
// retained payload so a reader sees only what the record changed. The scope
// markers (agentId, agentName, isSidechain) are deliberately KEPT.
var claudeControlRecordEnvelopeKeys = map[string]bool{
	"type":              true,
	"sessionId":         true,
	"session_id":        true,
	"uuid":              true,
	"parentUuid":        true,
	"logicalParentUuid": true,
	"timestamp":         true,
	"cwd":               true,
	"gitBranch":         true,
	"entrypoint":        true,
	"userType":          true,
	"version":           true,
	"slug":              true,
}

// claudeControlPayloadLimit caps the retained payload. A larger payload keeps
// only an identity object so one oversized record cannot bloat the index.
const claudeControlPayloadLimit = 8192

// isClaudeControlRecordType reports whether a record type names a control
// record this build represents.
func isClaudeControlRecordType(recordType string) bool {
	if strings.HasPrefix(recordType, "artifact-") {
		return true
	}
	return claudeControlRecordTypes[recordType]
}

// claudeControlRecordKind returns the provider kind label for a control record
// this build represents, or "" when the record is not a control record. The
// compact_boundary system subtype is labeled "compact-boundary".
func claudeControlRecordKind(line claudeIndexLine) string {
	if line.Type == "system" && line.Subtype == "compact_boundary" {
		return "compact-boundary"
	}
	if isClaudeControlRecordType(line.Type) {
		return line.Type
	}
	return ""
}

// claudeControlRecordFields enriches a depth=0 row with the control record's
// kind, its retained payload and a short preview. It returns nil values for a
// record this build does not treat as a control record.
func claudeControlRecordFields(raw []byte, line claudeIndexLine) (partType *string, extra *string, preview *string) {
	kind := claudeControlRecordKind(line)
	if kind == "" {
		return nil, nil, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, nil, nil
	}
	stripped := stripClaudeControlEnvelope(payload)
	label := kind
	extra = claudeControlRetainedPayload(kind, stripped)
	if text := claudeControlPreview(kind, stripped); text != "" {
		preview = &text
	}
	return &label, extra, preview
}

// stripClaudeControlEnvelope returns the payload without its transcript
// envelope fields. The map is new; the caller's payload is not altered.
func stripClaudeControlEnvelope(payload map[string]json.RawMessage) map[string]json.RawMessage {
	stripped := make(map[string]json.RawMessage, len(payload))
	for key, value := range payload {
		if claudeControlRecordEnvelopeKeys[key] {
			continue
		}
		stripped[key] = value
	}
	return stripped
}

// claudeControlRetainedPayload encodes the stripped payload, replacing it with
// an identity object once it exceeds the cap. rawBytes records the stripped
// payload size that tripped the cap.
func claudeControlRetainedPayload(kind string, payload map[string]json.RawMessage) *string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	if len(encoded) > claudeControlPayloadLimit {
		encoded, err = claudeControlIdentity(kind, payload, len(encoded))
		if err != nil {
			return nil
		}
	}
	value := string(encoded)
	return &value
}

func claudeControlIdentity(kind string, payload map[string]json.RawMessage, rawBytes int) ([]byte, error) {
	identity := map[string]json.RawMessage{}
	encodedKind, err := json.Marshal(kind)
	if err != nil {
		return nil, err
	}
	identity["kind"] = encodedKind
	if kind == "attachment" {
		if attachmentType, ok := payload["attachmentType"]; ok {
			identity["attachmentType"] = attachmentType
		}
	}
	identity["rawBytes"] = json.RawMessage(strconv.Itoa(rawBytes))
	return json.Marshal(identity)
}

// claudeControlPreview builds the short, factual preview for a control kind. An
// empty result means the kind keeps no preview (attachment) or carries no
// readable value.
func claudeControlPreview(kind string, payload map[string]json.RawMessage) string {
	switch kind {
	case "permission-mode":
		if value := claudeControlValue(payload, "permissionMode", "mode", "value", "setting"); value != "" {
			return "permission mode: " + value
		}
		return "permission mode changed"
	case "mode":
		if value := claudeControlValue(payload, "mode", "value"); value != "" {
			return "mode: " + value
		}
		return "mode changed"
	case "agent-setting":
		if value := claudeControlValue(payload, "agentSetting", "setting", "agent", "value"); value != "" {
			return "agent: " + value
		}
		return "agent setting changed"
	case "agent-name":
		if value := claudeControlValue(payload, "agentName", "name", "value"); value != "" {
			return "agent name: " + value
		}
		return "agent renamed"
	case "ai-title":
		if value := claudeControlValue(payload, "title", "aiTitle", "value"); value != "" {
			return "session title: " + value
		}
		return "session title set"
	case "pr-link":
		number := claudeControlValue(payload, "number", "prNumber", "pr")
		repository := claudeControlValue(payload, "repository", "repo", "repositoryName")
		switch {
		case number != "" && repository != "":
			return "PR #" + number + " (" + repository + ")"
		case number != "":
			return "PR #" + number
		case repository != "":
			return "PR linked (" + repository + ")"
		}
		return "PR linked"
	case "compact-boundary":
		return claudeCompactBoundaryPreview(payload)
	case "cost-state":
		return claudeCostStatePreview(payload)
	case "started":
		if value := claudeControlValue(payload, "key", "agent", "name", "id"); value != "" {
			return "started: " + value
		}
		return "started"
	case "attachment":
		// Attachments are tracked through the retained payload only.
		return ""
	default:
		if value := claudeControlFirstValue(payload); value != "" {
			return kind + ": " + value
		}
		return kind
	}
}

// claudeCompactBoundaryPreview summarizes a context compaction, omitting parts
// the payload does not carry.
func claudeCompactBoundaryPreview(payload map[string]json.RawMessage) string {
	var metadata struct {
		Trigger    string `json:"trigger"`
		PreTokens  *int   `json:"preTokens"`
		PostTokens *int   `json:"postTokens"`
		DurationMs *int   `json:"durationMs"`
	}
	if raw, ok := payload["compactMetadata"]; ok {
		_ = json.Unmarshal(raw, &metadata)
	}
	text := "Context compacted"
	if metadata.Trigger != "" {
		text += " (" + metadata.Trigger + ")"
	}
	switch {
	case metadata.PreTokens != nil && metadata.PostTokens != nil:
		text += fmt.Sprintf(": %d -> %d tokens", *metadata.PreTokens, *metadata.PostTokens)
	case metadata.PreTokens != nil:
		text += fmt.Sprintf(": %d tokens before", *metadata.PreTokens)
	case metadata.PostTokens != nil:
		text += fmt.Sprintf(": %d tokens after", *metadata.PostTokens)
	}
	if metadata.DurationMs != nil && len(text) <= 160 {
		text += fmt.Sprintf(" in %dms", *metadata.DurationMs)
	}
	return text
}

// claudeCostStatePreview summarizes the recorded cost, duration and line
// changes, joining only the parts the payload carries.
func claudeCostStatePreview(payload map[string]json.RawMessage) string {
	var parts []string
	if value := claudeControlValue(payload, "costUsd", "totalCostUsd", "cost"); value != "" {
		parts = append(parts, "cost: "+value)
	}
	if value := claudeControlValue(payload, "durationMs", "duration"); value != "" {
		parts = append(parts, "duration: "+value+"ms")
	}
	added := claudeControlValue(payload, "linesAdded", "added")
	removed := claudeControlValue(payload, "linesRemoved", "removed")
	switch {
	case added != "" && removed != "":
		parts = append(parts, "lines: +"+added+"/-"+removed)
	case added != "":
		parts = append(parts, "lines: +"+added)
	case removed != "":
		parts = append(parts, "lines: -"+removed)
	}
	if len(parts) == 0 {
		return "cost state updated"
	}
	return strings.Join(parts, ", ")
}

// claudeControlValue returns the first non-empty scalar among the named keys.
func claudeControlValue(payload map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if raw, ok := payload[key]; ok {
			if value := claudeScalarText(raw); value != "" {
				return value
			}
		}
	}
	return ""
}

// claudeControlFirstValue returns the first scalar in the payload in a stable
// key order, skipping the envelope leftovers that name the kind itself.
func claudeControlFirstValue(payload map[string]json.RawMessage) string {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		if key == "type" || key == "subtype" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if value := claudeScalarText(payload[key]); value != "" {
			return value
		}
	}
	return ""
}

// claudeScalarText decodes a JSON string or number into its plain text form.
func claudeScalarText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}
