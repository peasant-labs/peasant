package transcript

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

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
