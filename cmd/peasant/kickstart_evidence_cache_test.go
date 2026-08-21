package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestKickstartRescan_ReusesMinedTranscriptEvidence drives the real discovery
// core twice over an unchanged transcript corpus. Claude discovery mines the
// teammate links from the transcripts, and that mining used to read every file
// again on every scan. The second scan must return the same listings and must
// read no transcript at all.
func TestKickstartRescan_ReusesMinedTranscriptEvidence(t *testing.T) {
	t.Parallel()
	fixtures := loadRescanFixtures(t)
	ingestedAt := time.Now().Add(-rescanIngestedAgo)

	fs := testutil.NewCountingFS(testutil.NewMemFS())
	writeRescanSources(t, fs.MemFS, fixtures, ingestedAt)

	database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"))
	if err != nil {
		t.Fatalf("open the local store: %v", err)
	}
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			t.Errorf("close the local store: %v", closeErr)
		}
	}()

	git := newDirCountingGitResolver(fixtures.ResolvedRemote, fixtures.ResolvedBranch)

	_, first := ftueDiscoverWith(t.Context(), rescanConfig(t), fs, git, nil, database, nil)
	if len(first) == 0 {
		t.Fatal("the first scan listed no session, so the cache measurement proves nothing")
	}
	if fs.TotalReads() == 0 {
		t.Fatal("the first scan read no transcript, so the cache measurement proves nothing")
	}

	fs.ResetCounts()
	_, second := ftueDiscoverWith(t.Context(), rescanConfig(t), fs, git, nil, database, nil)

	if got := fs.TotalReads(); got != 0 {
		t.Errorf("the second scan read transcripts %d times, want 0 for an unchanged corpus", got)
	}
	if len(second) != len(first) {
		t.Fatalf("the second scan listed %d sessions, want %d", len(second), len(first))
	}
	firstByID := listingByID(first)
	for id, listing := range listingByID(second) {
		before, ok := firstByID[id]
		if !ok {
			t.Errorf("the second scan listed unknown session %s", id)
			continue
		}
		if listing.Title != before.Title {
			t.Errorf("session %s title = %q, want %q", id, listing.Title, before.Title)
		}
		if listing.Branch != before.Branch {
			t.Errorf("session %s branch = %q, want %q", id, listing.Branch, before.Branch)
		}
		if listing.WorkingDir != before.WorkingDir {
			t.Errorf("session %s working directory = %q, want %q", id, listing.WorkingDir, before.WorkingDir)
		}
		if len(listing.SubagentIDs) != len(before.SubagentIDs) {
			t.Errorf("session %s has %d linked teammates, want %d", id, len(listing.SubagentIDs), len(before.SubagentIDs))
		}
	}
}
