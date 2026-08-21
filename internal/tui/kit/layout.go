package kit

import (
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// FitCell truncates then pads one PLAIN-TEXT cell to exactly width terminal
// cells and returns it UNSTYLED. It is the shared column helper for a surface
// that composes several cells into one row and styles the finished row once.
// A surface must not repeat this padding by hand; the layout gate
// (internal/tui/gates/layout.go) fails a hand-rolled space pad.
func FitCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return padLine(truncateLine(s, width), width)
}

// Indent returns n spaces for a plain-text indent. A surface that needs a
// column offset calls this instead of repeating a space run itself, which
// keeps the layout gate's "no hand-rolled padding" rule checkable.
func Indent(n int) string { return spaces(n) }

// Center places content in the middle of a width x height region and paints
// every cell around it with background, so a centered dialog never leaves the
// surrounding cells on the terminal's own background. A non-positive region
// returns content unchanged. This is the ONE centering path a surface uses;
// the layout gate (internal/tui/gates/layout.go) fails a direct
// lipgloss.Place call outside the kit.
func Center(content string, width, height int, background lipgloss.Style) string {
	if width <= 0 || height <= 0 {
		return content
	}
	return lipgloss.Place(
		width, height,
		lipgloss.Center, lipgloss.Center,
		content,
		lipgloss.WithWhitespaceStyle(background),
	)
}

// CenterOnTheme centers content over the theme's Base background.
func CenterOnTheme(t theme.Theme, content string, width, height int) string {
	return Center(content, width, height, t.Styles().Base)
}
