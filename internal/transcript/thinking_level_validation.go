package transcript

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// ValidateThinkingLevelEntries enforces exact string typing, value, and
// assistant attribution at the indexed-entry trust boundary. It mirrors
// ValidateObservedModelEntries for thinking-level evidence: a stored row whose
// Extra carries a non-string value, an out-of-set canonical level, or an
// invalid raw spelling is refused instead of being silently folded to absent.
func ValidateThinkingLevelEntries(entries []schema.SessionEntry) error {
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
			var value string
			if err := json.Unmarshal(levelRaw, &value); err != nil {
				return fmt.Errorf("thinking-level entry validation failed\n  what: indexed entry %d carries a non-string thinking_level value\n  why: thinking-level source evidence must be an exact string: %v\n  where: transcript.ValidateThinkingLevelEntries\n  when: after storage read and before entry folding or payload emission\n  meaning: no local, exported, or remote payload can be emitted without silently changing or dropping evidence\n  fix: repair or re-index the source entry so thinking_level is an exact canonical string, then retry", entry.EntryIndex, err)
			}
			level = schema.ThinkingLevel(value)
		}
		var raw schema.ThinkingLevelRaw
		if hasRaw {
			var value string
			if err := json.Unmarshal(rawRaw, &value); err != nil {
				return fmt.Errorf("thinking-level entry validation failed\n  what: indexed entry %d carries a non-string thinking_level_raw value\n  why: thinking-level source evidence must be an exact string: %v\n  where: transcript.ValidateThinkingLevelEntries\n  when: after storage read and before entry folding or payload emission\n  meaning: no local, exported, or remote payload can be emitted without silently changing or dropping evidence\n  fix: repair or re-index the source entry so thinking_level_raw is an exact bounded string, then retry", entry.EntryIndex, err)
			}
			raw = schema.ThinkingLevelRaw(value)
		}
		if err := schema.ValidateThinkingLevelEvidence(entry.Role, level, raw); err != nil {
			return fmt.Errorf("thinking-level entry validation failed\n  what: indexed entry %d (role %q) carries thinking-level evidence level=%q raw=%q that the contract refuses\n  why: %v\n  where: transcript.ValidateThinkingLevelEntries\n  when: after storage read and before entry folding or payload emission\n  meaning: no local, exported, or remote payload can be emitted without silently normalizing or dropping evidence\n  fix: keep thinking_level/thinking_level_raw only on assistant entries; omit unknown canonical levels, preserve exact valid raw spellings (at most 128 UTF-8 bytes without edge whitespace), repair or re-index the source entry, and retry", entry.EntryIndex, entry.Role, level, raw, err)
		}
	}
	return nil
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
