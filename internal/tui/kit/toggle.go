package kit

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// ToggleMinSize is the smallest region a Toggle draws into: a label plus its
// on/off indicator on a single row.
var ToggleMinSize = Size{Width: 6, Height: 1}

// Toggle is a single boolean control: a label and an on/off indicator flipped
// with ActionToggle. It is the kit's building block for a yes/no setting.
type Toggle struct {
	theme   theme.Theme
	keymap  keymap.Keymap
	label   string
	on      bool
	focused bool
	width   int
}

// NewToggle builds a Toggle over theme t with the given label and initial
// value.
func NewToggle(t theme.Theme, label string, on bool) Toggle {
	return Toggle{
		theme:  t,
		keymap: keymap.Default(),
		label:  label,
		on:     on,
		width:  ToggleMinSize.Width,
	}
}

// Focus gives the Toggle keyboard focus.
func (t *Toggle) Focus() tea.Cmd { t.focused = true; return nil }

// Blur removes keyboard focus.
func (t *Toggle) Blur() { t.focused = false }

// Focused reports whether the Toggle holds focus.
func (t Toggle) Focused() bool { return t.focused }

var _ Focusable = (*Toggle)(nil)

// SetSize sets the row width. Height is always one row.
func (t *Toggle) SetSize(width, _ int) {
	if width < 0 {
		width = 0
	}
	t.width = width
}

var _ Sizeable = (*Toggle)(nil)

// On reports the current boolean value.
func (t Toggle) On() bool { return t.on }

// AvailableActions reports the actions a Toggle dispatches.
func (t Toggle) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionToggle}
}

var _ keymap.Availability = Toggle{}

// Update flips the value on ActionToggle and returns the concrete Toggle.
func (t Toggle) Update(msg tea.Msg) (Toggle, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return t, nil
	}
	action, ok := keymap.Match(t.keymap, keyMsg, t)
	if !ok {
		return t, nil
	}
	if action == keymap.ActionToggle {
		t.on = !t.on
	}
	return t, nil
}

// View renders the label and the on/off indicator, clipped to width. Below
// ToggleMinSize it renders just the indicator.
func (t Toggle) View() string {
	styles := t.theme.Styles()
	indicator := styles.Muted.Render("[off]")
	if t.on {
		indicator = styles.Success.Render("[on]")
	}
	if !ToggleMinSize.fitsWithin(t.width, 1) {
		return truncateLine(indicator, t.width)
	}
	// Measure the indicator dynamically: "[on]" is 4 cells, "[off]" is 5, so a
	// fixed budget would leave the on-state one cell short of t.width. Reserve
	// exactly the indicator's width plus one separator cell so label +
	// separator + indicator fills t.width exactly in BOTH states.
	label := fitLine(styles.Base, t.label, t.width-lipgloss.Width(indicator)-1)
	return label + " " + indicator
}
