package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
)

// TestHarvestClaimsNewSessionsWhenSignedIn proves the CLI wiring reaches the
// production store: a signed-in harvest stores the transcript and appends one
// automatic claim covering it.
func TestHarvestClaimsNewSessionsWhenSignedIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	saveUserCommandCredentials(t, dir)

	sessions, _ := LoadHarvestIndexSelectionFixtures(t)
	session := sessions[0]
	project := filepath.Join(sourceDir, "-fixture-project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatalf("prepare the project directory: %v", err)
	}
	transcript := filepath.Join(project, string(session.ID)+".jsonl")
	if err := os.WriteFile(transcript, []byte(session.Transcript), 0o600); err != nil {
		t.Fatalf("write the native transcript: %v", err)
	}
	settled := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(transcript, settled, settled); err != nil {
		t.Fatalf("settle the native transcript: %v", err)
	}

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-harness=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
	})
	if err != nil {
		t.Fatalf("harvest: %v\noutput: %s", err, output)
	}

	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open the harvest store: %v", err)
	}
	defer db.Close()
	page, err := db.OwnerClaims(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Claims) != 1 {
		t.Fatalf("claim entries = %+v, want one automatic entry", page.Claims)
	}
	if page.Claims[0].Source != store.ClaimSourceAutomatic || page.Claims[0].UsernameSnapshot != "alice" {
		t.Fatalf("claim = %+v, want the signed-in automatic claim", page.Claims[0])
	}
	counts, err := db.OwnershipCounts(t.Context(), &store.OwnershipAccount{VillageURL: "https://village.example/team", UserID: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	if counts.MineSessions == 0 || counts.UnclaimedSessions != 0 {
		t.Fatalf("ownership counts = %+v, want the harvested session claimed", counts)
	}
}
