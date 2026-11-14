package main

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

func TestSessionDetailShowsEachClaudeReplyOnce(t *testing.T) {
	t.Parallel()
	const fixtureRecords = 4
	s := harvestFixtureDetail(t, string(defaults.HarnessClaudeCode), "claude-fixture", ".", "6a46bcd8-b365-4608-be7d-5601e2fdd805")
	if len(s.Turns) != fixtureRecords {
		t.Errorf("detail turns = %d, want the fixture's %d records", len(s.Turns), fixtureRecords)
	}
	if s.TurnCount != len(s.Turns) {
		t.Errorf("turnCount = %d, but the detail serves %d turns", s.TurnCount, len(s.Turns))
	}
	byIndex := make(map[int]int, len(s.Turns))
	for i, turn := range s.Turns {
		byIndex[turn.Index] = i
	}
	for i, turn := range s.Turns {
		if turn.Timestamp.IsZero() {
			t.Errorf("turn %d (index %d, %s): zero timestamp", i, turn.Index, turn.Role)
		}
		if turn.ParentIndex == nil {
			continue
		}
		if p, ok := byIndex[*turn.ParentIndex]; ok && s.Turns[p].Role == turn.Role && s.Turns[p].Content == turn.Content {
			t.Errorf("turn %d (index %d) repeats its parent %d's %s content", i, turn.Index, *turn.ParentIndex, turn.Role)
		}
	}
}
