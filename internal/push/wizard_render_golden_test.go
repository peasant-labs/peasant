package push

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// The push wizard renders four pages plus a help overlay. Every one of them is
// captured here as a whole screen, in both palettes, at the two regions the
// visual review uses. A golden byte-diff alone would prove only that the screen
// did not change, so each captured shape also names what it must and must not
// show.

//go:embed testdata/wizard_render.yaml
var wizardRenderData []byte

const (
	expectedWizardRenderCaseCount      = 15
	expectedWizardRenderAssertionCount = 8
)

// wizardRenderState is the closed set of screens the goldens capture.
type wizardRenderState string

const (
	wizardRenderStart          wizardRenderState = "start"
	wizardRenderSelection      wizardRenderState = "selection"
	wizardRenderSessionPreview wizardRenderState = "session-preview"
	wizardRenderEmptyPreview   wizardRenderState = "empty-preview"
	wizardRenderConsent        wizardRenderState = "consent"
	// wizardRenderConsentScrolled captures the consent page scrolled to its
	// last line. The consent copy (config.ProjectIdentitySentence plus the
	// existing redaction hedge) no longer fits an 80x24 viewport in one
	// screen alongside the closing "deselect it" guidance, so that guidance
	// is verified reachable via scroll here instead of unreachably required
	// of the top-of-page capture.
	wizardRenderConsentScrolled wizardRenderState = "consent-scrolled"
	wizardRenderReceipt         wizardRenderState = "receipt"
	wizardRenderHelp            wizardRenderState = "help"
)

func (s wizardRenderState) valid() bool {
	switch s {
	case wizardRenderStart, wizardRenderSelection, wizardRenderSessionPreview,
		wizardRenderEmptyPreview, wizardRenderConsent, wizardRenderConsentScrolled,
		wizardRenderReceipt, wizardRenderHelp:
		return true
	default:
		return false
	}
}

// wizardRenderTheme names the palette a case renders with.
type wizardRenderTheme string

const (
	wizardRenderDark  wizardRenderTheme = "dark"
	wizardRenderLight wizardRenderTheme = "light"
)

func (t wizardRenderTheme) valid() bool {
	return t == wizardRenderDark || t == wizardRenderLight
}

func (t wizardRenderTheme) mode() theme.Mode {
	if t == wizardRenderLight {
		return theme.ModeLight
	}
	return theme.ModeDark
}

// wizardRenderCase is one captured screen: the state the wizard is driven into,
// the palette, and the region it renders at.
type wizardRenderCase struct {
	Name   string            `yaml:"name"`
	State  wizardRenderState `yaml:"state"`
	Theme  wizardRenderTheme `yaml:"theme"`
	Width  int               `yaml:"width"`
	Height int               `yaml:"height"`
}

// wizardRenderAssertionRow names what one captured state must and must not show.
type wizardRenderAssertionRow struct {
	State        wizardRenderState `yaml:"state"`
	WantContains []string          `yaml:"wantContains"`
	WantMissing  []string          `yaml:"wantMissing"`
}

type wizardRenderDoc struct {
	ExpectedCaseCount      int                        `yaml:"expectedCaseCount"`
	ExpectedAssertionCount int                        `yaml:"expectedAssertionCount"`
	Cases                  []wizardRenderCase         `yaml:"cases"`
	Assertions             []wizardRenderAssertionRow `yaml:"assertions"`
}

