package kit

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

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

// FitLine truncates then pads one PLAIN-TEXT line to exactly width terminal
// cells and applies style exactly once, after fitting. Applying the style last
// makes background-bearing rows paint through their final cell instead of
// stopping at the final glyph. Measurement and truncation are ANSI-aware and
// grapheme-aware, but callers must not pass an already-rendered ANSI string:
// composed output needs fitRenderedLine so nested style resets can restore the
// parent's background correctly.
func FitLine(style lipgloss.Style, s string, width int) string {
	if width <= 0 {
		return ""
	}
	clipped := s
	if ansi.StringWidth(clipped) > width {
		clipped = ansi.Truncate(clipped, width, "…")
	}
	return style.Render(padLine(clipped, width))
}

// fitLine preserves the private helper used throughout the component
// implementations while exposing FitLine to presentation packages that need
// the same fixed-row contract.
func fitLine(style lipgloss.Style, s string, width int) string {
	return FitLine(style, s, width)
}

// fitRenderedLine fits a line that already contains ANSI styling inside a
// parent style. Unlike FitLine, this function never applies a style around
// already-rendered bytes: ANSI reset sequences are not nestable and would
// otherwise cancel the parent's background before trailing padding. It starts
// in the parent style, restores that style after each reset, and pads before
// the final reset so every otherwise-transparent cell inherits the parent.
func fitRenderedLine(style lipgloss.Style, rendered string, width int) string {
	if width <= 0 {
		return ""
	}
	clipped := rendered
	if ansi.StringWidth(clipped) > width {
		clipped = ansi.Truncate(clipped, width, "…")
	}
	padding := spaces(width - ansi.StringWidth(clipped))
	if clipped == "" {
		return FitLine(style, "", width)
	}

	styledProbe := style.Render("x")
	prefixAt := strings.Index(styledProbe, "x")
	if prefixAt < 0 {
		return clipped + padding
	}
	prefix := styledProbe[:prefixAt]
	suffix := styledProbe[prefixAt+1:]
	if prefix == "" || suffix == "" {
		return clipped + padding
	}
	for strings.HasSuffix(clipped, suffix) {
		clipped = strings.TrimSuffix(clipped, suffix)
	}
	restore := suffix + prefix
	clipped = strings.ReplaceAll(clipped, "\x1b[m", restore)
	clipped = strings.ReplaceAll(clipped, "\x1b[0m", restore)
	return prefix + clipped + padding + suffix
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
