package settings

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

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/step_tabs.yaml
var stepTabsData []byte

const (
	requiredStepTabsStepCount  = 8
	requiredStepTabsWidthCount = 3
	stepTabsReceiptTitle       = "review & save"
	stepTabsGoldenWidth        = 60
	stepTabsGoldenHeight       = 12
)

type stepTabsDoc struct {
	ExpectedStepCount  int      `yaml:"expectedStepCount"`
	ExpectedWidthCount int      `yaml:"expectedWidthCount"`
	Titles             []string `yaml:"titles"`
	Widths             []int    `yaml:"widths"`
}

func decodeStepTabsDoc(data []byte) (stepTabsDoc, error) {
	var doc stepTabsDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/step_tabs.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("step_tabs.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedStepCount != requiredStepTabsStepCount || len(doc.Titles) != requiredStepTabsStepCount {
		return doc, fmt.Errorf("step tab titles: declared=%d actual=%d required=%d",
			doc.ExpectedStepCount, len(doc.Titles), requiredStepTabsStepCount)
	}
	if doc.ExpectedWidthCount != requiredStepTabsWidthCount || len(doc.Widths) != requiredStepTabsWidthCount {
		return doc, fmt.Errorf("step tab widths: declared=%d actual=%d required=%d",
			doc.ExpectedWidthCount, len(doc.Widths), requiredStepTabsWidthCount)
	}
	seen := map[string]bool{}
	for _, title := range doc.Titles {
		if strings.TrimSpace(title) == "" || seen[title] {
			return doc, fmt.Errorf("step tab title %q is empty or duplicated", title)
		}
		seen[title] = true
	}
	for _, width := range doc.Widths {
		if width <= 0 {
			return doc, fmt.Errorf("step tab width %d is not positive", width)
		}
	}
	return doc, nil
}

