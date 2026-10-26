package ingest

import (
	"encoding/json"
	"fmt"
)

// addThinkingLevelObservation records a mapped thinking-level observation on
// an assistant entry's Extra JSON under ThinkingLevelExtraKey and
// ThinkingLevelRawExtraKey. It keeps the existing Extra fields, writes only
// the non-empty halves of the mapping, and never overwrites an observation the
// entry already carries. A zero mapping or undecodable Extra leaves the entry
// unchanged, so no level is ever fabricated. Callers stamp assistant entries
// only.
func addThinkingLevelObservation(extraJSON *string, mapping thinkingLevelMapping) *string {
	if mapping.IsZero() {
		return extraJSON
	}
	extra := map[string]json.RawMessage{}
	if extraJSON != nil {
		if json.Unmarshal([]byte(*extraJSON), &extra) != nil {
			return extraJSON
		}
	}
	if extra == nil {
		extra = make(map[string]json.RawMessage)
	}
	if _, present := extra[ThinkingLevelExtraKey]; present {
		return extraJSON
	}
	if _, present := extra[ThinkingLevelRawExtraKey]; present {
		return extraJSON
	}
	if mapping.Level != "" {
		encoded, err := json.Marshal(mapping.Level)
		if err != nil {
			return extraJSON
		}
		extra[ThinkingLevelExtraKey] = encoded
	}
	if mapping.Raw != "" {
		encoded, err := json.Marshal(mapping.Raw)
		if err != nil {
			return extraJSON
		}
		extra[ThinkingLevelRawExtraKey] = encoded
	}
	value, err := json.Marshal(extra)
	if err != nil {
		return extraJSON
	}
	result := string(value)
	return &result
}

// removeThinkingLevelObservation strips thinking-level evidence from an entry
// that is not (or is no longer) an assistant entry. It returns nil when
// nothing else remains in Extra.
func removeThinkingLevelObservation(extraJSON *string) *string {
	if extraJSON == nil {
		return nil
	}
	var extra map[string]json.RawMessage
	if json.Unmarshal([]byte(*extraJSON), &extra) != nil {
		return extraJSON
	}
	_, hasLevel := extra[ThinkingLevelExtraKey]
	_, hasRaw := extra[ThinkingLevelRawExtraKey]
	if !hasLevel && !hasRaw {
		return extraJSON
	}
	delete(extra, ThinkingLevelExtraKey)
	delete(extra, ThinkingLevelRawExtraKey)
	if len(extra) == 0 {
		return nil
	}
	encoded, err := json.Marshal(extra)
	if err != nil {
		return extraJSON
	}
	value := string(encoded)
	return &value
}

// appendThinkingLevelDiagnostic records a thinking_level_raw_dropped
// diagnostic as a metadata warning, naming the source line when known.
func appendThinkingLevelDiagnostic(meta *UnifiedMetadata, diagnostic *DiagnosticEntry, line int) {
	if meta == nil || diagnostic == nil {
		return
	}
	entry := *diagnostic
	if line > 0 {
		entry.Location = fmt.Sprintf("%s, line %d", entry.Location, line)
	}
	meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, entry)
}
