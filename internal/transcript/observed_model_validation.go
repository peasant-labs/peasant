package transcript

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// ValidateObservedModelEvidence enforces the producer-only role condition that
// generated shape validators cannot express.
func ValidateObservedModelEvidence(turn ingest.Turn) error {
	if turn.ObservedModel == "" {
		return nil
	}
	if err := schema.ValidateObservedModelEvidence(turn.Role, schema.ObservedModelID(turn.ObservedModel)); err != nil {
		return fmt.Errorf("observed-model producer validation failed\n  what: turn %d carries invalid source evidence %q\n  why: the identifier does not satisfy the released observedModel contract: %v\n  where: transcript.ValidateObservedModelEvidence\n  when: during SessionDetailPayload construction, before local, exported, or remote emission\n  meaning: no payload can be emitted because changing these bytes would falsify the recorded observation\n  fix: omit absent evidence or repair the source identifier, re-index the session, and retry", turn.Index, turn.ObservedModel, err)
	}
	return nil
}

// ValidateObservedModelEntries enforces exact value and assistant attribution at
// the indexed-entry trust boundary. It reports whether valid enriched evidence
// exists so remote emission can negotiate the corresponding capability.
func ValidateObservedModelEntries(entries []schema.SessionEntry) (bool, error) {
	hasEvidence := false
	for _, entry := range entries {
		if entry.Extra == nil {
			continue
		}
		var extra map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*entry.Extra), &extra); err != nil {
			continue
		}
		raw, present := extra["model_id"]
		if !present {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return false, fmt.Errorf("observed-model entry validation failed\n  what: indexed entry %d carries a non-string model_id value\n  why: observedModel source evidence must be an exact string: %v\n  where: transcript.ValidateObservedModelEntries\n  when: after storage read and before entry folding or payload emission\n  meaning: no local, exported, or remote payload can be emitted without silently changing or dropping evidence\n  fix: repair or re-index the source entry so model_id is an exact valid string, then retry", entry.EntryIndex, err)
		}
		observed := schema.ObservedModelID(value)
		if err := schema.ValidateObservedModelEvidence(entry.Role, observed); err != nil {
			return false, fmt.Errorf("observed-model entry validation failed\n  what: indexed entry %d carries invalid model_id evidence %q\n  why: the identifier does not satisfy the released observedModel contract: %v\n  where: transcript.ValidateObservedModelEntries\n  when: after storage read and before entry folding or payload emission\n  meaning: no local, exported, or remote payload can be emitted without silently normalizing or dropping evidence\n  fix: repair or re-index the source entry with exact valid model evidence, then retry", entry.EntryIndex, value, err)
		}
		hasEvidence = true
	}
	return hasEvidence, nil
}

// EntriesToTurnsValidated is the canonical storage-to-fold producer boundary.
func EntriesToTurnsValidated(entries []schema.SessionEntry) ([]ingest.Turn, error) {
	if _, err := ValidateObservedModelEntries(entries); err != nil {
		return nil, err
	}
	return EntriesToTurns(entries), nil
}

func nonAssistantObservedModelError(index int, role schema.Role, value, where, when string) error {
	return fmt.Errorf("observed-model producer validation failed\n  what: turn or entry %d with role %q carries observedModel %q\n  why: model evidence attributes generated output, but this is not assistant or subagent assistant output\n  where: %s\n  when: %s\n  meaning: no local, exported, or remote payload can be emitted because silently stripping the evidence would conceal invalid attribution\n  fix: remove the observation from the non-assistant source entry, re-index the session, and retry", index, role, value, where, when)
}

func validateSessionObservedModelEvidence(session *ingest.Session) error {
	for _, turn := range session.Turns {
		if err := ValidateObservedModelEvidence(turn); err != nil {
			return err
		}
	}
	return nil
}
