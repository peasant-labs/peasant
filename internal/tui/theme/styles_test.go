package theme_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/color"
	"io"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/styles_render.yaml
var stylesRenderFixtureData []byte

type stylesRenderDocument struct {
	ExpectedCaseCount int                `yaml:"expectedCaseCount"`
	Cases             []stylesRenderCase `yaml:"cases"`
}

type stylesRenderCase struct {
	Name         string `yaml:"name"`
	StylesField  string `yaml:"stylesField"`
	Property     string `yaml:"property"`
	PaletteField string `yaml:"paletteField"`
}

func loadStylesRenderFixture(data []byte) (stylesRenderDocument, error) {
	var doc stylesRenderDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/styles_render.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/styles_render.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf(
			"testdata/styles_render.yaml: expectedCaseCount=%d but found %d cases (and must be non-zero)",
			doc.ExpectedCaseCount, len(doc.Cases))
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			return doc, fmt.Errorf("testdata/styles_render.yaml: case name %q is missing or duplicated", c.Name)
		}
		seen[c.Name] = true
		if c.StylesField == "" || c.Property == "" || c.PaletteField == "" {
			return doc, fmt.Errorf("testdata/styles_render.yaml: case %q leaves stylesField/property/paletteField blank", c.Name)
		}
	}
	return doc, nil
}

// TestStyles_DeriveFromPaletteTokens is the render-fixture gate for
// "components use tokens ONLY": for every case, in BOTH modes, it resolves
// the named Palette token via reflection, resolves the derived Styles
// field's actual rendered color via lipgloss's Get* accessors, and asserts
// they are the SAME color. A Styles field that read the wrong Palette field,
// or a derivation that forgot to thread Mode through (e.g. always rendering
// the dark side), fails here even though nothing about it looks wrong in a
// single screenshot.
func TestStyles_DeriveFromPaletteTokens(t *testing.T) {
	t.Parallel()
	doc, err := loadStylesRenderFixture(stylesRenderFixtureData)
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			th := theme.New(mode)
			styles := th.Styles()
			stylesVal := reflect.ValueOf(styles)
			paletteVal := reflect.ValueOf(th.Palette)

			for _, c := range doc.Cases {
				t.Run(c.Name, func(t *testing.T) {
					t.Parallel()
					styleFV := stylesVal.FieldByName(c.StylesField)
					if !styleFV.IsValid() {
						t.Fatalf("theme.Styles has no field %q (fixture case %q)", c.StylesField, c.Name)
					}
					style, ok := styleFV.Interface().(interface {
						GetForeground() color.Color
						GetBackground() color.Color
						GetBorderTopForeground() color.Color
					})
					if !ok {
						t.Fatalf("Styles.%s is not a lipgloss.Style", c.StylesField)
					}

					paletteFV := paletteVal.FieldByName(c.PaletteField)
					if !paletteFV.IsValid() {
						t.Fatalf("theme.Palette has no field %q (fixture case %q)", c.PaletteField, c.Name)
					}
					pair, ok := paletteFV.Interface().(theme.ColorPair)
					if !ok {
						t.Fatalf("Palette.%s is not a theme.ColorPair", c.PaletteField)
					}
					want := pair.For(mode)

					var got color.Color
					switch c.Property {
					case "foreground":
						got = style.GetForeground()
					case "background":
						got = style.GetBackground()
					case "borderForeground":
						got = style.GetBorderTopForeground()
					default:
						t.Fatalf("fixture case %q names unknown property %q (want foreground, background, or borderForeground)", c.Name, c.Property)
					}
					if !colorsEqual(got, want) {
						t.Errorf("mode=%s Styles.%s %s = %v, want Palette.%s.For(%s) = %v",
							mode, c.StylesField, c.Property, hexOf(got), c.PaletteField, mode, hexOf(want))
					}
				})
			}
		})
	}
}

// colorsEqual compares two color.Color values by their resolved RGBA, since
// the concrete types lipgloss.Color returns are not comparable with ==
// across every code path (ANSI-indexed colors resolve through a terminal
// color profile).
func colorsEqual(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == b
	}
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

func hexOf(c color.Color) string {
	if c == nil {
		return "<nil>"
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// TestTheme_New_DefaultsPaletteToGenerated proves theme.New wires the
// generated fairtrade palette, not a zero-value or test-only stand-in - the
// same production Palette every kit component gets.
func TestTheme_New_DefaultsPaletteToGenerated(t *testing.T) {
	t.Parallel()
	th := theme.New(theme.ModeDark)
	if !colorsEqual(th.Palette.Canvas.Dark, theme.GeneratedPalette.Canvas.Dark) {
		t.Fatalf("theme.New(ModeDark).Palette is not theme.GeneratedPalette")
	}
}
