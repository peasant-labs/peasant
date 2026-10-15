package transcript

import (
	"encoding/json"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

type entryModelObservation struct {
	present bool
	value   string
}

func modelObservation(entry schema.SessionEntry) entryModelObservation {
	if entry.Role != schema.RoleAssistant || entry.Extra == nil {
		return entryModelObservation{}
	}
	var extra map[string]json.RawMessage
	if json.Unmarshal([]byte(*entry.Extra), &extra) != nil {
		return entryModelObservation{}
	}
	raw, ok := extra["model_id"]
	if !ok {
		return entryModelObservation{}
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !ingest.ValidObservedModel(value) {
		return entryModelObservation{}
	}
	return entryModelObservation{present: true, value: value}
}

// thinkingLevelObservation reads the per-entry thinking-level evidence from an
// assistant entry's Extra. Non-assistant entries never carry the evidence, and
// a malformed or out-of-contract value is dropped rather than normalized: the
// malformed half is served absent while a well-formed surviving half is still
// served, so this turn's level is unknown only when the canonical level was
// dropped. An explicit JSON null decodes to nil here and is served absent, a
// non-string half fails the typed decode and drops the whole pair, and
// ThinkingLevelEntryWarnings reports each drop so the source can be repaired.
func thinkingLevelObservation(entry schema.SessionEntry) (schema.ThinkingLevel, schema.ThinkingLevelRaw) {
	if entry.Role != schema.RoleAssistant || entry.Extra == nil {
		return "", ""
	}
	var extra struct {
		Level *string `json:"thinking_level"`
		Raw   *string `json:"thinking_level_raw"`
	}
	if json.Unmarshal([]byte(*entry.Extra), &extra) != nil {
		return "", ""
	}
	var level schema.ThinkingLevel
	if extra.Level != nil && schema.ThinkingLevel(*extra.Level).IsValid() {
		level = schema.ThinkingLevel(*extra.Level)
	}
	var raw schema.ThinkingLevelRaw
	if extra.Raw != nil {
		raw, _ = schema.NewThinkingLevelRaw(*extra.Raw)
	}
	return level, raw
}
