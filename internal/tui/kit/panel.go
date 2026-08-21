package kit

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// wrapText breaks plain text at width cells, ansi-aware.
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	return ansi.Wrap(text, width, "")
}

// PanelAlign is the horizontal placement of a panel line inside the panel
// box. It is a closed set: a surface either flushes its lines left or centers
// them, and no other value is renderable.
type PanelAlign string

const (
	// PanelAlignLeft flushes every line to the left edge of the panel box.
	PanelAlignLeft PanelAlign = "left"
	// PanelAlignCenter centers every line inside the panel box.
	PanelAlignCenter PanelAlign = "center"
)

// String implements fmt.Stringer.
func (a PanelAlign) String() string { return string(a) }

// IsValid reports whether a names a placement Panel can render.
func (a PanelAlign) IsValid() bool {
	switch a {
	case PanelAlignLeft, PanelAlignCenter:
		return true
	}
	return false
}

// panelLine is one row of panel content. rendered marks content that already
// carries ANSI styling (a child component's View), which must be fitted
// inside the panel style instead of being wrapped in a second style.
type panelLine struct {
	text     string
	style    lipgloss.Style
	rendered bool
}

// Panel is the shared surface primitive: a block of lines that always paints
// EVERY cell of its box with one background token.
//
// The recurring ragged-background bug has one cause. A theme text role
// (Header, Muted, Danger) carries a foreground only, and lipgloss pads a line
// to a target width only when that line is narrower than the target. A block
// of mixed-length lines therefore paints a different-width background box per
// line, so the block reads as a staircase instead of one panel. Every surface
// that hit this bug re-implemented the same repair: measure the widest line,
// pad all lines to that width, paint the background, and optionally center.
//
// Panel owns that repair once. It measures the widest line, fits EVERY line
// to one shared width, applies the panel background to every style it renders
// with, and pads the block to the requested height. Use [Panel.Style] to
// derive a text style that carries the panel background, instead of calling
// Background on a theme style at the surface.
//
// Panel is a value type. The line builders take a pointer receiver so a
// surface can append lines in sequence, and the option methods return a copy
// so a configured panel can be reused.
type Panel struct {
	theme      theme.Theme
	background theme.ColorPair
	align      PanelAlign
	lines      []panelLine

	width  int
	height int
}

// NewPanel builds a Panel over theme t. The default background is the Canvas
// token (the page background) and the default alignment is left. Use
// [Panel.WithBackground] for a raised panel on the Surface token.
func NewPanel(t theme.Theme) Panel {
	return Panel{theme: t, background: t.Palette.Canvas, align: PanelAlignLeft}
}

// WithBackground returns a copy of p that paints background token pair
// instead of Canvas.
func (p Panel) WithBackground(pair theme.ColorPair) Panel {
	p.background = pair
	return p
}

// WithAlign returns a copy of p with its line placement set. An invalid value
// keeps the current placement, so a Panel can never render an unknown
// alignment.
func (p Panel) WithAlign(align PanelAlign) Panel {
	if !align.IsValid() {
		return p
	}
	p.align = align
	return p
}

// Align reports the panel's current line placement.
func (p Panel) Align() PanelAlign { return p.align }

// Style returns base carrying the panel's background. This is the ONE
// sanctioned way for a surface to put a foreground-only theme role (Header,
// Muted, Danger) onto a filled region. The layout gate
// (internal/tui/gates/layout.go) fails a surface that calls Background
// directly instead.
//
// A role that ALREADY names its own background is returned unchanged. Selected
// is Ink on AmberFill, a deliberate full-cell highlight, and Surface is a
// raised panel; repainting either with the panel background would erase the
// very thing the role exists to draw.
func (p Panel) Style(base lipgloss.Style) lipgloss.Style {
	if !isUnsetColor(base.GetBackground()) {
		return base
	}
	return base.Background(p.theme.Color(p.background))
}

