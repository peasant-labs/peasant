// Package kit is the peasant TUI's own component vocabulary, wrapping
// charm.land/bubbles/v2 where a component exists and hand-rolling the rest,
// over the tokens-only theme (internal/tui/theme) and the single keymap
// (internal/tui/keymap). Every future TUI page - the selection tree, the
// preview split, the settings flow, the rebuilt kickstart - composes these
// components rather than re-deriving chrome math, colors, or key handling.
//
// # Design: capability interfaces, not a god Component
//
// There is deliberately NO single Component interface every model must
// satisfy. That shape forces every component to implement (or stub) methods
// it does not need and turns each new cross-cutting concern into an edit of
// one wide interface plus every implementer - the OOP variant explosion the
// project's design guidance calls out. Instead each component is a concrete
// Model with the bubbles convention (New builds it, Update returns the
// concrete type, View renders it), and the shared cross-cutting behaviors
// are small capability interfaces a component satisfies only when it
// actually has that capability: [Focusable] for anything that takes input,
// [Sizeable] for anything that must fit a region. A caller type-asserts for
// the capability it needs. This is the crush pattern, chosen for the same
// reason.
//
// # Kit-wide invariants (enforced by the gates + fixture tests)
//
//   - All width/height math goes through the ansi-aware lipgloss.Width /
//     lipgloss.Height, NEVER len() on a rendered string (len counts bytes,
//     not display cells, and miscounts every escape sequence and wide rune).
//   - Every component declares a minimum size ([MinSize]) and renders a
//     truncation-safe fallback below it - never a panic, never overlapping
//     or clipped chrome.
//   - [Frame] OWNS all chrome-height accounting. Every other component
//     receives an INNER content size via [Sizeable.SetSize] and never
//     subtracts border/title/footer constants itself. This kills the class
//     of per-surface -3/-4/-10/-12 fudge constants the audit found.
//   - Colors come only from a theme.Theme bundle; input is dispatched only
//     through keymap.Match against a named keymap.ActionID. The
//     internal/tui/gates color and key gates enforce both boundaries.
package kit

import (
	tea "charm.land/bubbletea/v2"
)

// Focusable is the capability of a component that accepts keyboard input:
// exactly one focusable component on a screen holds focus at a time, and
// only the focused one interprets key presses. The signatures match
// bubbles/v2's own textinput/textarea (Focus returns a tea.Cmd because a
// component like a text field must start its cursor blink on focus), so a
// kit component wrapping a bubbles model can forward straight through.
type Focusable interface {
	// Focus gives the component keyboard focus and returns any command it
	// needs to run as a result (e.g. a cursor blink); the command may be
	// nil for a component with nothing to start.
	Focus() tea.Cmd
	// Blur removes keyboard focus. It takes no command because losing focus
	// only stops behavior, it never starts any.
	Blur()
	// Focused reports whether the component currently holds focus.
	Focused() bool
}

// Sizeable is the capability of a component that must fit a bounded region.
// The width and height passed are the INNER content size the component may
// draw into - the caller (typically a [Frame]) has already subtracted its
// own chrome. A component never subtracts border/title/footer height from
// the size it is handed; if it cannot fit the region it renders a
// truncation-safe fallback (see [MinSize]), never a panic or an overflow.
type Sizeable interface {
	// SetSize sets the inner content width and height the component draws
	// into. Non-positive values are treated as the component's minimum and
	// never panic.
	SetSize(width, height int)
}

// Size is a component's declared minimum drawable size in terminal cells.
// Below either dimension a component must still render a truncation-safe
// fallback (a clipped single line, an ellipsis) rather than panic or draw
// overlapping chrome - the property the both-themes x constrained-size
// render fixture matrix exists to prove.
type Size struct {
	Width  int
	Height int
}

// fitsWithin reports whether an available (width, height) meets this
// minimum size on both axes.
func (s Size) fitsWithin(width, height int) bool {
	return width >= s.Width && height >= s.Height
}
