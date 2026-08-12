package kit

import (
	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// ConfirmMinSize is the smallest region a Confirm renders its prompt and the
// two choices into; below it Confirm falls back to a single clipped line. The
// width is the true span of the choices row - the highlighted button
// "[ yes ]" (7) + a two-cell gap (2) + the muted "  no  " (6) = 15 cells -
// so the declared minimum equals what the component actually needs to draw
// both buttons readably rather than truncating them.
var ConfirmMinSize = Size{Width: 15, Height: 2}

// ConfirmResultMsg is the typed result a Confirm emits when the user answers.
// It is the sole way a Confirm communicates outward: the parent program (or
// an [Overlay] host) switches on this message to act on the answer and to pop
// the modal. OK is true for the affirmative choice, false for cancel/back.
// This typed message is the future home of the unified [y/N] prompt every
// destructive kit action will route through.
type ConfirmResultMsg struct {
	OK bool
}

// Confirm is a modal yes/no prompt. It emits [ConfirmResultMsg] from Update
// and holds no pointer to whatever presented it (see [OverlayLayer]); the
// parent navigates on the result. The highlighted choice moves with
// ActionLeft/ActionRight and is submitted with ActionConfirm; ActionBack
// answers cancel outright.
type Confirm struct {
	theme   theme.Theme
	keymap  keymap.Keymap
	prompt  string
	okLabel string
	noLabel string
	// okFocused reports which of the two choices is highlighted; it starts
	// false (No) so a stray Enter on a destructive prompt cancels rather than
	// confirms.
	okFocused bool
	focused   bool
	width     int
	height    int
}

// NewConfirm builds a Confirm over theme t with the given prompt. Labels
// default to "yes"/"no" (lowercase, matching the kit's all-lowercase chrome).
func NewConfirm(t theme.Theme, prompt string) Confirm {
	return Confirm{
		theme:   t,
		keymap:  keymap.Default(),
		prompt:  prompt,
		okLabel: "yes",
		noLabel: "no",
		height:  ConfirmMinSize.Height,
		width:   ConfirmMinSize.Width,
	}
}

// Focus gives the Confirm keyboard focus. It returns no command.
func (c *Confirm) Focus() tea.Cmd { c.focused = true; return nil }

// Blur removes keyboard focus.
func (c *Confirm) Blur() { c.focused = false }

// Focused reports whether the Confirm holds focus.
func (c Confirm) Focused() bool { return c.focused }

var _ Focusable = (*Confirm)(nil)

// SetSize sets the inner region the Confirm draws into.
func (c *Confirm) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	c.width = width
	c.height = height
}

var _ Sizeable = (*Confirm)(nil)

// AvailableActions reports the actions a Confirm dispatches, in priority
// order, so a footer/help mount renders exactly what Confirm can do.
func (c Confirm) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{
		keymap.ActionConfirm,
		keymap.ActionBack,
		keymap.ActionLeft,
		keymap.ActionRight,
	}
}

var _ keymap.Availability = Confirm{}

// Update handles a key press. It returns the concrete Confirm (bubbles
// convention) and, when the user answers, a command that delivers the typed
// [ConfirmResultMsg]. Non-key and unmatched messages leave the model
// unchanged.
func (c Confirm) Update(msg tea.Msg) (Confirm, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return c, nil
	}
	action, ok := keymap.Match(c.keymap, keyMsg, c)
	if !ok {
		return c, nil
	}
	switch action {
	case keymap.ActionLeft:
		c.okFocused = true
	case keymap.ActionRight:
		c.okFocused = false
	case keymap.ActionConfirm:
		result := c.okFocused
		return c, func() tea.Msg { return ConfirmResultMsg{OK: result} }
	case keymap.ActionBack:
		return c, func() tea.Msg { return ConfirmResultMsg{OK: false} }
	}
	return c, nil
}

// View renders the prompt and the two choices, highlighting the focused one.
// Below ConfirmMinSize it returns a single clipped line.
func (c Confirm) View() string {
	styles := c.theme.Styles()
	if !ConfirmMinSize.fitsWithin(c.width, c.height) {
		return styles.Muted.Render(truncateLine(c.prompt, c.width))
	}

	ok := styles.Base.Render("  " + c.okLabel + "  ")
	no := styles.Base.Render("  " + c.noLabel + "  ")
	if c.okFocused {
		ok = styles.Selected.Render("[ " + c.okLabel + " ]")
		no = styles.Muted.Render("  " + c.noLabel + "  ")
	} else {
		no = styles.Selected.Render("[ " + c.noLabel + " ]")
		ok = styles.Muted.Render("  " + c.okLabel + "  ")
	}

	prompt := fitLine(styles.Base, c.prompt, c.width)
	choices := ok + styles.Base.Render("  ") + no
	return prompt + "\n" + choices
}
