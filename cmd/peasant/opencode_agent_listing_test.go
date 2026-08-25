package main

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// TestOpenCodeAgentLabelReachesKickstartListing proves that a SQLite-discovered
// OpenCode session carries its agent label into the kickstart selection listing,
// so a subagent session such as "reviewer-openai" shows its agent the way a
// Claude teammate does. Clearing the listing's Agent assignment fails this case.
func TestOpenCodeAgentLabelReachesKickstartListing(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "extended-attribution")
	root := filepath.Dir(materialized.Path)
	git := newMountedOpenCodeGitResolver()
	_, listings, _ := ftueDiscoverWith(t.Context(), mountedOpenCodeConfig(t, root), &ingest.OSFileSystem{}, git, nil, nil, nil)
	if len(listings) != 2 {
		t.Fatalf("kickstart listed %d OpenCode sessions, want 2 from the extended attribution database", len(listings))
	}
	agents := make(map[string]string, len(listings))
	for _, listing := range listings {
		agents[listing.SessionID] = listing.Agent
	}
	if got := agents["ses_3cd91f52effeXd3QAJ54jOyzX1"]; got != "general" {
		t.Errorf("primary session listing agent = %q, want %q", got, "general")
	}
	if got := agents["ses_3cd91f52effeXd3QAJ54jOyzX2"]; got != "reviewer-openai" {
		t.Errorf("subagent session listing agent = %q, want %q", got, "reviewer-openai")
	}
}
