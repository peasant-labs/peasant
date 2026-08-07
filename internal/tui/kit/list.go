package kit

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// ListMinSize is the smallest region a List draws into: one row of content
// and enough width for a cursor plus a couple of label cells.
var ListMinSize = Size{Width: 6, Height: 1}

// ListItem is one entry a [List] can display. FilterValue is the text a
// filter matches against (and the default delegate's label when no custom
// delegate is supplied).
type ListItem interface {
	FilterValue() string
}

// ListDelegate renders one item row. Splitting rendering out of List keeps the
// component reusable across item shapes (a plain string row, a two-line
// session row) without a subclass per shape: a caller supplies the delegate
// that knows how to draw its own item. The delegate is handed the theme, the
// item, whether its row is the cursor row, and the exact width it must fit -
// it must return a single line no wider than width (List does not re-clip).
type ListDelegate interface {
	Render(t theme.Theme, item ListItem, selected bool, width int) string
}

// StringItem is the trivial ListItem: a bare string. It exists so the common
// "list of labels" case needs no bespoke item type.
type StringItem string

// FilterValue implements ListItem.
func (s StringItem) FilterValue() string { return string(s) }

// defaultDelegate renders a StringItem-style row: the shared selection cursor
// then the item's FilterValue, styled selected on the cursor row.
type defaultDelegate struct{}

// Render implements ListDelegate.
func (defaultDelegate) Render(t theme.Theme, item ListItem, selected bool, width int) string {
	styles := t.Styles()
	cur := styledCursor(styles, selected)
	labelStyle := styles.Base
	if selected {
		labelStyle = styles.Selected
	}
	label := item.FilterValue()
	body := fitLine(labelStyle, label, width-lipgloss.Width(cur))
	return cur + body
}

// List is a windowed, cursor-driven vertical list. It owns only the cursor
// and the scroll window; per-row rendering is delegated. Movement is
// dispatched through keymap actions (Up/Down/PageUp/PageDown), never raw key
// comparisons.
type List struct {
	theme    theme.Theme
	keymap   keymap.Keymap
	delegate ListDelegate
	items    []ListItem
	cursor   int
	offset   int
	width    int
	height   int
	focused  bool
}

// NewList builds a List over theme t with the given items, using the default
// string delegate. Supply a custom delegate with [List.WithDelegate].
func NewList(t theme.Theme, items []ListItem) List {
	return List{
		theme:    t,
		keymap:   keymap.Default(),
		delegate: defaultDelegate{},
		items:    items,
		width:    ListMinSize.Width,
		height:   ListMinSize.Height,
	}
}

// WithDelegate returns a copy of l that renders rows with d.
func (l List) WithDelegate(d ListDelegate) List {
	l.delegate = d
	return l
}

// Focus gives the List keyboard focus.
func (l *List) Focus() tea.Cmd { l.focused = true; return nil }

// Blur removes keyboard focus.
func (l *List) Blur() { l.focused = false }

// Focused reports whether the List holds focus.
func (l List) Focused() bool { return l.focused }

var _ Focusable = (*List)(nil)

// SetSize sets the inner region the List draws into and re-clamps the scroll
// window so the cursor stays visible at the new height.
func (l *List) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	l.width = width
	l.height = height
	l.clampWindow()
}

var _ Sizeable = (*List)(nil)

// AvailableActions reports the actions a List dispatches, in priority order.
func (l List) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{
		keymap.ActionUp,
		keymap.ActionDown,
		keymap.ActionPageUp,
		keymap.ActionPageDown,
		keymap.ActionConfirm,
	}
}

var _ keymap.Availability = List{}

// Selected returns the item under the cursor and true, or nil and false when
// the list is empty.
func (l List) Selected() (ListItem, bool) {
	if len(l.items) == 0 || l.cursor < 0 || l.cursor >= len(l.items) {
		return nil, false
	}
	return l.items[l.cursor], true
}

// Cursor reports the current cursor index (0 when empty).
func (l List) Cursor() int { return l.cursor }

// Update moves the cursor/window on a dispatched navigation action and
// returns the concrete List. Non-key and unmatched messages are ignored.
func (l List) Update(msg tea.Msg) (List, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return l, nil
	}
	action, ok := keymap.Match(l.keymap, keyMsg, l)
	if !ok {
		return l, nil
	}
	page := l.height
	if page < 1 {
		page = 1
	}
	switch action {
	case keymap.ActionUp:
		l.moveCursor(-1)
	case keymap.ActionDown:
		l.moveCursor(1)
	case keymap.ActionPageUp:
		l.moveCursor(-page)
	case keymap.ActionPageDown:
		l.moveCursor(page)
	}
	return l, nil
}

// moveCursor moves the cursor by delta, clamped to the item range, then
// re-clamps the scroll window.
func (l *List) moveCursor(delta int) {
	if len(l.items) == 0 {
		l.cursor = 0
		l.offset = 0
		return
	}
	l.cursor += delta
	if l.cursor < 0 {
		l.cursor = 0
	}
	if l.cursor >= len(l.items) {
		l.cursor = len(l.items) - 1
	}
	l.clampWindow()
}

// clampWindow scrolls the visible window so the cursor stays inside it.
func (l *List) clampWindow() {
	if l.height < 1 || len(l.items) == 0 {
		l.offset = 0
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+l.height {
		l.offset = l.cursor - l.height + 1
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

// View renders exactly height rows of the visible window, delegating each
// row. An empty list renders a muted placeholder clipped to width. Below
// ListMinSize it renders a single truncated placeholder line.
func (l List) View() string {
	styles := l.theme.Styles()
	if !ListMinSize.fitsWithin(l.width, l.height) {
		if item, ok := l.Selected(); ok {
			return styles.Muted.Render(truncateLine(item.FilterValue(), l.width))
		}
		return ""
	}
	if len(l.items) == 0 {
		return fitLine(styles.Muted, "no items", l.width)
	}
	end := l.offset + l.height
	if end > len(l.items) {
		end = len(l.items)
	}
	rows := make([]string, 0, l.height)
	for i := l.offset; i < end; i++ {
		rows = append(rows, l.delegate.Render(l.theme, l.items[i], i == l.cursor, l.width))
	}
	// Pad the window to a full height with blank rows so the box never jumps.
	for len(rows) < l.height {
		rows = append(rows, fitLine(styles.Base, "", l.width))
	}
	return strings.Join(rows, "\n")
}
