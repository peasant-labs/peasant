package keymap_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/footer_render.yaml
var footerRenderFixtureData []byte

type footerRenderDocument struct {
	ExpectedCaseCount int                `yaml:"expectedCaseCount"`
	Cases             []footerRenderCase `yaml:"cases"`
}

type footerRenderCase struct {
	Name          string   `yaml:"name"`
	Available     []string `yaml:"available"`
	WantPlainText string   `yaml:"wantPlainText"`
}

func loadFooterRenderFixture(data []byte) (footerRenderDocument, error) {
	var doc footerRenderDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/footer_render.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/footer_render.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf(
			"testdata/footer_render.yaml: expectedCaseCount=%d but found %d cases (and must be non-zero)",
			doc.ExpectedCaseCount, len(doc.Cases))
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			return doc, fmt.Errorf("testdata/footer_render.yaml: case name %q is missing or duplicated", c.Name)
		}
		seen[c.Name] = true
	}
	return doc, nil
}

// TestFooterView_RendersBothThemes drives keymap.FooterView in BOTH
// theme.Mode values for every fixture case and asserts:
//
//  1. the ansi.Strip'd visible text is IDENTICAL to the fixture's
//     wantPlainText in both modes - the rendered hint text itself must not
//     change with theme, only its color;
//  2. lipgloss.Width on the raw (still color-escaped) rendering equals
//     lipgloss.Width on the stripped plain text - the "ansi-aware width
//     only" validation item: width must be measured ignoring escape codes,
//     not by counting them;
//  3. for a non-empty case, the dark and light renderings are NOT
//     byte-identical - proving t.Styles() is actually threaded through
//     (not hardcoded to one mode, which would pass every other assertion
//     here while silently always rendering the same color).
func TestFooterView_RendersBothThemes(t *testing.T) {
	t.Parallel()
	doc, err := loadFooterRenderFixture(footerRenderFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	km := keymap.Default()

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			avail := availabilityFromNames(t, c.Available)

			dark := keymap.FooterView(theme.New(theme.ModeDark), km, avail)
			light := keymap.FooterView(theme.New(theme.ModeLight), km, avail)

			for _, rendered := range []struct {
				mode string
				text string
			}{{"dark", dark}, {"light", light}} {
				plain := ansi.Strip(rendered.text)
				if plain != c.WantPlainText {
					t.Errorf("FooterView(%s) plain text = %q, want %q", rendered.mode, plain, c.WantPlainText)
				}
				if gotWidth, wantWidth := lipgloss.Width(rendered.text), lipgloss.Width(plain); gotWidth != wantWidth {
					t.Errorf("FooterView(%s): lipgloss.Width(rendered)=%d != lipgloss.Width(stripped)=%d - width must "+
						"be measured ansi-aware, ignoring color escape codes", rendered.mode, gotWidth, wantWidth)
				}
			}

			if c.WantPlainText != "" && dark == light {
				t.Errorf("FooterView rendered identical bytes for ModeDark and ModeLight; theme.Theme.Styles() is not "+
					"actually being threaded through (case %q)", c.Name)
			}
		})
	}
}
