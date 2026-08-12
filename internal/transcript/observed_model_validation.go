package transcript

import (
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
	if _, err := ingest.NewObservedModelID(turn.ObservedModel.String()); err != nil {
		return fmt.Errorf("observed-model producer validation failed because turn %d carries invalid source evidence %q in transcript.ValidateObservedModelEvidence during SessionDetailPayload construction; no local, exported, or remote payload can be emitted, because changing these bytes would falsify the recorded observation; omit absent evidence or repair the source identifier and re-index the session, then retry: %w", turn.Index, turn.ObservedModel, err)
	}
	if turn.Role != schema.RoleAssistant {
		return fmt.Errorf("observed-model producer validation failed because turn %d with role %q carries observedModel %q in transcript.ValidateObservedModelEvidence during SessionDetailPayload construction; no local, exported, or remote payload can be emitted, because model evidence attributes generated output and this turn is not assistant or subagent assistant output; remove the observation from the non-assistant source entry, re-index the session, and retry", turn.Index, turn.Role, turn.ObservedModel)
	}
	return nil
}

func validateSessionObservedModelEvidence(session *ingest.Session) error {
	for _, turn := range session.Turns {
		if err := ValidateObservedModelEvidence(turn); err != nil {
			return err
		}
	}
	return nil
}
