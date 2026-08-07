package settings

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
)

// TestApplyExistingSelection_MatcherParity proves the pre-population uses the
// SAME canonical matcher (config.CompileSelectionMatcher / MatchDiscovery)
// ingest, push, discovery, and prune use: every session leaf's state after
// pre-population agrees, node-for-node, with what the matcher decides — and the
// derived selection round-trips back to the saved project pick.
func TestApplyExistingSelection_MatcherParity(t *testing.T) {
	roots, err := scannerfix.Load("standard")
	if err != nil {
		t.Fatalf("load standard fixture: %v", err)
	}

	// A saved selection that admits the whole peasant project under the first
	// provider in the scanned forest. The harness name is taken from the
	// fixture node itself (not a bare literal), so the selection stays in step
	// with whatever provider the scanner fixture declares.
	claudeHarness := roots[0].ID
	sel := config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			claudeHarness: {
				Projects: []config.ProjectSelection{
					{GitRemote: "git@github.com:peasant-labs/peasant.git"},
				},
			},
		},
	}

	ApplyExistingSelection(roots, sel)

	// Every session leaf's pre-checked state must equal the matcher's verdict.
	matcher := config.CompileSelectionMatcher(sel)
	var checkedSessions int
	for _, provider := range roots {
		harness := ingest.Harness(provider.ID)
		for _, remote := range provider.Children {
			gitRemote := gitRemoteOf(remote)
			for _, worktree := range remote.Children {
				branch := branchOf(worktree)
				for _, session := range worktree.Children {
					want := matcher.MatchDiscovery(harness, gitRemote, remote.Label, branch,
						ingest.SessionID(session.ID), sel.AutoIngestNewBranches)
					got := session.State
					switch want {
					case ingest.BranchMatchYes:
						if got != kit.Checked {
							t.Errorf("session %s: state %v, want Checked", session.ID, got)
						}
						checkedSessions++
					case ingest.BranchMatchWithheldConflict:
						if got != kit.Conflict {
							t.Errorf("session %s: state %v, want Conflict", session.ID, got)
						}
					default:
						if got != kit.Unchecked {
							t.Errorf("session %s: state %v, want Unchecked", session.ID, got)
						}
					}
				}
			}
		}
	}
	if checkedSessions == 0 {
		t.Fatalf("no sessions matched; pre-population did nothing")
	}

	// The whole peasant project became Checked, so the derived selection is the
	// project pick again (Branches nil).
	got := FromTreeNodes(roots)
	if got.Mode != config.SelectionModeSelected {
		t.Fatalf("mode = %q", got.Mode)
	}
	claude := got.Harnesses[claudeHarness]
	if len(claude.Projects) != 1 || claude.Projects[0].GitRemote != "git@github.com:peasant-labs/peasant.git" {
		t.Fatalf("derived project pick = %#v", claude.Projects)
	}
	if len(claude.Projects[0].Branches) != 0 {
		t.Fatalf("expected whole-project pick (nil branches), got %#v", claude.Projects[0].Branches)
	}
}

// TestApplyExistingSelection_All pre-checks the whole forest for mode all.
func TestApplyExistingSelection_All(t *testing.T) {
	roots, err := scannerfix.Load("standard")
	if err != nil {
		t.Fatalf("load standard fixture: %v", err)
	}
	ApplyExistingSelection(roots, config.SelectionConfig{Mode: config.SelectionModeAll})
	for _, r := range roots {
		if r.State != kit.Checked {
			t.Fatalf("provider %s not checked under mode all: %v", r.ID, r.State)
		}
	}
	if got := FromTreeNodes(roots); got.Mode != config.SelectionModeAll {
		t.Fatalf("derived mode = %q, want all", got.Mode)
	}
}
