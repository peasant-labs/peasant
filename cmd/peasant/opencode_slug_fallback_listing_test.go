package main

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// TestOpenCodeSlugFallbackReachesKickstartListing proves that a session whose
// row carries no title shows its harness slug as the display name in the
// kickstart listing, while a titled session keeps its title. Clearing the slug
// fallback leaves the titleless session with an empty display name.
func TestOpenCodeSlugFallbackReachesKickstartListing(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "extended-attribution")
	root := filepath.Dir(materialized.Path)
	git := newMountedOpenCodeGitResolver()
	_, listings, _ := ftueDiscoverWith(t.Context(), mountedOpenCodeConfig(t, root), &ingest.OSFileSystem{}, git, nil, nil, nil)
	titles := make(map[string]string, len(listings))
	for _, listing := range listings {
		titles[listing.SessionID] = listing.Title
	}
	if got := titles["ses_3cd91f52effeXd3QAJ54jOyzX1"]; got != "legacy winner attribution" {
		t.Errorf("titled session display name = %q, want its title", got)
	}
	if got := titles["ses_3cd91f52effeXd3QAJ54jOyzX2"]; got != "silent-pixel" {
		t.Errorf("titleless session display name = %q, want the slug fallback %q", got, "silent-pixel")
	}
}
