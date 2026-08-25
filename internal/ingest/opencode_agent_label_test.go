package ingest_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestOpenCodeDiscoveryCarriesAgentLabel proves that discovery carries the agent
// label from the session row onto every SQLite-discovered OpenCode session, so a
// subagent session such as "reviewer-openai" surfaces its agent through the
// kickstart listing. Dropping the agent assignment in discoverSQLiteCandidate
// leaves the field empty and fails this case.
func TestOpenCodeDiscoveryCarriesAgentLabel(t *testing.T) {
	t.Parallel()
	byID := discoverOpenCodeSessionsByID(t, "extended-attribution")

	primary, ok := byID["ses_3cd91f52effeXd3QAJ54jOyzX1"]
	if !ok {
		t.Fatalf("discovered sessions = %v, want the primary session", keysOf(byID))
	}
	if primary.Agent != "general" {
		t.Fatalf("primary session agent = %q, want the session-row label %q", primary.Agent, "general")
	}

	subagent, ok := byID["ses_3cd91f52effeXd3QAJ54jOyzX2"]
	if !ok {
		t.Fatalf("discovered sessions = %v, want the subagent session", keysOf(byID))
	}
	if subagent.Agent != "reviewer-openai" {
		t.Fatalf("subagent session agent = %q, want the session-row label %q", subagent.Agent, "reviewer-openai")
	}
}

func keysOf(byID map[string]ingest.DiscoveredSession) []string {
	keys := make([]string, 0, len(byID))
	for id := range byID {
		keys = append(keys, id)
	}
	return keys
}
