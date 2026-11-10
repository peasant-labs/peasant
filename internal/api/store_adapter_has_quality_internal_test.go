package api

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
)

// TestHasQualityIgnoresTheConversationTurnCount pins the white-box rule: the
// conversation turn count is producer state carried on every indexed session,
// not a computed quality signal. A row with only a count reports no quality;
// a row with a computed signal reports quality and still carries the count.
func TestHasQualityIgnoresTheConversationTurnCount(t *testing.T) {
	t.Parallel()
	countOnly := &store.SessionRow{TurnCount: 7}
	if hasQuality(countOnly) {
		t.Fatal("a conversation turn count alone reported computed quality")
	}
	if q := rowToQualityMetrics(countOnly); q != nil {
		t.Fatalf("a conversation turn count alone produced quality metrics: %+v", q)
	}

	computed := &store.SessionRow{TurnCount: 7, TokensTotal: 1200}
	if !hasQuality(computed) {
		t.Fatal("a computed token total did not report quality")
	}
	q := rowToQualityMetrics(computed)
	if q == nil || q.TurnCount == nil || *q.TurnCount != 7 {
		t.Fatalf("quality metrics = %+v, want turn count 7 carried beside the computed signal", q)
	}
}
