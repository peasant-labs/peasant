package kit

import (
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// TextFieldMinSize is the smallest region a TextField draws into: a couple of
// cells of a single input row.
var TextFieldMinSize = Size{Width: 3, Height: 1}

// TextField is a single-line text input wrapping bubbles/v2's textinput. In
// v2 the cursor moved into its own bubbles/v2/cursor package and textinput
// can drive either a real terminal cursor or a "virtual" cursor drawn inline
// in the returned string; the kit uses the VIRTUAL cursor (SetVirtualCursor)
// so a TextField's View is a self-contained string a [Frame] can place like
// any other content, and so render fixtures are deterministic without a live
// terminal cursor position.
type TextField struct {
	theme theme.Theme
	inner textinput.Model
	width int
}

// NewTextField builds a TextField over theme t with the given placeholder.
func NewTextField(t theme.Theme, placeholder string) TextField {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.SetVirtualCursor(true)
	styles := t.Styles()
	s := ti.Styles()
	s.Focused.Text = styles.Base
	s.Focused.Placeholder = styles.Muted
	s.Focused.Prompt = styles.Selected
	s.Blurred.Text = styles.Muted
	s.Blurred.Placeholder = styles.Muted
	s.Blurred.Prompt = styles.Muted
	ti.SetStyles(s)
	ti.SetWidth(TextFieldMinSize.Width)
	return TextField{theme: t, inner: ti, width: TextFieldMinSize.Width}
}

// Focus gives the field keyboard focus and returns the cursor-blink command
// the wrapped input starts on focus.
func (f *TextField) Focus() tea.Cmd { return f.inner.Focus() }

// Blur removes keyboard focus.
func (f *TextField) Blur() { f.inner.Blur() }

// Focused reports whether the field holds focus.
func (f TextField) Focused() bool { return f.inner.Focused() }

var _ Focusable = (*TextField)(nil)

// SetSize sets the row width the whole field (prompt + text + cursor) must
// fit. The wrapped input measures only its TEXT span, so the prompt and the
// trailing cursor cell are chrome the field reserves here; without that
// reservation the rendered row would run [overhead] cells past the width it
// was sized to. Height is always one row; the height argument is accepted to
// satisfy Sizeable and otherwise ignored.
func (f *TextField) SetSize(width, _ int) {
	if width < 0 {
		width = 0
	}
	f.width = width
	text := width - f.overhead()
	if text < 1 {
		text = 1
	}
	f.inner.SetWidth(text)
}

// overhead is the non-text width the wrapped input always draws: the prompt
// run plus the one trailing cell the virtual cursor occupies. Reserving it
// from the field's width keeps the rendered row within its declared width.
func (f TextField) overhead() int {
	return lipgloss.Width(f.inner.Prompt) + 1
}

var _ Sizeable = (*TextField)(nil)

// Value returns the current text.
func (f TextField) Value() string { return f.inner.Value() }

// SetValue replaces the current text.
func (f *TextField) SetValue(s string) { f.inner.SetValue(s) }

// Update forwards the message to the wrapped input (which handles its own
// editing keys) and returns the concrete TextField. The wrapped input owns
// text-editing key handling internally; the kit's key gate concerns only the
// kit's OWN dispatch code, which forwards here rather than comparing key
// strings itself.
func (f TextField) Update(msg tea.Msg) (TextField, tea.Cmd) {
	var cmd tea.Cmd
	f.inner, cmd = f.inner.Update(msg)
	return f, cmd
}

// View renders the input row (prompt, text or placeholder, virtual cursor).
// When the field is sized too narrow to seat the prompt, one text cell, and
// the cursor, it renders a truncation-safe muted fallback of the plain
// value/placeholder instead of letting the wrapped input overflow the width.
func (f TextField) View() string {
	if f.width > 0 && f.width < f.overhead()+1 {
		styles := f.theme.Styles()
		text := f.inner.Value()
		if text == "" {
			text = f.inner.Placeholder
		}
		return fitLine(styles.Muted, text, f.width)
	}
	return f.inner.View()
}
