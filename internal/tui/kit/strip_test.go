package kit_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/strip_window.yaml
var stripWindowData []byte

const (
	requiredStripCaseCount = 12
	requiredStripItemCount = 8
)

type stripWindowCase struct {
	Name      string `yaml:"name"`
	Active    int    `yaml:"active"`
	Width     int    `yaml:"width"`
	WantStart int    `yaml:"wantStart"`
	WantEnd   int    `yaml:"wantEnd"`
	WantLeft  bool   `yaml:"wantLeft"`
	WantRight bool   `yaml:"wantRight"`
}

type stripWindowDoc struct {
	ExpectedCaseCount int               `yaml:"expectedCaseCount"`
	GapWidth          int               `yaml:"gapWidth"`
	MarkerWidth       int               `yaml:"markerWidth"`
	ItemWidths        []int             `yaml:"itemWidths"`
	Cases             []stripWindowCase `yaml:"cases"`
}

func decodeStripWindowDoc(data []byte) (stripWindowDoc, error) {
	var doc stripWindowDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/strip_window.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("strip_window.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != requiredStripCaseCount || len(doc.Cases) != requiredStripCaseCount {
		return doc, fmt.Errorf("strip window cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), requiredStripCaseCount)
	}
	if len(doc.ItemWidths) != requiredStripItemCount {
		return doc, fmt.Errorf("strip window items: actual=%d required=%d", len(doc.ItemWidths), requiredStripItemCount)
	}
	if doc.GapWidth <= 0 || doc.MarkerWidth <= 0 {
		return doc, fmt.Errorf("strip window gap/marker widths must be positive: %d/%d", doc.GapWidth, doc.MarkerWidth)
	}
	names := map[string]bool{}
	for _, c := range doc.Cases {
		if strings.TrimSpace(c.Name) == "" || names[c.Name] {
			return doc, fmt.Errorf("strip window case has an empty or duplicate name %q", c.Name)
		}
		names[c.Name] = true
		if c.Width <= 0 {
			return doc, fmt.Errorf("strip window case %q has a non-positive width", c.Name)
		}
		if c.WantEnd <= c.WantStart {
			return doc, fmt.Errorf("strip window case %q wants an empty window", c.Name)
		}
	}
	return doc, nil
}

func loadStripWindowDoc(t *testing.T) stripWindowDoc {
	t.Helper()
	doc, err := decodeStripWindowDoc(stripWindowData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestStripWindowFixtureRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	mutated := append(append([]byte(nil), stripWindowData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeStripWindowDoc(mutated); err == nil {
		t.Fatal("strip window fixture accepted an unknown field")
	}
}

func TestStripWindowFixtureRejectsRemovedCase(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(stripWindowData,
		[]byte("  - {name: last-active-tiny, active: 7, width: 40, wantStart: 6, wantEnd: 8, wantLeft: true, wantRight: false}\n"),
		[]byte(""), 1)
	if bytes.Equal(mutated, stripWindowData) {
		t.Fatal("the strip window mutation did not find its target row")
	}
	if _, err := decodeStripWindowDoc(mutated); err == nil {
		t.Fatal("strip window fixture accepted a removed case row")
	}
}

// TestStripWindow_KeepsTheActiveItemVisible is the core contract: whatever the
// budget, the window always contains the active item, and the overflow flags
// report each side truthfully.
func TestStripWindow_KeepsTheActiveItemVisible(t *testing.T) {
	doc := loadStripWindowDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			view := kit.StripWindow(doc.ItemWidths, doc.GapWidth, doc.MarkerWidth, c.Active, c.Width)
			if view.Start != c.WantStart || view.End != c.WantEnd {
				t.Errorf("window = [%d,%d), want [%d,%d)", view.Start, view.End, c.WantStart, c.WantEnd)
			}
			if view.LeftOverflow != c.WantLeft || view.RightOverflow != c.WantRight {
				t.Errorf("overflow left/right = %v/%v, want %v/%v",
					view.LeftOverflow, view.RightOverflow, c.WantLeft, c.WantRight)
			}
			active := c.Active
			if active < 0 {
				active = 0
			}
			if active >= len(doc.ItemWidths) {
				active = len(doc.ItemWidths) - 1
			}
			if !view.Contains(active) {
				t.Errorf("window [%d,%d) does not contain the active item %d", view.Start, view.End, active)
			}
		})
	}
}

// TestStripWindow_FitsTheBudget proves the chosen window plus its overflow
// markers actually fits the cell budget, so the render cannot rely on
// truncation to hide a window that was too wide.
func TestStripWindow_FitsTheBudget(t *testing.T) {
	doc := loadStripWindowDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			view := kit.StripWindow(doc.ItemWidths, doc.GapWidth, doc.MarkerWidth, c.Active, c.Width)
			total := 0
			for i := view.Start; i < view.End; i++ {
				total += doc.ItemWidths[i]
			}
			total += doc.GapWidth * (view.End - view.Start - 1)
			if view.LeftOverflow {
				total += doc.MarkerWidth
			}
			if view.RightOverflow {
				total += doc.MarkerWidth
			}
			single := view.End-view.Start == 1
			if total > c.Width && !single {
				t.Errorf("window costs %d cells, over the %d budget", total, c.Width)
			}
		})
	}
}

func TestStripWindow_EmptyInputs(t *testing.T) {
	t.Parallel()
	if got := kit.StripWindow(nil, 2, 3, 0, 80); !got.Empty() {
		t.Errorf("an empty item list must yield an empty window, got %+v", got)
	}
	if got := kit.StripWindow([]int{4}, 2, 3, 0, 0); !got.Empty() {
		t.Errorf("a zero budget must yield an empty window, got %+v", got)
	}
}

// TestScrollStrip_RendersTheActiveLabelAndMarkers renders the real strip at a
// width that cannot show every tab, and proves the active label survives while
// the clipped side reports itself.
func TestScrollStrip_RendersTheActiveLabelAndMarkers(t *testing.T) {
	t.Parallel()
	th := theme.New(theme.ModeDark)
	styles := th.Styles()
	panel := kit.NewPanel(th)
	labels := []string{"choose sessions", "auto-ingest", "publication", "privacy", "license", "sharing", "review & save"}

	for active := range labels {
		items := make([]kit.StripItem, 0, len(labels))
		for i, label := range labels {
			style := panel.Style(styles.Muted)
			if i == active {
				style = panel.Style(styles.Selected)
			}
			items = append(items, kit.StripItem{Text: " " + label + " ", Style: style})
		}
		const width = 40
		rendered := kit.ScrollStrip(panel.Style(styles.Muted), items, active, width, "  ")
		plain := ansi.Strip(rendered)
		if got := lipgloss.Width(rendered); got != width {
			t.Errorf("active %d: strip is %d cells, want exactly %d", active, got, width)
		}
		if !strings.Contains(plain, labels[active]) {
			t.Errorf("active %d: the active label %q is not visible in %q", active, labels[active], plain)
		}
		if !strings.Contains(plain, labels[0]) && !strings.Contains(plain, kit.StripMarkerLeft) {
			t.Errorf("active %d: hidden earlier tabs are not marked in %q", active, plain)
		}
		if !strings.Contains(plain, labels[len(labels)-1]) && !strings.Contains(plain, kit.StripMarkerRight) {
			t.Errorf("active %d: hidden later tabs are not marked in %q", active, plain)
		}
	}
}
