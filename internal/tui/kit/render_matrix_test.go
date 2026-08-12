package kit_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/render_matrix.yaml
var renderMatrixData []byte

// renderMatrixCase is one component x theme x size row of the render matrix.
type renderMatrixCase struct {
	Name      string `yaml:"name"`
	Component string `yaml:"component"`
	Theme     string `yaml:"theme"`
	Width     int    `yaml:"width"`
	Height    int    `yaml:"height"`
}

// renderMatrixDocument is the whole fixture, with the self-consistency counts
// the row-count guards assert against so the matrix cannot silently drop a
// component, a theme, or a size.
type renderMatrixDocument struct {
	ExpectedCaseCount              int                `yaml:"expectedCaseCount"`
	ExpectedComponentCount         int                `yaml:"expectedComponentCount"`
	ExpectedSizesPerComponentTheme int                `yaml:"expectedSizesPerComponentTheme"`
	Components                     []string           `yaml:"components"`
	Cases                          []renderMatrixCase `yaml:"cases"`
}

func loadRenderMatrix(t *testing.T) renderMatrixDocument {
	t.Helper()
	var doc renderMatrixDocument
	dec := yaml.NewDecoder(bytes.NewReader(renderMatrixData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/render_matrix.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("render_matrix.yaml must hold exactly one document: %v", err)
	}
	return doc
}

// TestRenderMatrix_FixtureIsExhaustive is the row-count guard: it proves the
// fixture still covers every declared component in BOTH themes at exactly the
// declared number of sizes, and that the case count matches - so a new
// component, or a dropped size/theme, cannot slip through untested.
func TestRenderMatrix_FixtureIsExhaustive(t *testing.T) {
	doc := loadRenderMatrix(t)

	if doc.ExpectedComponentCount != len(doc.Components) || len(doc.Components) == 0 {
		t.Fatalf("expectedComponentCount=%d but %d components listed", doc.ExpectedComponentCount, len(doc.Components))
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	wantPerComponent := 2 * doc.ExpectedSizesPerComponentTheme // dark + light
	if doc.ExpectedCaseCount != len(doc.Components)*wantPerComponent {
		t.Fatalf("case count %d != components(%d) * themes(2) * sizes(%d)",
			doc.ExpectedCaseCount, len(doc.Components), doc.ExpectedSizesPerComponentTheme)
	}

	// Per (component, theme) there must be exactly the declared number of
	// distinct sizes, and case names must be unique.
	type ct struct{ comp, theme string }
	perCT := map[ct]int{}
	names := map[string]bool{}
	known := map[string]bool{}
	for _, c := range doc.Components {
		known[c] = true
	}
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("case name %q missing or duplicated", c.Name)
		}
		names[c.Name] = true
		if !known[c.Component] {
			t.Fatalf("case %q names component %q not in the components list", c.Name, c.Component)
		}
		if c.Theme != "dark" && c.Theme != "light" {
			t.Fatalf("case %q has theme %q; want dark or light", c.Name, c.Theme)
		}
		if c.Width <= 0 || c.Height <= 0 {
			t.Fatalf("case %q has non-positive size %dx%d", c.Name, c.Width, c.Height)
		}
		perCT[ct{c.Component, c.Theme}]++
	}
	for _, comp := range doc.Components {
		for _, th := range []string{"dark", "light"} {
			if got := perCT[ct{comp, th}]; got != doc.ExpectedSizesPerComponentTheme {
				t.Fatalf("component %q theme %q has %d sizes; want %d", comp, th, got, doc.ExpectedSizesPerComponentTheme)
			}
		}
	}
}

// TestRenderMatrix_WidthInvariant proves that every component renders WITHIN
// the width it was sized to: for every matrix case, no rendered line's
// ansi-aware display width exceeds the case width. A component that draws
// wider than its declared width (a fixed indicator budget that shorts the
// on-state, a choices row wider than the declared minimum) would bake that
// overflow into its golden and the golden test would silently "accept" the
// bug; this invariant makes the goldens non-vacuous for width by measuring
// the real output independently, exactly as reviewers did by hand. Run across
// all ten components at all three sizes.
func TestRenderMatrix_WidthInvariant(t *testing.T) {
	doc := loadRenderMatrix(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			th := themeForName(t, c.Theme)
			out := buildComponent(t, c.Component, th, c.Width, c.Height)
			for i, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > c.Width {
					t.Errorf("line %d renders %d cells, exceeding the %d-cell width the component was sized to:\n%q",
						i, w, c.Width, line)
				}
			}
		})
	}
}

// TestRenderMatrix_Golden renders every matrix case and compares its final
// frame to a banked .golden file. Both themes render distinct truecolor
// escapes, so the golden for a dark case genuinely differs from its light
// twin; a constrained/minimum size that panicked or overflowed would fail
// here rather than silently producing garbage. Regenerate with:
//
//	go test ./internal/tui/kit/ -run TestRenderMatrix_Golden -update
func TestRenderMatrix_Golden(t *testing.T) {
	doc := loadRenderMatrix(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			th := themeForName(t, c.Theme)
			out := buildComponent(t, c.Component, th, c.Width, c.Height)
			golden.RequireEqual(t, []byte(out))
		})
	}
}
