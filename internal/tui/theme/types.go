// Package theme is the single terminal color source for the peasant TUI kit.
//
// The closed-set Palette (internal/tui/theme/palette_gen.go, generated) is
// sourced from the fairtrade design system's tokens.json; every other TUI
// package derives its colors from a Theme built over that Palette, never
// from a hand-picked lipgloss.Color or hex literal. internal/tui/gates'
// color grep gate enforces that boundary across internal/tui/... and
// internal/push/wizard.go.
package theme

import (
	"fmt"
	"image/color"
)

// Mode selects which side of a ColorPair a component renders. It is the
// terminal-rendering counterpart of config.Theme (internal/config/config.go);
// the config value is read once at TUI mount and converted to a Mode via
// ModeFromConfig.
type Mode string

const (
	// ModeDark selects the dark side of every ColorPair. This is the default
	// terminal mode (matches config.ThemeDark).
	ModeDark Mode = "dark"
	// ModeLight selects the light side of every ColorPair.
	ModeLight Mode = "light"
)

// String implements fmt.Stringer.
func (m Mode) String() string { return string(m) }

// IsValid reports whether m is a known Mode.
func (m Mode) IsValid() bool {
	switch m {
	case ModeDark, ModeLight:
		return true
	}
	return false
}

// ColorPair is one fairtrade color token's terminal color in both modes.
// Every field of the generated Palette is a ColorPair; both Dark and Light
// are always populated (gen.ParseColorTokens fails closed on any token
// missing either side), so For never has a missing value to fall back to.
type ColorPair struct {
	Dark  color.Color
	Light color.Color
}

// For returns the color for the given mode.
//
// mode is expected to always be ModeDark or ModeLight: it is either a
// theme.Theme's own Mode (set once from the validated config.Theme at TUI
// mount, see config.Theme.IsValid) or a literal ModeDark/ModeLight passed by
// a test. An invalid Mode reaching here is a programming error, not bad user
// input - config.Theme is validated at config load, before it is ever
// converted to a theme.Mode - so For panics with an actionable message
// rather than silently guessing a side, which would render the wrong colors
// without any signal that something upstream skipped validation.
func (c ColorPair) For(mode Mode) color.Color {
	switch mode {
	case ModeDark:
		return c.Dark
	case ModeLight:
		return c.Light
	default:
		panic(fmt.Sprintf(
			"theme: ColorPair.For called with invalid Mode %q.\n"+
				"what: mode is neither theme.ModeDark nor theme.ModeLight.\n"+
				"why: the caller did not validate its Mode before calling For.\n"+
				"where: theme.ColorPair.For.\n"+
				"when: while resolving a color for rendering.\n"+
				"means: this is a bug in the caller, not user input - config.Theme is validated at config load "+
				"(config.Theme.IsValid) before any theme.Mode is derived from it.\n"+
				"fix: construct the Mode via theme.ModeDark/theme.ModeLight (or theme.New with a validated config.Theme), "+
				"never a bare string.",
			mode))
	}
}
