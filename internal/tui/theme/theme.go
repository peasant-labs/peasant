package theme

import (
	"fmt"
	"image/color"

	"charm.land/lipgloss/v2"
)

// Theme is a Palette bound to a rendering Mode: everything a kit component
// needs to color itself without ever touching a raw lipgloss.Color or hex
// literal (the internal/tui/gates color grep gate enforces that boundary).
type Theme struct {
	Palette Palette
	Mode    Mode
}

// New builds a Theme over the generated fairtrade Palette
// (internal/tui/theme/palette_gen.go) for the given Mode. This is the ONE
// production entry point a TUI mount uses; there is no path to a Theme built
// over any other Palette outside of tests.
func New(mode Mode) Theme {
	return Theme{Palette: GeneratedPalette, Mode: mode}
}

// ModeFromConfig converts a config.Theme's string value into a Mode.
//
// It takes a bare string rather than importing internal/config's Theme type
// directly, so this leaf package (consumed by every kit component) does not
// depend on the config package; the two enums are kept in lockstep by their
// shared string values ("dark"/"light") and by TestModeFromConfig_AgreesWithConfigTheme
// in internal/config, which asserts config.ThemeDark/ThemeLight round-trip
// through this function.
func ModeFromConfig(configTheme string) (Mode, error) {
	mode := Mode(configTheme)
	if !mode.IsValid() {
		return "", fmt.Errorf(
			"theme.ModeFromConfig: unknown display theme %q.\n"+
				"what: the config value does not name a Mode this package defines.\n"+
				"why: it is neither %q nor %q.\n"+
				"where: theme.ModeFromConfig.\n"+
				"when: converting the loaded config's display.theme into a rendering Mode at TUI mount.\n"+
				"means: the TUI cannot choose a color mode to render in.\n"+
				"fix: this should be unreachable - config.Theme.IsValid is checked at config load "+
				"(internal/config validate()); if it was reached, the config layer's validation gained a value "+
				"this package does not recognize, and the two must be brought back in sync.",
			configTheme, ModeDark, ModeLight)
	}
	return mode, nil
}

// Color returns the color pair's value for this Theme's Mode - a shorthand
// for pair.For(t.Mode) when composing a one-off lipgloss.Style from a
// Palette token outside the curated bundles Styles already provides (e.g. a
// kit component styling a property Styles has no field for). Prefer a
// Styles field when one already fits; this exists so "the token I need isn't
// in Styles yet" is not a reason to reach for a raw lipgloss.Color.
func (t Theme) Color(pair ColorPair) color.Color {
	return pair.For(t.Mode)
}

// Styles is the closed set of per-component style bundles a kit component
// composes from, each derived entirely from Theme's Palette tokens. Adding a
// new bundle here (rather than letting a component call lipgloss.NewStyle
// with a Palette color directly) keeps every semantic color choice in one
// place, reviewable independently of any one component.
type Styles struct {
	// Base is the default page/background text style: Ink on Canvas.
	Base lipgloss.Style
	// Surface is text on a raised panel: Ink on Surface.
	Surface lipgloss.Style
	// SurfaceHover is text on a hovered/highlighted row: Ink on SurfaceHover.
	SurfaceHover lipgloss.Style
	// Header is a section title: bold InkStrong.
	Header lipgloss.Style
	// Muted is secondary/dim text: Ink3.
	Muted lipgloss.Style
	// Border is a panel's resting border color: Rule.
	Border lipgloss.Style
	// BorderFocus is a panel's focused/active border color: FocusRing.
	BorderFocus lipgloss.Style
	// Selected is a selected/highlighted row: AmberFillInk on AmberFill.
	Selected lipgloss.Style
	// Success is affirmative status text: Success.
	Success lipgloss.Style
	// Warning is cautionary status text: Warning.
	Warning lipgloss.Style
	// Danger is destructive/error status text: Danger.
	Danger lipgloss.Style
	// DiffAdd is added-line diff text: AddText.
	DiffAdd lipgloss.Style
	// DiffDel is removed-line diff text: DelText.
	DiffDel lipgloss.Style
}

// Styles derives the full Styles bundle from t's Palette and Mode.
func (t Theme) Styles() Styles {
	p := t.Palette
	m := t.Mode
	return Styles{
		Base:         lipgloss.NewStyle().Foreground(p.Ink.For(m)).Background(p.Canvas.For(m)),
		Surface:      lipgloss.NewStyle().Foreground(p.Ink.For(m)).Background(p.Surface.For(m)),
		SurfaceHover: lipgloss.NewStyle().Foreground(p.Ink.For(m)).Background(p.SurfaceHover.For(m)),
		Header:       lipgloss.NewStyle().Foreground(p.InkStrong.For(m)).Bold(true),
		Muted:        lipgloss.NewStyle().Foreground(p.Ink3.For(m)),
		Border:       lipgloss.NewStyle().BorderForeground(p.Rule.For(m)),
		BorderFocus:  lipgloss.NewStyle().BorderForeground(p.FocusRing.For(m)),
		Selected:     lipgloss.NewStyle().Foreground(p.AmberFillInk.For(m)).Background(p.AmberFill.For(m)),
		Success:      lipgloss.NewStyle().Foreground(p.Success.For(m)),
		Warning:      lipgloss.NewStyle().Foreground(p.Warning.For(m)),
		Danger:       lipgloss.NewStyle().Foreground(p.Danger.For(m)),
		DiffAdd:      lipgloss.NewStyle().Foreground(p.AddText.For(m)),
		DiffDel:      lipgloss.NewStyle().Foreground(p.DelText.For(m)),
	}
}
