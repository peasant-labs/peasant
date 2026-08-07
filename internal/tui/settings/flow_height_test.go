package settings

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// TestFlow_BodyFitsHeightOnTreeStep proves the step body never renders more
// lines than the height it is given, on a step whose field is the scrolling
// tree. The tree pads to the height it is sized to, so if the body handed it the
// full height (ignoring the tab strip, blank separator, and label it draws
// above), the body would overflow and the frame would clip the tree's bottom
// rows — making the cursor invisible when it reaches the last row.
func TestFlow_BodyFitsHeightOnTreeStep(t *testing.T) {
	path, loaded := writeConfigFile(t)
	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	src := scannerfix.NewFixtureTreeSource("standard")
	f := NewFlow(theme.New(theme.ModeDark), treeRegistry(src), d)
	f.SetSize(80, 24)
	f = drainInit(f)

	for _, height := range []int{6, 8, 12, 20} {
		body := f.body(78, height)
		if got := strings.Count(body, "\n") + 1; got > height {
			t.Fatalf("body(78, %d) produced %d lines, exceeds the height; the tree is oversized and its bottom rows are clipped", height, got)
		}
	}
}
