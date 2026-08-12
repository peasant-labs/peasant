package kickstart_test

import (
	"fmt"
	"image/color"
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// TestSelectionStep_PreviewRendersMarkdownAndShowsFocus is what makes the
// selection-step goldens non-vacuous for this pane. A golden records whatever
// the step draws, including a pane that quietly stopped rendering markdown or
// stopped saying which side takes input; these assertions name both, per
// captured state, from the same fixture the goldens are built from.
func TestSelectionStep_PreviewRendersMarkdownAndShowsFocus(t *testing.T) {
	t.Parallel()
	doc := loadSelectionRenderDoc(t)

	for _, row := range doc.PreviewAssertions.Rows {
		t.Run(row.Case, func(t *testing.T) {
			t.Parallel()
			c := renderCaseNamed(t, doc, row.Case)
			th := theme.New(renderThemeFor(t, c.Theme))
			view := buildSelectionStep(t, doc, c).View()

			visible := stripRender(view)
			for _, want := range row.WantVisible {
				if !strings.Contains(visible, want) {
					t.Errorf("preview must show %q; screen:\n%s", want, visible)
				}
			}
			for _, missing := range row.WantMissing {
				if strings.Contains(visible, missing) {
					t.Errorf("preview must not show %q; screen:\n%s", missing, visible)
				}
			}
			for _, run := range row.WantColored {
				pattern := paletteRunPattern(t, th, run.Token, run.Text)
				if !pattern.MatchString(view) {
					t.Errorf("preview must carry %q styled with the %s token (pattern %s); screen:\n%s",
						run.Text, run.Token, pattern, visible)
				}
			}
			assertFocusMarker(t, th, view, row.WantFocusMarker)
		})
	}
}

// assertFocusMarker checks the divider's focus marker points the declared way -
// and, just as importantly, that it does NOT point the other way, so a pane
// that drew both markers (or never moved the one it draws) fails.
func assertFocusMarker(t *testing.T, th theme.Theme, view, want string) {
	t.Helper()
	markers := map[string]string{"left": "<", "right": ">"}
	glyph, ok := markers[want]
	if !ok {
		t.Fatalf("fixture wants focus marker %q, which is neither %q nor %q", want, "left", "right")
	}
	focus := lipgloss.NewStyle().Foreground(th.Color(th.Palette.FocusRing))
	if !strings.Contains(view, focus.Render(glyph)) {
		t.Errorf("the divider does not point %s; screen:\n%s", want, stripRender(view))
	}
	for name, other := range markers {
		if name == want {
			continue
		}
		if strings.Contains(view, focus.Render(other)) {
			t.Errorf("the divider also points %s while focus is %s; screen:\n%s", name, want, stripRender(view))
		}
	}
}

// renderCaseNamed looks a capture case up by name, failing closed so an
// assertion row can never silently describe a state nothing renders.
func renderCaseNamed(t *testing.T, doc selectionRenderDoc, name string) selectionRenderCase {
	t.Helper()
	for _, c := range doc.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("preview assertion names capture case %q, which the fixture does not declare", name)
	return selectionRenderCase{}
}

// paletteRunPattern matches text carried by a style run set to a NAMED palette
// token's color, tolerating whatever other attributes travel with it. The token
// name comes from the fixture and is resolved against the same theme.Palette
// the renderer draws from, so the expectation is a decision the fixture made,
// not whatever the code happened to emit.
func paletteRunPattern(t *testing.T, th theme.Theme, token, text string) *regexp.Regexp {
	t.Helper()
	if text == "" {
		t.Fatal("a colored-run assertion declares no text; an empty needle always matches")
	}
	r, g, b, _ := previewPaletteColor(t, th, token).RGBA()
	fg := fmt.Sprintf("38;2;%d;%d;%d", uint8(r>>8), uint8(g>>8), uint8(b>>8))
	return regexp.MustCompile(`\x1b\[[0-9;]*` + regexp.QuoteMeta(fg) + `[0-9;]*m` + regexp.QuoteMeta(text))
}

// previewPaletteColor resolves a fixture's palette token name, failing closed on
// one this test cannot map.
func previewPaletteColor(t *testing.T, th theme.Theme, token string) color.Color {
	t.Helper()
	switch token {
	case "ink":
		return th.Color(th.Palette.Ink)
	case "ink-3":
		return th.Color(th.Palette.Ink3)
	case "mauve":
		return th.Color(th.Palette.Mauve)
	case "teal":
		return th.Color(th.Palette.Teal)
	case "olive":
		return th.Color(th.Palette.Olive)
	default:
		t.Fatalf("preview assertion names palette token %q, which this test cannot resolve", token)
		return nil
	}
}
