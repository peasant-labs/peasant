package kit

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// FrameMinSize is the smallest outer size a Frame can draw its full chrome
// (top border, one content row, bottom border, and the two side borders)
// into. Below it Frame renders a truncation-safe fallback instead of
// panicking or drawing overlapping borders.
var FrameMinSize = Size{Width: 4, Height: 3}

// Frame is the single bordered container the whole kit draws inside, and the
// ONE place chrome-height accounting lives. A caller sets Frame's OUTER size
// (the region the terminal gives it) with [Frame.SetSize]; Frame subtracts
// its own border, title, and footer rows exactly once and exposes the
// remaining INNER content size via [Frame.InnerWidth]/[Frame.InnerHeight].
// Every child component is then sized from those inner values and never
// subtracts a border constant itself - this is what retires the per-surface
// -3/-4/-10/-12 fudge constants the audit catalogued.
//
// Frame renders whatever content string it is handed via [Frame.SetContent]
// (typically a child component's View()). It does not own the child; it owns
// the box around it.
type Frame struct {
	theme   theme.Theme
	title   string
	footer  string
	content string

	// outerWidth/outerHeight are the full region the frame occupies.
	outerWidth  int
	outerHeight int
}

// NewFrame builds a Frame over theme t. Optional title and footer are set via
// the returned value's setters; a Frame with neither still draws its border.
func NewFrame(t theme.Theme) Frame {
	return Frame{theme: t}
}

// WithTitle returns a copy of f with its top-border title set. An empty title
// draws a plain top border.
func (f Frame) WithTitle(title string) Frame {
	f.title = title
	return f
}

// WithFooter returns a copy of f with its footer line set. An empty footer
// means no footer row, and InnerHeight reclaims that row for content.
func (f Frame) WithFooter(footer string) Frame {
	f.footer = footer
	return f
}

// SetContent sets the body string Frame draws inside its border. The content
// is expected to already be sized to InnerWidth x InnerHeight (Frame clips
// and pads defensively, but sizing is the child's job via the inner size
// Frame reports).
func (f *Frame) SetContent(content string) { f.content = content }

// SetSize sets Frame's OUTER width and height (the region the terminal gave
// it). This is the only size a caller sets on a Frame; children are sized
// from InnerWidth/InnerHeight. Non-positive values are clamped to zero and
// handled by the truncation-safe fallback in View.
func (f *Frame) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	f.outerWidth = width
	f.outerHeight = height
}

// SetSize on a Frame satisfies Sizeable, but note the semantics differ from
// every other component: Frame receives its OUTER size and hands INNER sizes
// down. That asymmetry is the whole point of the type.
var _ Sizeable = (*Frame)(nil)

// chromeHeight is the number of vertical cells Frame's own chrome consumes:
// the top and bottom border rows always, plus one footer row when a footer
// is set. This is the single definition of vertical chrome accounting for
// the entire kit.
func (f Frame) chromeHeight() int {
	h := 2 // top + bottom border
	if f.footer != "" {
		h++ // footer row inside the box
	}
	return h
}

// chromeWidth is the horizontal cells Frame's chrome consumes: the two side
// border columns.
func (f Frame) chromeWidth() int { return 2 }

// InnerWidth is the content width a child should be sized to. It is always
// >= 0; a frame too narrow for any content reports 0 and relies on the
// child's truncation-safe fallback.
func (f Frame) InnerWidth() int {
	w := f.outerWidth - f.chromeWidth()
	if w < 0 {
		return 0
	}
	return w
}

// InnerHeight is the content height a child should be sized to, after Frame
// subtracts its border and footer rows exactly once.
func (f Frame) InnerHeight() int {
	h := f.outerHeight - f.chromeHeight()
	if h < 0 {
		return 0
	}
	return h
}

// View renders the bordered frame. Below FrameMinSize it returns a
// truncation-safe fallback (a single clipped line) rather than drawing
// overlapping or negative-width borders.
func (f Frame) View() string {
	styles := f.theme.Styles()
	if !FrameMinSize.fitsWithin(f.outerWidth, f.outerHeight) {
		// Truncation-safe fallback: one clipped line of whatever we can show,
		// never a broken box.
		w := f.outerWidth
		if w <= 0 {
			return ""
		}
		label := f.title
		if label == "" {
			label = f.content
		}
		return styles.Muted.Render(truncateLine(label, w))
	}

	inner := f.InnerWidth()
	// The border characters are drawn by hand (so the title can be embedded in
	// the top run), which means lipgloss's BorderForeground does not apply -
	// that setting only colors a border lipgloss itself draws. Color the glyphs
	// through the Rule token via the theme's sanctioned Color accessor so the
	// border stays tokens-only. The Canvas background is painted onto the border
	// and corner glyphs too so the themed background fills EVERY cell of the
	// frame region - the border and footer rows are no longer left on the
	// terminal's default background, which read as an inconsistent backdrop that
	// stopped at the last content character.
	canvas := f.theme.Color(f.theme.Palette.Canvas)
	border := lipgloss.NewStyle().
		Foreground(f.theme.Color(f.theme.Palette.Rule)).
		Background(canvas)
	// The horizontal run of border cells between the two corners.
	horiz := inner

	top := f.borderTop(border, horiz)
	bottom := border.Render("└" + strings.Repeat("─", horiz) + "┘")

	var b strings.Builder
	b.WriteString(top)
	b.WriteString("\n")

	contentLines := f.bodyLines(styles, inner)
	for _, line := range contentLines {
		b.WriteString(border.Render("│"))
		b.WriteString(line)
		b.WriteString(border.Render("│"))
		b.WriteString("\n")
	}

	if f.footer != "" {
		b.WriteString(border.Render("│"))
		b.WriteString(fitLine(styles.Muted.Background(canvas), f.footer, inner))
		b.WriteString(border.Render("│"))
		b.WriteString("\n")
	}

	b.WriteString(bottom)
	return b.String()
}

// borderTop draws the top border with the title embedded, truncating the
// title so the border stays exactly horiz cells wide between the corners.
func (f Frame) borderTop(border lipgloss.Style, horiz int) string {
	if f.title == "" {
		return border.Render("┌" + strings.Repeat("─", horiz) + "┐")
	}
	// "┌─ title ─...─┐" : one leading dash, a space, the title, a space,
	// then filler dashes. Title is clipped so the total fill stays == horiz.
	title := f.title
	// cells the decorated title occupies inside the run: "─ " + title + " "
	const fixed = 3 // "─", " ", " "
	avail := horiz - fixed
	if avail < 1 {
		// No room for a title; plain border.
		return border.Render("┌" + strings.Repeat("─", horiz) + "┐")
	}
	title = truncateLine(title, avail)
	used := lipgloss.Width(title) + fixed
	fill := horiz - used
	if fill < 0 {
		fill = 0
	}
	run := "─ " + title + " " + strings.Repeat("─", fill)
	return border.Render("┌"+run) + border.Render("┐")
}

// bodyLines returns exactly InnerHeight lines, each exactly inner cells wide,
// splitting the content string and clipping/padding every line so no row can
// overflow the box or leave a ragged right edge.
func (f Frame) bodyLines(styles theme.Styles, inner int) []string {
	height := f.InnerHeight()
	lines := make([]string, 0, height)
	src := strings.Split(f.content, "\n")
	for i := 0; i < height; i++ {
		var raw string
		if i < len(src) {
			raw = src[i]
		}
		lines = append(lines, fitLine(styles.Base, raw, inner))
	}
	return lines
}
