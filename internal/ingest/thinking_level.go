package ingest

import (
	"fmt"

	schema "github.com/peasant-labs/schema"
)

// Entry Extra keys carrying per-turn thinking-level evidence. They are written
// on assistant entries only; the transcript fold lifts them onto ingest.Turn.
const (
	ThinkingLevelExtraKey    = "thinking_level"
	ThinkingLevelRawExtraKey = "thinking_level_raw"
)

// ThinkingLevelRawDroppedErrorType names the diagnostic emitted when a native
// thinking-level spelling cannot be carried as raw evidence (invalid UTF-8,
// over the byte bound, or edge whitespace). The observation then contributes
// neither a canonical level nor raw evidence; the session still ingests.
const ThinkingLevelRawDroppedErrorType = "thinking_level_raw_dropped"

// thinkingLevelMapping is the result of mapping one native thinking-level
// observation. Level empty means unknown (absent), never off. Raw carries the
// exact native spelling when it differs from Level, including when no
// canonical level could be mapped. Both empty means no usable evidence.
type thinkingLevelMapping struct {
	Level schema.ThinkingLevel
	Raw   schema.ThinkingLevelRaw
}

// IsZero reports whether the mapping carries no evidence at all.
func (m thinkingLevelMapping) IsZero() bool {
	return m.Level == "" && m.Raw == ""
}

// thinkingLevelRule describes one harness vocabulary. Every exact canonical
// spelling (schema.AllThinkingLevels) maps to itself without raw, checked
// before any alias; aliases map to a fixed canonical (possibly empty =
// unknown) and keep the native spelling as raw. Anything else is unknown with
// the exact native bytes as raw.
type thinkingLevelRule struct {
	source  string
	aliases map[string]schema.ThinkingLevel
}

var (
	codexEffortRule = thinkingLevelRule{
		source: "Codex turn_context.payload.effort",
		aliases: map[string]schema.ThinkingLevel{
			"none":       schema.ThinkingLevelOff,
			"persistent": schema.ThinkingLevelOff,
			"custom":     "",
		},
	}
	claudeEffortRule = thinkingLevelRule{
		source: "Claude Code assistant effort",
		aliases: map[string]schema.ThinkingLevel{
			"none":      schema.ThinkingLevelOff,
			"ultracode": schema.ThinkingLevelUltra,
		},
	}
	claudeLegacyLevelRule = thinkingLevelRule{
		source: "Claude Code thinkingMetadata.level",
		aliases: map[string]schema.ThinkingLevel{
			"none": schema.ThinkingLevelOff,
		},
	}
	openCodeVariantRule = thinkingLevelRule{
		source: "OpenCode model.variant",
		aliases: map[string]schema.ThinkingLevel{
			"none":    schema.ThinkingLevelOff,
			"default": "",
		},
	}
	piThinkingLevelRule = thinkingLevelRule{
		source: "Pi thinkingLevel",
	}
)

// mapCodexEffort maps a Codex turn_context effort value.
func mapCodexEffort(native string) (thinkingLevelMapping, *DiagnosticEntry) {
	return codexEffortRule.apply(native)
}

// mapClaudeEffort maps a Claude Code assistant-line effort value, including
// the native ultracode mode name (ultra with raw "ultracode").
func mapClaudeEffort(native string) (thinkingLevelMapping, *DiagnosticEntry) {
	return claudeEffortRule.apply(native)
}

// mapClaudeLegacyLevel maps the legacy Claude Code thinkingMetadata.level
// value. Numeric thinking budgets are never passed here and never mapped.
func mapClaudeLegacyLevel(native string) (thinkingLevelMapping, *DiagnosticEntry) {
	return claudeLegacyLevelRule.apply(native)
}

// mapOpenCodeVariant maps an OpenCode message or session model.variant.
func mapOpenCodeVariant(native string) (thinkingLevelMapping, *DiagnosticEntry) {
	return openCodeVariantRule.apply(native)
}

// mapPiThinkingLevel maps a Pi thinking_level_change or assistant-record
// thinkingLevel value.
func mapPiThinkingLevel(native string) (thinkingLevelMapping, *DiagnosticEntry) {
	return piThinkingLevelRule.apply(native)
}

// apply maps one native value. Nothing is trimmed or case-folded: identity is
// exact-byte. An empty native value is "not observed" and yields the zero
// mapping. A raw that cannot satisfy schema.NewThinkingLevelRaw is dropped
// with one diagnostic; no canonical level is fabricated in its place.
func (r thinkingLevelRule) apply(native string) (thinkingLevelMapping, *DiagnosticEntry) {
	if native == "" {
		return thinkingLevelMapping{}, nil
	}
	if level := schema.ThinkingLevel(native); level.IsValid() {
		return thinkingLevelMapping{Level: level}, nil
	}
	level := r.aliases[native]
	raw, err := schema.NewThinkingLevelRaw(native)
	if err != nil {
		return thinkingLevelMapping{Level: level}, &DiagnosticEntry{
			ErrorType: ThinkingLevelRawDroppedErrorType,
			Location:  r.source,
			Message: fmt.Sprintf("native thinking-level value from %s cannot be kept as thinkingLevelRaw evidence, so the observation is recorded without raw and no canonical level is invented: %v",
				r.source, err),
			Remediation: "No action is needed for ingest: the session is kept and the level stays unknown for this observation. If the harness should record a usable value, report the native spelling so the mapping table can be extended.",
		}
	}
	return thinkingLevelMapping{Level: level, Raw: raw}, nil
}

// applyThinkingLevelSeed records the session-level seed from the earliest
// valid observation. It sets nothing when the metadata already carries a
// level or raw (the earliest observation wins, including an unknown one with
// raw evidence) or when the mapping is empty. It reports whether it set the
// seed.
func applyThinkingLevelSeed(meta *UnifiedMetadata, mapping thinkingLevelMapping) bool {
	if meta == nil || mapping.IsZero() {
		return false
	}
	if meta.ThinkingLevel != "" || meta.ThinkingLevelRaw != "" {
		return false
	}
	meta.ThinkingLevel = mapping.Level
	meta.ThinkingLevelRaw = mapping.Raw
	return true
}
