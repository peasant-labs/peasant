package kickstart_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// selectionStepView drives the REAL mounted program - the same scanner source,
// registry, and preview the command wires - to the selection step and returns
// what it renders. The parent/child fixture supplies the listing, the store is
// stubbed by the fixture's ingested set, and the preview body comes from the
// same seam the mount fills with the local store.
func selectionStepView(t *testing.T, width, height int) string {
	t.Helper()
	doc := loadNestedListings(t)
	stored := map[string]string{"sess-p2": "please refactor the ingest pipeline"}
	preview := kickstart.NewListingPreview(theme.New(theme.ModeDark), doc.Listings, turnsFromPrompts(stored))

	p, _ := newTestProgram(t, kickstart.ProgramDeps{
		Source:  kickstart.NewScannerTreeSource(doc.Listings, kickstart.WithIngestedSessionIDs(doc.Ingested)),
		Preview: preview,
	})
	p.SetSize(width, height)
	p = declineOAuth(t, p)

	// Run the flow's startup commands (the tree scan and the first preview load)
	// and feed every message back, exactly as the runtime would.
	for _, msg := range collectMsgs(p.Init()) {
		var cmd tea.Cmd
		p, cmd = p.Update(msg)
		for _, follow := range collectMsgs(cmd) {
			p, _ = p.Update(follow)
		}
	}
	if p.Phase() != kickstart.PhaseFlow {
		t.Fatalf("phase = %s, want flow", p.Phase())
	}
	return p.View()
}

// TestSelectionStep_RendersCountsSplitFacetAndPreview is the mounted check that
// the selection step carries all four of its answers at once: a parent session
// summarised by a child count, an already-imported marker, the harness facet
// gutter named in this flow's lowercase chrome, and the preview body of the
// highlighted row beside the tree.
func TestSelectionStep_RendersCountsSplitFacetAndPreview(t *testing.T) {
	t.Parallel()
	view := selectionStepView(t, 120, 30)

	for _, want := range []string{
		// The parent summarises its subagent chain instead of nesting it.
		"+ 2 child sessions",
		// The already-imported session is marked as such.
		"already imported",
		// The facet gutter, named in lowercase chrome.
		"harness",
		"claude code",
		// The preview names the highlighted row; the cursor opens on the project.
		"select a session to preview it",
	} {
		if !strings.Contains(stripRender(view), want) {
			t.Errorf("selection step must show %q; view:\n%s", want, view)
		}
	}
	// A subagent session is summarised, never a row of its own.
	for _, missing := range []string{"child subagent", "grandchild subagent"} {
		if strings.Contains(view, missing) {
			t.Errorf("subagent %q must not be a row of its own; view:\n%s", missing, view)
		}
	}
}

// TestSelectionStep_PreviewFollowsTheCursor proves the preview is live: moving
// the cursor onto a session row loads and renders THAT session's body beside the
// tree, so the pane always describes the row the user is on.
func TestSelectionStep_PreviewFollowsTheCursor(t *testing.T) {
	t.Parallel()
	doc := loadNestedListings(t)
	stored := map[string]string{"sess-p2": "please refactor the ingest pipeline"}
	preview := kickstart.NewListingPreview(theme.New(theme.ModeDark), doc.Listings, turnsFromPrompts(stored))

	p, _ := newTestProgram(t, kickstart.ProgramDeps{
		Source:  kickstart.NewScannerTreeSource(doc.Listings, kickstart.WithIngestedSessionIDs(doc.Ingested)),
		Preview: preview,
	})
	p.SetSize(120, 30)
	p = declineOAuth(t, p)
	for _, msg := range collectMsgs(p.Init()) {
		p, _ = p.Update(msg)
	}

	// Rows are project, branch, then the two parent sessions: step onto the
	// already-imported one and drain the preview load it triggers.
	for i := 0; i < 3; i++ {
		var cmd tea.Cmd
		p, cmd = p.Update(tea.KeyPressMsg{Code: 'j'})
		for _, msg := range collectMsgs(cmd) {
			p, _ = p.Update(msg)
		}
	}

	view := p.View()
	// The pane renders the recorded message as markdown, so the assertion reads
	// the VISIBLE characters rather than the styling glamour opens at each wrap.
	if !strings.Contains(stripRender(view), "please refactor the ingest pipeline") {
		t.Fatalf("preview did not follow the cursor onto the session row; view:\n%s", view)
	}
}
