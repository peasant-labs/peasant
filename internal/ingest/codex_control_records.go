package ingest

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

const codexControlPayloadLimit = 8192

const (
	codexTypeWorldState = "world_state"
	codexTypeCompacted  = "compacted"

	codexEventSubAgentActivity      = "sub_agent_activity"
	codexEventPatchApplyEnd         = "patch_apply_end"
	codexEventThreadSettingsApplied = "thread_settings_applied"
	codexEventWebSearchEnd          = "web_search_end"
)

// codexControlRecordKind names the known rollout-level and event-level
// annotations represented as depth-0 system entries. Unknown records return
// ok=false so the retained-unknown boundary can preserve them as opaque evidence.
func codexControlRecordKind(env codexRolloutLine) (kind string, payload json.RawMessage, ok bool, err error) {
	if err := requireJSONObject(env.Payload); err != nil {
		return "", nil, false, fmt.Errorf("rollout payload: %w", err)
	}
	switch env.Type {
	case codexTypeWorldState, codexTypeCompacted:
		return env.Type, env.Payload, true, nil
	case codexTypeEventMsg:
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(env.Payload, &header); err != nil {
			return "", nil, false, err
		}
		switch header.Type {
		case codexEventSubAgentActivity, codexEventPatchApplyEnd, codexEventThreadSettingsApplied, codexEventWebSearchEnd:
			return header.Type, env.Payload, true, nil
		}
	}
	return "", nil, false, nil
}

func codexControlRecordEntry(sessionID SessionID, index int, env codexRolloutLine, rawLen int, fullContent bool) (schema.SessionEntry, bool, error) {
	kind, payload, ok, err := codexControlRecordKind(env)
	if err != nil || !ok {
		return schema.SessionEntry{}, false, err
	}
	entry := schema.SessionEntry{
		SessionID:     sessionID,
		EntryIndex:    index,
		Harness:       HarnessCodex,
		Role:          RoleSystem,
		EntryType:     EntryTypeSystem,
		Depth:         0,
		RawByteLength: &rawLen,
		PartType:      &kind,
		Extra:         codexControlPayload(kind, payload),
	}
	if ms := parseTimestampMillis(env.Timestamp); ms > 0 {
		entry.TimestampMs = &ms
	}
	if preview := codexControlPreview(kind, payload); preview != "" {
		if !fullContent {
			preview = truncateString(preview, defaults.ContentPreviewLimit)
		}
		entry.ContentPreview = &preview
	}
	return entry, true, nil
}

func codexControlPayload(kind string, raw json.RawMessage) *string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	delete(payload, "type")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	if len(encoded) > codexControlPayloadLimit {
		encoded, err = json.Marshal(map[string]any{"kind": kind, "rawBytes": len(encoded)})
		if err != nil {
			return nil
		}
	}
	value := string(encoded)
	return &value
}

func codexControlPreview(kind string, raw json.RawMessage) string {
	var payload struct {
		Kind      string `json:"kind"`
		AgentPath string `json:"agent_path"`
		Query     string `json:"query"`
	}
	_ = json.Unmarshal(raw, &payload)
	switch kind {
	case codexEventSubAgentActivity:
		detail := firstNonEmpty(payload.Kind, payload.AgentPath)
		if payload.Kind != "" && payload.AgentPath != "" {
			detail = payload.Kind + " " + payload.AgentPath
		}
		if detail != "" {
			return "Sub-agent activity: " + detail
		}
		return "Sub-agent activity"
	case codexEventPatchApplyEnd:
		return "Patch application completed"
	case codexTypeCompacted:
		return "Context compacted"
	case codexEventWebSearchEnd:
		if payload.Query != "" {
			return "Web search completed: " + payload.Query
		}
		return "Web search completed"
	default:
		return ""
	}
}
