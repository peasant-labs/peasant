package kit_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

func loadStaticTree(t *testing.T, roots []*kit.TreeNode, width, height int) kit.Tree {
	t.Helper()
	tr := kit.NewTree(darkTheme(), staticSource{roots: roots})
	tr, cmd := tr.Load()
	if msg := runCmd(cmd); msg != nil {
		tr, _ = tr.Update(msg)
	}
	tr.SetSize(width, height)
	return tr
}

// TestTree_MultilineLabelRendersOneLinePerRow proves a label that contains
// newlines (e.g. a multi-line first-turn title) renders as exactly one display
// row: the tree pads to its height, so if the label were not flattened the row
// would span several lines and the view would exceed the height — which is what
// broke the row background and the scroll math.
func TestTree_MultilineLabelRendersOneLinePerRow(t *testing.T) {
	roots := []*kit.TreeNode{{
		ID:    "p",
		Label: "project",
		Children: []*kit.TreeNode{
			{ID: "s1", Label: "first line\nsecond line\nthird", State: kit.Unchecked},
		},
	}}
	const height = 6
	view := loadStaticTree(t, roots, 60, height).View()
	if got := strings.Count(view, "\n") + 1; got != height {
		t.Fatalf("tree rendered %d lines at height %d; a multi-line label was not flattened to one row", got, height)
	}
	if !strings.Contains(view, "first line second line third") {
		t.Fatalf("flattened label not found in view:\n%s", view)
	}
}

// TestTree_ScrollsToKeepDeepCursorVisible proves the window scrolls so a cursor
// driven to the bottom of a forest taller than the viewport stays on-screen and
// the top rows scroll off — the fix for selecting rows below the screen edge.
func TestTree_ScrollsToKeepDeepCursorVisible(t *testing.T) {
	children := make([]*kit.TreeNode, 0, 20)
	for i := range 20 {
		children = append(children, &kit.TreeNode{
			ID:    fmt.Sprintf("s%02d", i),
			Label: fmt.Sprintf("sess-%02d", i),
			State: kit.Unchecked,
		})
	}
	roots := []*kit.TreeNode{{ID: "ROOTID", Label: "ROOTLABEL", Children: children}}

	tr := loadStaticTree(t, roots, 60, 8)
	tr, _ = tr.Update(keyPress(t, "G")) // go to bottom
	view := tr.View()

	if !strings.Contains(view, "sess-19") {
		t.Fatalf("bottom cursor row not visible after scrolling to the end; view:\n%s", view)
	}
	if strings.Contains(view, "ROOTLABEL") {
		t.Fatalf("top row should have scrolled off-screen once the cursor reached the bottom; view:\n%s", view)
	}
}
