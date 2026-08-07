package kit

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// RadioMinSize is the smallest region a Radio draws into: one option row wide
// enough for the cursor, the radio glyph, and a short label.
var RadioMinSize = Size{Width: 8, Height: 1}

// Radio is a single-choice control: a vertical list of options where exactly
// one is selected, marked with a filled radio glyph, and a movable cursor.
// Up/Down move the cursor; ActionToggle (or Confirm) selects the cursor's
// option. It shares the "▸" cursor vocabulary with the other selection
// controls.
type Radio struct {
	theme    theme.Theme
	keymap   keymap.Keymap
	options  []string
	cursor   int
	selected int
	focused  bool
	width    int
	height   int
	offset   int
}

// NewRadio builds a Radio over theme t with the given options; the first
// option is selected initially. With no options it renders an empty
// placeholder.
func NewRadio(t theme.Theme, options []string) Radio {
	return Radio{
		theme:   t,
		keymap:  keymap.Default(),
		options: options,
		width:   RadioMinSize.Width,
		height:  RadioMinSize.Height,
	}
}

// Focus gives the Radio keyboard focus.
func (r *Radio) Focus() tea.Cmd { r.focused = true; return nil }

// Blur removes keyboard focus.
func (r *Radio) Blur() { r.focused = false }

// Focused reports whether the Radio holds focus.
func (r Radio) Focused() bool { return r.focused }

var _ Focusable = (*Radio)(nil)

// SetSize sets the inner region and re-clamps the scroll window.
func (r *Radio) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	r.width = width
	r.height = height
	r.clampWindow()
}

var _ Sizeable = (*Radio)(nil)

// AvailableActions reports the actions a Radio dispatches, in priority order.
func (r Radio) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{
		keymap.ActionUp,
		keymap.ActionDown,
		keymap.ActionToggle,
		keymap.ActionConfirm,
	}
}

var _ keymap.Availability = Radio{}

// Selected reports the index of the currently selected option.
func (r Radio) Selected() int { return r.selected }

// SelectedValue reports the currently selected option's text and true, or ""
// and false when there are no options.
func (r Radio) SelectedValue() (string, bool) {
	if len(r.options) == 0 {
		return "", false
	}
	return r.options[r.selected], true
}

// Cursor reports the current cursor index.
func (r Radio) Cursor() int { return r.cursor }

// Update moves the cursor on Up/Down and sets the selection to the cursor's
// option on ActionToggle or ActionConfirm.
func (r Radio) Update(msg tea.Msg) (Radio, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return r, nil
	}
	action, ok := keymap.Match(r.keymap, keyMsg, r)
	if !ok {
		return r, nil
	}
	switch action {
	case keymap.ActionUp:
		r.moveCursor(-1)
	case keymap.ActionDown:
		r.moveCursor(1)
	case keymap.ActionToggle, keymap.ActionConfirm:
		if len(r.options) > 0 {
			r.selected = r.cursor
		}
	}
	return r, nil
}

func (r *Radio) moveCursor(delta int) {
	if len(r.options) == 0 {
		r.cursor = 0
		return
	}
	r.cursor += delta
	if r.cursor < 0 {
		r.cursor = 0
	}
	if r.cursor >= len(r.options) {
		r.cursor = len(r.options) - 1
	}
	r.clampWindow()
}

func (r *Radio) clampWindow() {
	if r.height < 1 || len(r.options) == 0 {
		r.offset = 0
		return
	}
	if r.cursor < r.offset {
		r.offset = r.cursor
	}
	if r.cursor >= r.offset+r.height {
		r.offset = r.cursor - r.height + 1
	}
	if r.offset < 0 {
		r.offset = 0
	}
}

// View renders the visible window of option rows. Below RadioMinSize it
// renders a single truncated line of the selected option.
func (r Radio) View() string {
	styles := r.theme.Styles()
	if !RadioMinSize.fitsWithin(r.width, r.height) {
		if v, ok := r.SelectedValue(); ok {
			return styles.Muted.Render(truncateLine(v, r.width))
		}
		return ""
	}
	if len(r.options) == 0 {
		return fitLine(styles.Muted, "no options", r.width)
	}
	end := r.offset + r.height
	if end > len(r.options) {
		end = len(r.options)
	}
	rows := make([]string, 0, r.height)
	for i := r.offset; i < end; i++ {
		rows = append(rows, r.optionRow(styles, i))
	}
	for len(rows) < r.height {
		rows = append(rows, fitLine(styles.Base, "", r.width))
	}
	return strings.Join(rows, "\n")
}

func (r Radio) optionRow(styles theme.Styles, i int) string {
	active := i == r.cursor
	cur := styledCursor(styles, active)
	glyph := radioOff
	glyphStyle := styles.Muted
	if i == r.selected {
		glyph = radioOn
		glyphStyle = styles.Selected
	}
	labelStyle := styles.Base
	if active {
		labelStyle = styles.Selected
	}
	// cursor(2) + glyph(3) + space(1) = 6 chrome cells.
	body := fitLine(labelStyle, r.options[i], r.width-6)
	return cur + glyphStyle.Render(glyph) + styles.Base.Render(" ") + body
}
