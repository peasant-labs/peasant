package theme_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/color"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/mode_validity.yaml
var modeValidityFixtureData []byte

type modeValidityDocument struct {
	ExpectedCaseCount int                `yaml:"expectedCaseCount"`
	Cases             []modeValidityCase `yaml:"cases"`
}

type modeValidityCase struct {
	Name string `yaml:"name"`
	Mode string `yaml:"mode"`
	Want bool   `yaml:"want"`
}

// loadModeValidityFixture decodes and validates testdata/mode_validity.yaml,
// mirroring the embed+KnownFields+single-document+declared-count idiom
// internal/config/level_phrases_test.go establishes.
func loadModeValidityFixture(data []byte) (modeValidityDocument, error) {
	var doc modeValidityDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/mode_validity.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/mode_validity.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf(
			"testdata/mode_validity.yaml: expectedCaseCount=%d but found %d cases (and must be non-zero)",
			doc.ExpectedCaseCount, len(doc.Cases))
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			return doc, fmt.Errorf("testdata/mode_validity.yaml: case name %q is missing or duplicated", c.Name)
		}
		seen[c.Name] = true
	}
	return doc, nil
}

func TestMode_IsValid(t *testing.T) {
	t.Parallel()
	doc, err := loadModeValidityFixture(modeValidityFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			mode := theme.Mode(c.Mode)
			if got := mode.IsValid(); got != c.Want {
				t.Errorf("Mode(%q).IsValid() = %v, want %v", c.Mode, got, c.Want)
			}
		})
	}
}

func TestColorPair_For(t *testing.T) {
	t.Parallel()
	dark := colorFromHex(t, "#111111")
	light := colorFromHex(t, "#eeeeee")
	pair := theme.ColorPair{Dark: dark, Light: light}

	if got := pair.For(theme.ModeDark); got != dark {
		t.Errorf("For(ModeDark) = %v, want %v", got, dark)
	}
	if got := pair.For(theme.ModeLight); got != light {
		t.Errorf("For(ModeLight) = %v, want %v", got, light)
	}
}

// TestColorPair_For_PanicsOnInvalidMode is the mutation proof that For fails
// closed rather than silently defaulting to a side: an invalid Mode reaching
// a rendering call is a caller bug (config.Theme is validated before ever
// being converted to a theme.Mode), and a silent default would render the
// wrong colors with no signal anything was wrong.
func TestColorPair_For_PanicsOnInvalidMode(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("For(invalid Mode) did not panic; an unvalidated Mode must fail closed, not silently pick a side")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string", r)
		}
		for _, want := range []string{"invalid Mode", "ModeDark", "ModeLight"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message %q does not mention %q; it must be actionable", msg, want)
			}
		}
	}()
	pair := theme.ColorPair{Dark: colorFromHex(t, "#111111"), Light: colorFromHex(t, "#eeeeee")}
	pair.For(theme.Mode("sepia"))
}

func colorFromHex(t *testing.T, hex string) color.Color {
	t.Helper()
	return lipgloss.Color(hex)
}
