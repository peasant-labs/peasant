package settings

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// receiptPaintWidth is the render width of the receipt paint check. It is
// wider than every line the fixture flow produces, so the check reads real pad
// cells rather than truncated text.
const receiptPaintWidth = 70

// TestReceipt_PaintsEveryCell proves the final review step composes one block:
// every row is the same width and every cell carries the page background.
// Before the receipt used the shared panel, each row painted only as many
// cells as its own text, so the block showed a ragged right edge.
func TestReceipt_PaintsEveryCell(t *testing.T) {
	doc := loadStepTabsDoc(t)
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			th := theme.New(mode)
			f := newFullStepFlow(t, th, doc, receiptPaintWidth, 24)
			for f.Step() < len(doc.Titles) {
				f = send(f, "tab")
			}
			if !f.OnReceipt() {
				t.Fatalf("the flow is on step %d, want the receipt step", f.Step())
			}
			lines := strings.Split(f.renderReceipt(th.Styles(), receiptPaintWidth), "\n")
			if len(lines) == 0 {
				t.Fatal("the receipt rendered no lines")
			}
			for i, line := range lines {
				if got := lipgloss.Width(line); got != receiptPaintWidth {
					t.Errorf("receipt line %d is %d cells, want exactly %d: %q", i, got, receiptPaintWidth, line)
				}
				if got := testutil.UnpaintedCells(line); got != 0 {
					t.Errorf("receipt line %d leaves %d cell(s) unpainted: %q", i, got, line)
				}
			}
		})
	}
}