func decodeWizardRender(data []byte) (wizardRenderDoc, error) {
	var doc wizardRenderDoc
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/wizard_render.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("wizard_render.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != expectedWizardRenderCaseCount || len(doc.Cases) != expectedWizardRenderCaseCount {
		return doc, fmt.Errorf("wizard render cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedWizardRenderCaseCount)
	}
	if doc.ExpectedAssertionCount != expectedWizardRenderAssertionCount ||
		len(doc.Assertions) != expectedWizardRenderAssertionCount {
		return doc, fmt.Errorf("wizard render assertions: declared=%d actual=%d required=%d",
			doc.ExpectedAssertionCount, len(doc.Assertions), expectedWizardRenderAssertionCount)
	}
	names := make(map[string]bool, len(doc.Cases))
	states := make(map[wizardRenderState]int, len(doc.Cases))
	for _, c := range doc.Cases {
		want := fmt.Sprintf("%s-%s-%dx%d", c.State, c.Theme, c.Width, c.Height)
		if c.Name != want || names[c.Name] || !c.State.valid() || !c.Theme.valid() || c.Width <= 0 || c.Height <= 0 {
			return doc, fmt.Errorf("wizard render case is invalid or duplicated: %#v", c)
		}
		names[c.Name] = true
		states[c.State]++
	}
	seen := make(map[wizardRenderState]bool, len(doc.Assertions))
	for _, row := range doc.Assertions {
		if !row.State.valid() || seen[row.State] || states[row.State] == 0 ||
			len(row.WantContains)+len(row.WantMissing) == 0 {
			return doc, fmt.Errorf("wizard render assertion is invalid, duplicated, or assertion-free: %#v", row)
		}
		for _, value := range append(append([]string{}, row.WantContains...), row.WantMissing...) {
			if strings.TrimSpace(value) == "" {
				return doc, fmt.Errorf("wizard render assertion for %q holds an empty value", row.State)
			}
		}
		seen[row.State] = true
	}
	// Every captured state must name what it shows. A state verified by the
	// byte-diff alone tells a reviewer nothing about what broke.
	for state := range states {
		if !seen[state] {
			return doc, fmt.Errorf("wizard render state %q has no text assertion", state)
		}
	}
	return doc, nil
}

func loadWizardRenderDoc(t *testing.T) wizardRenderDoc {
	t.Helper()
	doc, err := decodeWizardRender(wizardRenderData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// buildWizardScreen drives the real model through the real Update loop into one
// state and returns it ready to render.
func buildWizardScreen(t *testing.T, c wizardRenderCase) PushWizardModel {
	t.Helper()
	m := NewPushWizard(theme.New(c.Theme.mode()), testSessions(), testPublishedTurns())
	updated, _ := m.Update(windowSize(c.Width, c.Height))
	m = updated.(PushWizardModel)
	switch c.State {
	case wizardRenderStart:
		return m
	case wizardRenderSelection:
		return acceptStart(m)
	case wizardRenderSessionPreview:
		return pressKey(acceptStart(m), keyDown())
	case wizardRenderEmptyPreview:
		// Five rows down is the last session in the forest, which the fixture
		// store holds no transcript for.
		m = acceptStart(m)
		for i := 0; i < 5; i++ {
			m = pressKey(m, keyDown())
		}
		return m
	case wizardRenderConsent:
		return pressKey(acceptStart(m), keyEnter())
	case wizardRenderConsentScrolled:
		// keyRune('G') is the footer's "shift+g: go to bottom" action.
		return pressKey(pressKey(acceptStart(m), keyEnter()), keyRune('G'))
	case wizardRenderReceipt:
		return pressKey(pressKey(acceptStart(m), keyEnter()), keyEnter())
	case wizardRenderHelp:
		return pressKey(acceptStart(m), keyRune('?'))
	default:
		t.Fatalf("unknown wizard render state %q", c.State)
		return m
	}
}

// TestPushWizard_RenderGolden captures every page of the wizard, so the frame,
// the tree, the preview, the consent copy, the receipt, and the help card are
// all visible in the test artifact.
func TestPushWizard_RenderGolden(t *testing.T) {
	doc := loadWizardRenderDoc(t)
	assertions := make(map[wizardRenderState]wizardRenderAssertionRow, len(doc.Assertions))
	for _, row := range doc.Assertions {
		assertions[row.State] = row
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			view := buildWizardScreen(t, c).viewString()
			screen := strings.Join(strings.Fields(ansi.Strip(view)), " ")
			row := assertions[c.State]
			for _, want := range row.WantContains {
				if !strings.Contains(screen, want) {
					t.Errorf("the %s screen must show %q; got:\n%s", c.State, want, screen)
				}
			}
			for _, forbidden := range row.WantMissing {
				if strings.Contains(screen, forbidden) {
					t.Errorf("the %s screen must not show %q; got:\n%s", c.State, forbidden, screen)
				}
			}
			golden.RequireEqual(t, []byte(view))
		})
	}
}

// TestPushWizard_RenderSizeInvariant proves the goldens cannot bake in an
// overflow: every captured screen is exactly the height it was sized to, and no
// line exceeds its width.
func TestPushWizard_RenderSizeInvariant(t *testing.T) {
	doc := loadWizardRenderDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			view := buildWizardScreen(t, c).viewString()
			lines := strings.Split(view, "\n")
			if len(lines) != c.Height {
				t.Errorf("rendered %d lines, want exactly %d", len(lines), c.Height)
			}
			for i, line := range lines {
				if got := lipgloss.Width(line); got > c.Width {
					t.Errorf("line %d is %d cells, over width %d: %q", i, got, c.Width, ansi.Strip(line))
				}
			}
		})
	}
}
