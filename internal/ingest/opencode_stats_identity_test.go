package ingest_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestOpenCodeMetadataFillsRowStatsIdentityAndSlugFallback proves that at
// metadata time the session row aggregates fill the token totals and the harness
// version without folding entries, and that the harness slug is the display-name
// fallback when the session row carries no title. Each assertion pins a distinct
// source: tokens and version come from the session row, and the slug fallback
// lands on the discovered session and its metadata.
func TestOpenCodeMetadataFillsRowStatsIdentityAndSlugFallback(t *testing.T) {
	t.Parallel()
	materialized := testfixture.MaterializeByName(t, "extended-attribution")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("run production OpenCode discovery: %v", err)
	}
	var subagent ingest.DiscoveredSession
	found := false
	for _, session := range discovered {
		if string(session.SessionID) == "ses_3cd91f52effeXd3QAJ54jOyzX2" {
			subagent = session
			found = true
		}
	}
	if !found {
		t.Fatal("discovery did not return the titleless subagent session")
	}
	// The slug fallback lands on the discovered session: its row has no title.
	if subagent.Title != "silent-pixel" {
		t.Fatalf("subagent display name = %q, want the slug fallback %q", subagent.Title, "silent-pixel")
	}
	metadata, err := adapter.ExtractMetadata(t.Context(), subagent)
	if err != nil {
		t.Fatalf("extract metadata for the subagent session: %v", err)
	}
	if metadata.Stats.TokensIn != 1200 || metadata.Stats.TokensOut != 800 {
		t.Fatalf("metadata token totals = in %d out %d, want the session-row aggregates 1200/800", metadata.Stats.TokensIn, metadata.Stats.TokensOut)
	}
	if metadata.Version != "1.1.40" {
		t.Fatalf("metadata version = %q, want the session-row version %q", metadata.Version, "1.1.40")
	}
}