// isUnsetColor reports whether a style property holds no color. lipgloss
// reports an unset property as NoColor rather than nil, so both forms count
// as unset.
func isUnsetColor(c color.Color) bool {
	if c == nil {
		return true
	}
	_, noColor := c.(lipgloss.NoColor)
	return noColor
}

// Base returns the panel's own text style: the theme's Base foreground on the
// panel background.
func (p Panel) Base() lipgloss.Style {
	return p.Style(p.theme.Styles().Base)
}

// Line appends one plain-text line rendered with style. The panel background
// is applied to style, so the caller passes a bare theme role.
func (p *Panel) Line(style lipgloss.Style, text string) {
	p.lines = append(p.lines, panelLine{text: text, style: style})
}

// Text appends one plain-text line rendered with the panel's Base style.
func (p *Panel) Text(text string) { p.Line(p.theme.Styles().Base, text) }

// Blank appends one empty line, painted with the panel background like every
// other row.
func (p *Panel) Blank() { p.Text("") }

// Wrapped appends text wrapped to the panel's content width, one panel line
// per wrapped line, all rendered with style. A panel with no explicit width
// cannot wrap, so the text is appended as a single line.
func (p *Panel) Wrapped(style lipgloss.Style, text string) {
	if p.width <= 0 {
		p.Line(style, text)
		return
	}
	for _, line := range strings.Split(wrapText(text, p.width), "\n") {
		p.Line(style, line)
	}
}

// Rendered appends content that ALREADY carries ANSI styling, such as a child
// component's View. Multi-line content becomes one panel line per line. The
// panel fits each line inside its own style, so unstyled cells in the child
// still inherit the panel background instead of the terminal default.
func (p *Panel) Rendered(content string) {
	for _, line := range strings.Split(content, "\n") {
		p.lines = append(p.lines, panelLine{text: line, rendered: true})
	}
}

// SetSize sets the panel box in terminal cells. A non-positive width means
// "shrink to the widest line", which is what a centered dialog wants; a
// non-positive height means "as many lines as there are". Negative values are
// clamped to zero.
func (p *Panel) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	p.width = width
	p.height = height
}

var _ Sizeable = (*Panel)(nil)

// ContentWidth reports the width every rendered line is fitted to: the
// requested width when one is set, otherwise the widest line in the panel.
func (p Panel) ContentWidth() int {
	if p.width > 0 {
		return p.width
	}
	widest := 0
	for _, line := range p.lines {
		if w := lipgloss.Width(line.text); w > widest {
			widest = w
		}
	}
	return widest
}

// LineCount reports how many lines View renders: the requested height when
// one is set, otherwise the number of appended lines.
func (p Panel) LineCount() int {
	if p.height > 0 {
		return p.height
	}
	return len(p.lines)
}

// View renders the panel. Every returned line is exactly ContentWidth cells
// wide and paints the panel background through its final cell, so the block
// has one straight right edge in both themes.
func (p Panel) View() string {
	width := p.ContentWidth()
	if width <= 0 {
		return ""
	}
	base := p.Base()
	count := p.LineCount()
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		if i >= len(p.lines) {
			out = append(out, FitLine(base, "", width))
			continue
		}
		out = append(out, p.renderLine(p.lines[i], base, width))
	}
	return strings.Join(out, "\n")
}

// renderLine fits one line to width, applying the panel alignment. Alignment
// is a left pad computed before fitting, so the padded cells carry the same
// style (and therefore the same background) as the text itself.
func (p Panel) renderLine(line panelLine, base lipgloss.Style, width int) string {
	text := line.text
	if p.align == PanelAlignCenter {
		if pad := (width - lipgloss.Width(text)) / 2; pad > 0 {
			text = spaces(pad) + text
		}
	}
	if line.rendered {
		return fitRenderedLine(base, text, width)
	}
	return FitLine(p.Style(line.style), text, width)
}