func loadStepTabsDoc(t *testing.T) stepTabsDoc {
	t.Helper()
	doc, err := decodeStepTabsDoc(stepTabsData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// fullStepRegistry builds the whole multi-section flow named by the fixture.
// The one-section guided fixture cannot overflow its tab strip, so the
// overflow behavior needs the full registry.
func fullStepRegistry(doc stepTabsDoc) Registry {
	sections := make([]Section, 0, len(doc.Titles))
	for i, title := range doc.Titles {
		sections = append(sections, Section{
			Key:    fmt.Sprintf("section-%d", i),
			Title:  title,
			Fields: []Field{Toggle(fmt.Sprintf("toggle-%d", i), title, connectedAccessor())},
		})
	}
	return Registry{Sections: sections}
}

func newFullStepFlow(t *testing.T, th theme.Theme, doc stepTabsDoc, width, height int) Flow {
	t.Helper()
	d, err := NewDraft("/tmp/settings-step-tabs/config.yaml", config.BaseConfig())
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	f := NewFlow(th, fullStepRegistry(doc), d)
	f.SetSize(width, height)
	return f
}

// stepTabsLine returns the plain text of the flow's rendered tab strip.
func stepTabsLine(f Flow, width int) string {
	return ansi.Strip(f.stepTabs(width))
}

func TestStepTabsFixtureRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	mutated := append(append([]byte(nil), stepTabsData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeStepTabsDoc(mutated); err == nil {
		t.Fatal("step tabs fixture accepted an unknown field")
	}
}

func TestStepTabsFixtureRejectsRemovedTitle(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(stepTabsData, []byte("  - claude retention\n"), []byte(""), 1)
	if bytes.Equal(mutated, stepTabsData) {
		t.Fatal("the step tabs mutation did not find its target title")
	}
	if _, err := decodeStepTabsDoc(mutated); err == nil {
		t.Fatal("step tabs fixture accepted a removed title")
	}
}

// TestStepTabs_ActiveTabStaysVisibleAtEveryWidth walks every step of the FULL
// flow at each narrow width and proves the active tab's whole label is on
// screen. Before the strip scrolled, the leading tabs consumed the width and
// the active tab was clipped off the right edge.
func TestStepTabs_ActiveTabStaysVisibleAtEveryWidth(t *testing.T) {
	doc := loadStepTabsDoc(t)
	th := theme.New(theme.ModeDark)
	labels := append(append([]string{}, doc.Titles...), stepTabsReceiptTitle)
	for _, width := range doc.Widths {
		for step := range labels {
			name := fmt.Sprintf("width-%d-step-%d", width, step)
			t.Run(name, func(t *testing.T) {
				f := newFullStepFlow(t, th, doc, width, 20)
				for i := 0; i < step; i++ {
					f = send(f, "tab")
				}
				if f.cur != step {
					t.Fatalf("flow is on step %d, want %d", f.cur, step)
				}
				strip := stepTabsLine(f, width)
				if got := lipgloss.Width(strip); got != width {
					t.Errorf("strip is %d cells, want exactly %d: %q", got, width, strip)
				}
				if !strings.Contains(strip, labels[step]) {
					t.Errorf("the active tab %q is not visible at width %d: %q", labels[step], width, strip)
				}
			})
		}
	}
}

// TestStepTabs_MarksHiddenTabs proves a clipped side reports itself, so a cut
// reads as "more steps this way" instead of a random truncation.
func TestStepTabs_MarksHiddenTabs(t *testing.T) {
	doc := loadStepTabsDoc(t)
	th := theme.New(theme.ModeLight)
	labels := append(append([]string{}, doc.Titles...), stepTabsReceiptTitle)
	for _, width := range doc.Widths {
		for step := range labels {
			name := fmt.Sprintf("width-%d-step-%d", width, step)
			t.Run(name, func(t *testing.T) {
				f := newFullStepFlow(t, th, doc, width, 20)
				for i := 0; i < step; i++ {
					f = send(f, "tab")
				}
				strip := stepTabsLine(f, width)
				if !strings.Contains(strip, labels[0]) && !strings.Contains(strip, kit.StripMarkerLeft) {
					t.Errorf("earlier hidden tabs are not marked at width %d, step %d: %q", width, step, strip)
				}
				last := labels[len(labels)-1]
				if !strings.Contains(strip, last) && !strings.Contains(strip, kit.StripMarkerRight) {
					t.Errorf("later hidden tabs are not marked at width %d, step %d: %q", width, step, strip)
				}
			})
		}
	}
}

// TestStepTabs_RenderGolden captures the mounted flow at a narrow width for
// the first, a middle, and the final step, in both themes, so the scrolled
// strip is reviewable as a rendered screen.
func TestStepTabs_RenderGolden(t *testing.T) {
	doc := loadStepTabsDoc(t)
	steps := []int{0, 4, len(doc.Titles)}
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
		for _, step := range steps {
			name := fmt.Sprintf("%s-step-%d", mode, step)
			t.Run(name, func(t *testing.T) {
				f := newFullStepFlow(t, theme.New(mode), doc, stepTabsGoldenWidth, stepTabsGoldenHeight)
				for i := 0; i < step; i++ {
					f = send(f, "tab")
				}
				golden.RequireEqual(t, []byte(f.View()))
			})
		}
	}
}

// TestStepTabs_PaintsEveryCell proves the mounted strip leaves no cell on the
// terminal's own background. The gap cells, the overflow markers, and the
// trailing pad all carry the surface background, so the strip reads as one
// row instead of separate boxes around each label.
func TestStepTabs_PaintsEveryCell(t *testing.T) {
	doc := loadStepTabsDoc(t)
	labels := append(append([]string{}, doc.Titles...), stepTabsReceiptTitle)
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
		for _, width := range doc.Widths {
			for step := range labels {
				name := fmt.Sprintf("%s-width-%d-step-%d", mode, width, step)
				t.Run(name, func(t *testing.T) {
					f := newFullStepFlow(t, theme.New(mode), doc, width, 20)
					for i := 0; i < step; i++ {
						f = send(f, "tab")
					}
					strip := f.stepTabs(width)
					if got := testutil.UnpaintedCells(strip); got != 0 {
						t.Errorf("the step tab strip leaves %d cell(s) unpainted: %q", got, strip)
					}
				})
			}
		}
	}
}
