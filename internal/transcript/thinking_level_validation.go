package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// ThinkingLevelEntryWarning is one malformed stored per-turn thinking-level
// evidence occurrence found while reading indexed entries. Stored evidence is
// dropped with this warning, never refused: the fold drops the malformed half
// and keeps a well-formed surviving half (see thinkingLevelObservation), so the
// warning lets an operator repair or re-index the source instead of losing the
// diagnostic.
type ThinkingLevelEntryWarning struct {
	SessionID  schema.SessionID
	EntryIndex int
	Role       schema.Role
	Level      schema.ThinkingLevel
	Raw        schema.ThinkingLevelRaw
	// Why names the exact contract rule the stored value violates.
	Why string
	// Fix names the concrete repair for the stored row.
	Fix string
}

// Message renders the actionable what/why/where/when/meaning/fix diagnostic
// for one dropped occurrence.
func (w ThinkingLevelEntryWarning) Message() string {
	return fmt.Sprintf("thinking-level stored evidence dropped\n  what: indexed entry %d (role %q) carries malformed thinking-level evidence level=%q raw=%q\n  why: %s\n  where: transcript.ThinkingLevelEntryWarnings during storage read\n  when: while folding stored entries into a projection, before local viewing, export, or publication\n  meaning: the malformed half is dropped and served absent, never guessed or normalized; a well-formed surviving half is still served, so this turn's level is unknown only when the canonical level was dropped. An explicit JSON null is treated as omission and drops only that half, a non-string half fails the entry's typed decode and drops the whole pair, and evidence on a non-assistant entry is dropped entirely; any of these yields one warning for the entry, and the source must be repaired to restore it\n  fix: %s", w.EntryIndex, w.Role, w.Level, w.Raw, w.Why, w.Fix)
}

// ThinkingLevelEntryWarnings collects one warning per indexed entry whose
// stored Extra carries malformed thinking-level evidence: an explicit JSON
// null, a non-string thinking_level/thinking_level_raw value, an out-of-set
// canonical level, an invalid raw spelling, or non-assistant attribution. A
// malformed half yields one warning and stops examining that entry, so an entry
// with two malformed halves (including two nulls) still yields exactly one
// warning. It is a pure collector: it never mutates entries and never returns an
// error, because malformed stored evidence is dropped rather than refused. The
// producer and wire boundaries still refuse malformed turns
// (ValidateThinkingLevelTurnEvidence, schema.ValidateSessionDetailPayload).
func ThinkingLevelEntryWarnings(entries []schema.SessionEntry) []ThinkingLevelEntryWarning {
	var warnings []ThinkingLevelEntryWarning
	for _, entry := range entries {
		if entry.Extra == nil {
			continue
		}
		var extra map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*entry.Extra), &extra); err != nil {
			continue
		}
		levelRaw, hasLevel := extra[ingest.ThinkingLevelExtraKey]
		rawRaw, hasRaw := extra[ingest.ThinkingLevelRawExtraKey]
		if !hasLevel && !hasRaw {
			continue
		}
		var level schema.ThinkingLevel
		if hasLevel {
			if isStoredJSONNull(levelRaw) {
				warnings = append(warnings, ThinkingLevelEntryWarning{
					SessionID:  entry.SessionID,
					EntryIndex: entry.EntryIndex,
					Role:       entry.Role,
					Why:        "thinking_level must be an exact canonical string, but the stored JSON value is null",
					Fix:        "repair or re-index the source entry so thinking_level is an exact canonical string, or omit the field",
				})
				continue
			}
			var value string
			if err := json.Unmarshal(levelRaw, &value); err != nil {
				warnings = append(warnings, ThinkingLevelEntryWarning{
					SessionID:  entry.SessionID,
					EntryIndex: entry.EntryIndex,
					Role:       entry.Role,
					Why:        fmt.Sprintf("thinking_level must be an exact canonical string, but the stored JSON value is not a string: %v", err),
					Fix:        "repair or re-index the source entry so thinking_level is an exact canonical string",
				})
				continue
			}
			level = schema.ThinkingLevel(value)
		}
		var raw schema.ThinkingLevelRaw
		if hasRaw {
			if isStoredJSONNull(rawRaw) {
				warnings = append(warnings, ThinkingLevelEntryWarning{
					SessionID:  entry.SessionID,
					EntryIndex: entry.EntryIndex,
					Role:       entry.Role,
					Why:        "thinking_level_raw must be an exact bounded string, but the stored JSON value is null",
					Fix:        "repair or re-index the source entry so thinking_level_raw is an exact bounded string, or omit the field",
				})
				continue
			}
			var value string
			if err := json.Unmarshal(rawRaw, &value); err != nil {
				warnings = append(warnings, ThinkingLevelEntryWarning{
					SessionID:  entry.SessionID,
					EntryIndex: entry.EntryIndex,
					Role:       entry.Role,
					Why:        fmt.Sprintf("thinking_level_raw must be an exact bounded string, but the stored JSON value is not a string: %v", err),
					Fix:        "repair or re-index the source entry so thinking_level_raw is an exact bounded string",
				})
				continue
			}
			raw = schema.ThinkingLevelRaw(value)
		}
		if err := schema.ValidateThinkingLevelEvidence(entry.Role, level, raw); err != nil {
			warnings = append(warnings, ThinkingLevelEntryWarning{
				SessionID:  entry.SessionID,
				EntryIndex: entry.EntryIndex,
				Role:       entry.Role,
				Level:      level,
				Raw:        raw,
				Why:        err.Error(),
				Fix:        "keep thinking_level/thinking_level_raw only on assistant entries; omit unknown canonical levels, and preserve exact valid raw spellings (at most 128 UTF-8 bytes without edge whitespace)",
			})
		}
	}
	return warnings
}

// isStoredJSONNull reports whether a present stored field's JSON value is the
// literal null. A null decodes into the typed string as an empty value with no
// error, so it must be detected on the raw JSON to be reported as a dropped
// occurrence instead of silently reading as omission.
func isStoredJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// ValidateThinkingLevelTurnEvidence enforces the producer-only rule that
// thinking-level evidence appears only on assistant output with an
// in-contract canonical level and raw spelling.
func ValidateThinkingLevelTurnEvidence(turn ingest.Turn) error {
	if err := schema.ValidateThinkingLevelEvidence(turn.Role, turn.ThinkingLevel, turn.ThinkingLevelRaw); err != nil {
		return fmt.Errorf("thinking-level producer validation failed\n  what: turn %d (role %q) carries thinking-level evidence level=%q raw=%q that the contract refuses\n  why: %v\n  where: transcript.ValidateThinkingLevelTurnEvidence\n  when: during SessionDetailPayload construction, before local, exported, or remote emission\n  meaning: no payload can be emitted because the evidence would be misattributed or out of contract\n  fix: keep thinking_level/thinking_level_raw only on assistant entries; omit unknown canonical levels, preserve exact valid raw spellings (at most 128 UTF-8 bytes without edge whitespace), repair or re-index the source, and retry", turn.Index, turn.Role, turn.ThinkingLevel, turn.ThinkingLevelRaw, err)
	}
	return nil
}

func validateSessionThinkingLevelEvidence(session *ingest.Session) error {
	for _, turn := range session.Turns {
		if err := ValidateThinkingLevelTurnEvidence(turn); err != nil {
			return err
		}
	}
	for _, section := range session.EarlierHistory {
		for _, turn := range section.Turns {
			if err := ValidateThinkingLevelTurnEvidence(turn); err != nil {
				return err
			}
		}
	}
	return nil
}
