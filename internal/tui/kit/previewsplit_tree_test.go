package kit_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

// treeSplit mounts a PreviewSplit whose LEFT pane is a tree over roots, loads
// the forest through the split (the path a mounted flow uses), and returns it.
func treeSplit(t *testing.T, roots []*kit.TreeNode, src kit.ContentSource, width, height int) kit.PreviewSplit {
	t.Helper()
	tree := kit.NewTree(darkTheme(), staticSource{roots: roots})
	ps := kit.NewPreviewSplit(darkTheme(), kit.NewTreeLeftPane(&tree), src)
	ps.SetSize(width, height)
	ps.Focus()

	// The tree's own load result reaches it THROUGH the split, which is what
	// makes an asynchronous left pane usable at all.
	loaded, loadCmd := tree.Load()
	tree = loaded
	for _, msg := range collectMsgs(loadCmd) {
		ps, _ = ps.Update(msg)
		for _, follow := range collectMsgs(nil) {
			ps, _ = ps.Update(follow)
		}
	}
	// Drain the preview load the forest arrival triggered.
	for _, msg := range collectMsgs(ps.Load()) {
		ps, _ = ps.Update(msg)
	}
	return ps
}

// splitRoots is the small forest the tree-left-pane tests preview over.
func splitRoots() []*kit.TreeNode {
	return []*kit.TreeNode{{
		ID:    "project",
		Label: "acme/tool",
		Children: []*kit.TreeNode{
			{ID: "sess-1", Label: "first session", State: kit.Unchecked},
			{ID: "sess-2", Label: "second session", State: kit.Unchecked},
		},
	}}
}

// idEchoSource echoes the id it was asked for, so a test can prove WHICH row
// the preview is bound to.
type idEchoSource struct{}

func (idEchoSource) Content(id string, _ int) (string, error) { return "body of " + id, nil }

// TestPreviewSplit_TreeLeftPaneLoadsThroughTheSplit proves an asynchronous left
// pane works: the tree's load result reaches it through the split, and the
// preview binds to the row the forest opens on.
func TestPreviewSplit_TreeLeftPaneLoadsThroughTheSplit(t *testing.T) {
	ps := treeSplit(t, splitRoots(), idEchoSource{}, 60, 8)

	id, ok := ps.HighlightedID()
	if !ok {
		t.Fatal("no row highlighted after the forest loaded through the split")
	}
	if id != "project" {
		t.Fatalf("highlighted id = %q, want the first row of the loaded forest", id)
	}
	view := stripANSI(ps.View())
	if !strings.Contains(view, "acme/tool") {
		t.Errorf("tree pane did not render the loaded forest; view:\n%s", view)
	}
	if !strings.Contains(view, "body of project") {
		t.Errorf("preview pane is not bound to the highlighted row; view:\n%s", view)
	}
}

// TestPreviewSplit_TreeLeftPaneFollowsTheCursor proves moving the tree cursor
// loads the preview of the newly-highlighted row, so the pane always describes
// the row the user is on.
func TestPreviewSplit_TreeLeftPaneFollowsTheCursor(t *testing.T) {
	ps := treeSplit(t, splitRoots(), idEchoSource{}, 60, 8)

	next, cmd := ps.Update(keyPress(t, "j"))
	ps = next
	for _, msg := range collectMsgs(cmd) {
		ps, _ = ps.Update(msg)
	}

	if id, _ := ps.HighlightedID(); id != "sess-1" {
		t.Fatalf("highlighted id after moving down = %q, want sess-1", id)
	}
	if view := stripANSI(ps.View()); !strings.Contains(view, "body of sess-1") {
		t.Errorf("preview did not follow the cursor; view:\n%s", view)
	}
}

// TestPreviewSplit_WrapsBodyToTheCurrentPaneWidth proves the pane owns wrapping:
// a body longer than the preview column is folded to the pane's CURRENT width at
// render time, so a source that produced its text before the pane was ever sized
// still reads as a paragraph rather than one truncated line.
func TestPreviewSplit_WrapsBodyToTheCurrentPaneWidth(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 60))
	ps := treeSplit(t, splitRoots(), fixedContentSource{content: long}, 60, 8)

	view := ps.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 8 {
		t.Fatalf("split rendered %d lines at height 8", len(lines))
	}
	// Several preview rows must carry text: an unwrapped body would fill one.
	filled := 0
	for _, line := range lines {
		if strings.Contains(stripANSI(line), "word") {
			filled++
		}
	}
	if filled < 4 {
		t.Fatalf("a long body was not wrapped down the pane; only %d rows carry text:\n%s", filled, stripANSI(view))
	}
}
