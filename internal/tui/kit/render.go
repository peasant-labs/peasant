package kit

import (
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// truncateLine clips a single (already-styled or plain) line to at most
// width display cells using the ansi-aware lipgloss.Width, NEVER len().
// When the content is wider than width it is cut and, if there is room, an
// ellipsis rune is placed in the last cell so truncation is visible rather
// than silent. A non-positive width yields the empty string. It operates on
// the raw runes of a plain string; callers style the result after clipping
// so the ellipsis inherits the surrounding style.
func truncateLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	// Grow the prefix until adding one more rune would exceed the budget,
	// reserving one cell for the ellipsis when the string is being cut.
	budget := width
	ellipsis := "…"
	if width >= 1 {
		budget = width - 1
	}
	var out []rune
	for _, r := range runes {
		if lipgloss.Width(string(append(out, r))) > budget {
			break
		}
		out = append(out, r)
	}
	clipped := string(out) + ellipsis
	// Defensive: if the ellipsis pushed us over (wide runes), fall back to a
	// hard cut with no ellipsis rather than overflow.
	if lipgloss.Width(clipped) > width {
		return string(out)
	}
	return clipped
}

// padLine right-pads a line with spaces to exactly width display cells,
// measuring with lipgloss.Width so styled/wide content is accounted for. A
// line already at or over width is returned unchanged (callers truncate
// first when they need a hard cap).
func padLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + spaces(width-w)
}

// fitLine truncates then pads a plain line to exactly width cells, applying
// style to the whole cell run so the padding carries the component's
// background. This is the single primitive kit components use to emit one
// content row of a fixed width.
func fitLine(style lipgloss.Style, s string, width int) string {
	if width <= 0 {
		return ""
	}
	clipped := truncateLine(s, width)
	return style.Render(padLine(clipped, width))
}

// spaces returns n space characters (n<=0 yields "").
func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// cursorGlyph is the shared selection-cursor vocabulary hoisted from the
// legacy FTUE project picker: the row the cursor is on leads with "▸ ", every
// other row with two spaces, so rows stay column-aligned.
const (
	cursorGlyph   = "▸ "
	noCursorGlyph = "  "
	checkedBox    = "[✓]"
	uncheckedBox  = "[ ]"
	radioOn       = "(•)"
	radioOff      = "( )"
)

// styledCursor returns the cursor prefix for a row, styled from t: the
// selected/amber style on the active row, muted otherwise.
func styledCursor(styles theme.Styles, active bool) string {
	if active {
		return styles.Selected.Render(cursorGlyph)
	}
	return styles.Base.Render(noCursorGlyph)
}
