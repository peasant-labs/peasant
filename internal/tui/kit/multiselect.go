package kit

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// MultiSelectMinSize is the smallest region a MultiSelect draws into: one
// option row wide enough for the cursor, the checkbox, and a short label.
var MultiSelectMinSize = Size{Width: 8, Height: 1}

// MultiSelect is a multi-choice control: a vertical list of options each with
// a checkbox, any number of which can be checked. Up/Down move the cursor,
// ActionToggle checks/unchecks the cursor's option, ActionSelectAll toggles
// every option at once. It uses the "▸" cursor and "[✓]"/"[ ]" checkbox
// vocabulary hoisted from the legacy FTUE selection screens.
type MultiSelect struct {
	theme   theme.Theme
	keymap  keymap.Keymap
	options []string
	checked []bool
	cursor  int
	offset  int
	focused bool
	width   int
	height  int
}

// NewMultiSelect builds a MultiSelect over theme t with the given options,
// all initially unchecked.
func NewMultiSelect(t theme.Theme, options []string) MultiSelect {
	return MultiSelect{
		theme:   t,
		keymap:  keymap.Default(),
		options: options,
		checked: make([]bool, len(options)),
		width:   MultiSelectMinSize.Width,
		height:  MultiSelectMinSize.Height,
	}
}

// Focus gives the MultiSelect keyboard focus.
func (m *MultiSelect) Focus() tea.Cmd { m.focused = true; return nil }

// Blur removes keyboard focus.
func (m *MultiSelect) Blur() { m.focused = false }

// Focused reports whether the MultiSelect holds focus.
func (m MultiSelect) Focused() bool { return m.focused }

var _ Focusable = (*MultiSelect)(nil)

// SetSize sets the inner region and re-clamps the scroll window.
func (m *MultiSelect) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	m.width = width
	m.height = height
	m.clampWindow()
}

var _ Sizeable = (*MultiSelect)(nil)

// AvailableActions reports the actions a MultiSelect dispatches, in priority
// order.
func (m MultiSelect) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{
		keymap.ActionUp,
		keymap.ActionDown,
		keymap.ActionToggle,
		keymap.ActionSelectAll,
	}
}

var _ keymap.Availability = MultiSelect{}

// Cursor reports the current cursor index.
func (m MultiSelect) Cursor() int { return m.cursor }

// Checked reports whether the option at index i is checked. An out-of-range
// index reports false rather than panicking.
func (m MultiSelect) Checked(i int) bool {
	if i < 0 || i >= len(m.checked) {
		return false
	}
	return m.checked[i]
}

// Selected returns the indices of every checked option, in order.
func (m MultiSelect) Selected() []int {
	var out []int
	for i, c := range m.checked {
		if c {
			out = append(out, i)
		}
	}
	return out
}

// Update handles navigation and selection and returns the concrete
// MultiSelect.
func (m MultiSelect) Update(msg tea.Msg) (MultiSelect, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	action, ok := keymap.Match(m.keymap, keyMsg, m)
	if !ok {
		return m, nil
	}
	switch action {
	case keymap.ActionUp:
		m.moveCursor(-1)
	case keymap.ActionDown:
		m.moveCursor(1)
	case keymap.ActionToggle:
		if m.cursor >= 0 && m.cursor < len(m.checked) {
			m.checked[m.cursor] = !m.checked[m.cursor]
		}
	case keymap.ActionSelectAll:
		m.toggleAll()
	}
	return m, nil
}

// toggleAll checks every option if any is currently unchecked, otherwise
// unchecks every option - a single key that reaches "all selected" and then
// "none selected".
func (m *MultiSelect) toggleAll() {
	allChecked := len(m.checked) > 0
	for _, c := range m.checked {
		if !c {
			allChecked = false
			break
		}
	}
	next := make([]bool, len(m.checked))
	for i := range next {
		next[i] = !allChecked
	}
	m.checked = next
}

func (m *MultiSelect) moveCursor(delta int) {
	if len(m.options) == 0 {
		m.cursor = 0
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.options) {
		m.cursor = len(m.options) - 1
	}
	m.clampWindow()
}

func (m *MultiSelect) clampWindow() {
	if m.height < 1 || len(m.options) == 0 {
		m.offset = 0
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+m.height {
		m.offset = m.cursor - m.height + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// View renders the visible window of option rows. Below MultiSelectMinSize it
// renders a single truncated line of the cursor's option.
func (m MultiSelect) View() string {
	styles := m.theme.Styles()
	if !MultiSelectMinSize.fitsWithin(m.width, m.height) {
		if m.cursor >= 0 && m.cursor < len(m.options) {
			return styles.Muted.Render(truncateLine(m.options[m.cursor], m.width))
		}
		return ""
	}
	if len(m.options) == 0 {
		return fitLine(styles.Muted, "no options", m.width)
	}
	end := m.offset + m.height
	if end > len(m.options) {
		end = len(m.options)
	}
	rows := make([]string, 0, m.height)
	for i := m.offset; i < end; i++ {
		rows = append(rows, m.optionRow(styles, i))
	}
	for len(rows) < m.height {
		rows = append(rows, fitLine(styles.Base, "", m.width))
	}
	return strings.Join(rows, "\n")
}

func (m MultiSelect) optionRow(styles theme.Styles, i int) string {
	active := i == m.cursor
	cur := styledCursor(styles, active)
	box := uncheckedBox
	boxStyle := styles.Muted
	if m.checked[i] {
		box = checkedBox
		boxStyle = styles.Success
	}
	labelStyle := styles.Base
	if active {
		labelStyle = styles.Selected
	}
	// cursor(2) + box(3) + space(1) = 6 chrome cells.
	body := fitLine(labelStyle, m.options[i], m.width-6)
	return cur + boxStyle.Render(box) + styles.Base.Render(" ") + body
}
