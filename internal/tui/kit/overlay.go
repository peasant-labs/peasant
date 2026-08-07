package kit

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// OverlayLayer is anything the [Overlay] stack can draw on top of the base
// view: it need only render itself. A layer NEVER holds a pointer back to the
// overlay or the parent program - navigation happens by message. A layer (a
// [Confirm], say) emits a typed result message from its own Update; the
// parent program reacts to that message by calling [Overlay.Pop] (or pushing
// the next layer). This one-way "children emit typed results, the parent
// navigates the stack" shape is why layers carry no parent reference and the
// stack has no callback plumbing.
type OverlayLayer interface {
	View() string
}

// Overlay is a modal layer stack composited over a base view. Push a layer
// to open a modal, pop to close it; the top layer is what the user interacts
// with. Overlay is passive: it owns the stack and the compositing math (both
// via lipgloss's ansi-aware measurement), not the layers' update logic - the
// parent program routes key presses to the top layer and pops it when the
// layer emits its typed result.
type Overlay struct {
	theme  theme.Theme
	layers []OverlayLayer
	width  int
	height int
}

// NewOverlay builds an empty Overlay over theme t. With no layers pushed,
// [Overlay.View] returns its base argument unchanged.
func NewOverlay(t theme.Theme) Overlay {
	return Overlay{theme: t}
}

// SetSize sets the region the overlay composites within, used to center the
// top layer. These are outer terminal cells, not an inner content size.
func (o *Overlay) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	o.width = width
	o.height = height
}

var _ Sizeable = (*Overlay)(nil)

// Push returns a copy of o with layer added as the new top of the stack.
// Overlay is a value type; the caller assigns the result back, keeping the
// bubbles "Update returns the concrete type" discipline.
func (o Overlay) Push(layer OverlayLayer) Overlay {
	o.layers = append(append([]OverlayLayer(nil), o.layers...), layer)
	return o
}

// Pop returns a copy of o with the top layer removed. Popping an empty stack
// is a no-op (returns o unchanged) rather than a panic.
func (o Overlay) Pop() Overlay {
	if len(o.layers) == 0 {
		return o
	}
	o.layers = append([]OverlayLayer(nil), o.layers[:len(o.layers)-1]...)
	return o
}

// Top returns the current top layer and true, or nil and false if the stack
// is empty.
func (o Overlay) Top() (OverlayLayer, bool) {
	if len(o.layers) == 0 {
		return nil, false
	}
	return o.layers[len(o.layers)-1], true
}

// Len reports how many layers are on the stack. Zero means nothing is
// modal and the base view shows through untouched.
func (o Overlay) Len() int { return len(o.layers) }

// View composites the top layer centered over base. With no layers it returns
// base unchanged. The center placement uses lipgloss.Place (ansi-aware) so a
// styled, multi-line layer lands correctly regardless of escape sequences.
func (o Overlay) View(base string) string {
	top, ok := o.Top()
	if !ok {
		return base
	}
	modal := top.View()
	if o.width <= 0 || o.height <= 0 {
		// No region to center within: stack the modal under the base so it is
		// still visible rather than dropped.
		return strings.Join([]string{base, modal}, "\n")
	}
	styles := o.theme.Styles()
	return lipgloss.Place(
		o.width, o.height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceStyle(styles.Base),
	)
}
