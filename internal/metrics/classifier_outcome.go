package metrics

import (
	"context"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// classifyOutcome is a ClassifierFunc that maps session outcome to the
// typeIDSessionOutcome annotation type (4-value domain: resolved, partial,
// failed, abandoned).
//
// Strategy:
//  1. Abandoned heuristic: zero tool calls AND at most one conversation turn
//     (checked first).
//  2. Otherwise: delegate to computeOutcome for the 3-value metric-derived result
//     (resolved / partial / failed) and map to the annotation domain.
//
// Returns nil if metrics is nil or computeOutcome returns no opinion.
func classifyOutcome(
	ctx context.Context,
	sessionID ingest.SessionID,
	entries []schema.SessionEntry,
	metrics *ingest.SessionMetrics,
) *ClassifierResult {
	if metrics == nil {
		return nil
	}

	// The abandoned heuristic is zero tool calls and at most one conversation
	// turn. The count comes from the same fold every reader uses, never from
	// the raw harness record count the retired metric seed carried. Checked
	// before computeOutcome because it takes precedence.
	toolCalls := 0
	if metrics.ToolCalls != nil {
		toolCalls = *metrics.ToolCalls
	}
	conversationTurnCount := ingest.ConversationTurnCount(ingest.EntriesToTurns(entries))
	if toolCalls == 0 && conversationTurnCount <= 1 {
		return &ClassifierResult{
			TypeID:     typeIDSessionOutcome,
			Value:      "abandoned",
			Confidence: 0.85,
			Reason:     "zero tool calls and at most one conversation turn indicate session was abandoned",
			Provenance: &schema.Provenance{
				Method:  "heuristic",
				Version: "1",
				Details: map[string]string{"rule": "tool_calls==0 AND conversation_turn_count<=1"},
			},
		}
	}

	// Map existing 3-value metric outcome to the 4-value annotation domain.
	partial := computeOutcome(ctx, sessionID, entries, metrics)
	if partial == nil || partial.Outcome == nil {
		return nil
	}

	var value string
	switch *partial.Outcome {
	case ingest.OutcomeResolved:
		value = "resolved"
	case ingest.OutcomePartial:
		value = "partial"
	case ingest.OutcomeFailed:
		value = "failed"
	default:
		return nil
	}

	return &ClassifierResult{
		TypeID: typeIDSessionOutcome,
		Value:  value,
		Provenance: &schema.Provenance{
			Method:  "heuristic",
			Version: "1",
		},
	}
}
