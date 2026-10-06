package ingest

import "encoding/json"

// Both native JSON and managed projections seed from the same source-ordered
// messages. A session-row variant is only a fallback when no message has evidence.
func applyOpenCodeThinkingSeed(metadata *UnifiedMetadata, messages []openCodeSemanticMessage, sessionVariant string) {
	for _, message := range messages {
		if message.Data.Role != RoleAssistant.String() {
			continue
		}
		var evidence struct {
			Model json.RawMessage `json:"model"`
		}
		if json.Unmarshal(message.Raw, &evidence) != nil {
			continue
		}
		mapping, diagnostic := mapOpenCodeVariant(decodeOpenCodeModelVariant(evidence.Model))
		if diagnostic != nil {
			metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, *diagnostic)
		}
		applyThinkingLevelObservation(metadata, mapping)
	}
	if metadata.ThinkingLevel == "" && metadata.ThinkingLevelRaw == "" {
		mapping, diagnostic := mapOpenCodeVariant(sessionVariant)
		if diagnostic != nil {
			metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, *diagnostic)
		}
		applyThinkingLevelSeed(metadata, mapping)
	}
}
