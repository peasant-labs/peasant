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

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/panel_render.yaml
var panelRenderData []byte

const (
	requiredPanelCaseCount       = 10
	requiredPanelThemeCount      = 2
	requiredPanelBackgroundCount = 2
	requiredPanelAlignCount      = 2
	requiredPanelLineCount       = 6
)

// panelRole names which theme style bundle a fixture line renders with.
type panelRole string

const (
	panelRoleBase     panelRole = "base"
	panelRoleHeader   panelRole = "header"
	panelRoleMuted    panelRole = "muted"
	panelRoleDanger   panelRole = "danger"
	panelRoleSelected panelRole = "selected"
	panelRoleSurface  panelRole = "surface"
)

func (r panelRole) valid() bool {
	switch r {
	case panelRoleBase, panelRoleHeader, panelRoleMuted, panelRoleDanger, panelRoleSelected, panelRoleSurface:
		return true
	}
	return false
}

func (r panelRole) style(styles theme.Styles) lipgloss.Style {
	switch r {
	case panelRoleHeader:
		return styles.Header
	case panelRoleMuted:
		return styles.Muted
	case panelRoleDanger:
		return styles.Danger
	case panelRoleSelected:
		return styles.Selected
	case panelRoleSurface:
		return styles.Surface
	default:
		return styles.Base
	}
}

// panelBackground names which palette token a fixture case paints behind its
// lines.
type panelBackground string

const (
	panelBackgroundCanvas  panelBackground = "canvas"
	panelBackgroundSurface panelBackground = "surface"
)

func (b panelBackground) valid() bool {
	return b == panelBackgroundCanvas || b == panelBackgroundSurface
}

func (b panelBackground) pair(t theme.Theme) theme.ColorPair {
	if b == panelBackgroundSurface {
		return t.Palette.Surface
	}
	return t.Palette.Canvas
}

type panelLineFixture struct {
	Role panelRole `yaml:"role"`
	Text string    `yaml:"text"`
}

type panelRenderCase struct {
	Name       string          `yaml:"name"`
	Theme      string          `yaml:"theme"`
	Background panelBackground `yaml:"background"`
	Align      kit.PanelAlign  `yaml:"align"`
	Width      int             `yaml:"width"`
	Height     int             `yaml:"height"`
}

type panelRenderDoc struct {
	ExpectedCaseCount       int                `yaml:"expectedCaseCount"`
	ExpectedThemeCount      int                `yaml:"expectedThemeCount"`
	ExpectedBackgroundCount int                `yaml:"expectedBackgroundCount"`
	ExpectedAlignCount      int                `yaml:"expectedAlignCount"`
	Lines                   []panelLineFixture `yaml:"lines"`
	Cases                   []panelRenderCase  `yaml:"cases"`
}

func decodePanelRenderDoc(data []byte) (panelRenderDoc, error) {
	var doc panelRenderDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/panel_render.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("panel_render.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != requiredPanelCaseCount || len(doc.Cases) != requiredPanelCaseCount {
		return doc, fmt.Errorf("panel render cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), requiredPanelCaseCount)
	}
	if len(doc.Lines) != requiredPanelLineCount {
		return doc, fmt.Errorf("panel render lines: actual=%d required=%d", len(doc.Lines), requiredPanelLineCount)
	}
	for _, line := range doc.Lines {
		if !line.Role.valid() {
			return doc, fmt.Errorf("panel render line has unknown role %q", line.Role)
		}
	}
	names := map[string]bool{}
	themes := map[string]bool{}
	backgrounds := map[panelBackground]bool{}
	aligns := map[kit.PanelAlign]bool{}
	for _, c := range doc.Cases {
		if strings.TrimSpace(c.Name) == "" || names[c.Name] {
			return doc, fmt.Errorf("panel render case has an empty or duplicate name %q", c.Name)
		}
		names[c.Name] = true
		if !c.Background.valid() {
			return doc, fmt.Errorf("panel render case %q has unknown background %q", c.Name, c.Background)
		}
		if !c.Align.IsValid() {
			return doc, fmt.Errorf("panel render case %q has unknown align %q", c.Name, c.Align)
		}
		if c.Theme != string(theme.ModeDark) && c.Theme != string(theme.ModeLight) {
			return doc, fmt.Errorf("panel render case %q has unknown theme %q", c.Name, c.Theme)
		}
		if c.Width < 0 || c.Height < 0 {
			return doc, fmt.Errorf("panel render case %q has a negative size", c.Name)
		}
		themes[c.Theme] = true
		backgrounds[c.Background] = true
		aligns[c.Align] = true
	}
	if len(themes) != doc.ExpectedThemeCount || doc.ExpectedThemeCount != requiredPanelThemeCount {
		return doc, fmt.Errorf("panel render themes: declared=%d actual=%d required=%d",
			doc.ExpectedThemeCount, len(themes), requiredPanelThemeCount)
	}
	if len(backgrounds) != doc.ExpectedBackgroundCount || doc.ExpectedBackgroundCount != requiredPanelBackgroundCount {
		return doc, fmt.Errorf("panel render backgrounds: declared=%d actual=%d required=%d",
			doc.ExpectedBackgroundCount, len(backgrounds), requiredPanelBackgroundCount)
	}
	if len(aligns) != doc.ExpectedAlignCount || doc.ExpectedAlignCount != requiredPanelAlignCount {
		return doc, fmt.Errorf("panel render alignments: declared=%d actual=%d required=%d",
			doc.ExpectedAlignCount, len(aligns), requiredPanelAlignCount)
	}
	return doc, nil
}

func loadPanelRenderDoc(t *testing.T) panelRenderDoc {
	t.Helper()
	doc, err := decodePanelRenderDoc(panelRenderData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func panelThemeFor(t *testing.T, name string) theme.Theme {
	t.Helper()
	switch theme.Mode(name) {
	case theme.ModeDark:
		return theme.New(theme.ModeDark)
	case theme.ModeLight:
		return theme.New(theme.ModeLight)
	default:
		t.Fatalf("unknown theme %q", name)
		return theme.Theme{}
	}
}

// buildPanel assembles one fixture case into a Panel.
func buildPanel(t *testing.T, doc panelRenderDoc, c panelRenderCase) kit.Panel {
	t.Helper()
	th := panelThemeFor(t, c.Theme)
	styles := th.Styles()
	panel := kit.NewPanel(th).WithBackground(c.Background.pair(th)).WithAlign(c.Align)
	panel.SetSize(c.Width, c.Height)
	for _, line := range doc.Lines {
		panel.Line(line.Role.style(styles), line.Text)
	}
	return panel
}

func TestPanelFixtureRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	mutated := append(append([]byte(nil), panelRenderData...), []byte("\nunknownField: true\n")...)
	if _, err := decodePanelRenderDoc(mutated); err == nil {
		t.Fatal("panel fixture accepted an unknown field")
	}
}

func TestPanelFixtureRejectsRemovedCase(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(panelRenderData,
		[]byte("  - {name: surface-center-light, theme: light, background: surface, align: center, width: 40, height: 0}\n"),
		[]byte(""), 1)
	if bytes.Equal(mutated, panelRenderData) {
		t.Fatal("the panel fixture mutation did not find its target row")
	}
	if _, err := decodePanelRenderDoc(mutated); err == nil {
		t.Fatal("panel fixture accepted a removed case row")
	}
}

func TestPanelFixtureRejectsUnknownAlign(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(panelRenderData, []byte("align: center, width: 40"), []byte("align: justify, width: 40"), 1)
	if _, err := decodePanelRenderDoc(mutated); err == nil {
		t.Fatal("panel fixture accepted an unknown alignment")
	}
}

// TestPanel_RenderGolden captures the rendered panel for every fixture case,
// so a change to the background painting or the line fitting is visible as a
// screen diff rather than as a silent color shift.
func TestPanel_RenderGolden(t *testing.T) {
	doc := loadPanelRenderDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			panel := buildPanel(t, doc, c)
			golden.RequireEqual(t, []byte(panel.View()))
		})
	}
}

// TestPanel_EveryLineFillsTheBox is the invariant the whole primitive exists
// for: EVERY line of the block is exactly the same number of cells wide. A
// ragged background is exactly the failure of this assertion.
func TestPanel_EveryLineFillsTheBox(t *testing.T) {
	doc := loadPanelRenderDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			panel := buildPanel(t, doc, c)
			want := panel.ContentWidth()
			if want <= 0 {
				t.Fatalf("panel measured a non-positive content width %d", want)
			}
			lines := strings.Split(panel.View(), "\n")
			if len(lines) != panel.LineCount() {
				t.Fatalf("panel rendered %d lines, want %d", len(lines), panel.LineCount())
			}
			for i, line := range lines {
				if got := lipgloss.Width(line); got != want {
					t.Errorf("line %d is %d cells, want exactly %d: %q", i, got, want, line)
				}
			}
		})
	}
}

// TestPanel_StyleKeepsARoleThatOwnsItsBackground proves the panel repaints
// only foreground-only roles. Selected is a deliberate amber fill; repainting
// it with the panel background would erase the highlight the role exists for.
func TestPanel_StyleKeepsARoleThatOwnsItsBackground(t *testing.T) {
	t.Parallel()
	th := theme.New(theme.ModeDark)
	styles := th.Styles()
	panel := kit.NewPanel(th)
	if got := panel.Style(styles.Selected).GetBackground(); got != styles.Selected.GetBackground() {
		t.Errorf("panel.Style repainted Selected's own background: got %v, want %v",
			got, styles.Selected.GetBackground())
	}
	if got := panel.Style(styles.Muted).GetBackground(); got != th.Color(th.Palette.Canvas) {
		t.Errorf("panel.Style did not paint the panel background onto Muted: got %v, want the canvas token", got)
	}
}

// TestPanel_HeightPadsWithPaintedLines proves a panel sized taller than its
// content fills the remaining rows instead of leaving them on the terminal's
// own background.
func TestPanel_HeightPadsWithPaintedLines(t *testing.T) {
	t.Parallel()
	th := theme.New(theme.ModeLight)
	panel := kit.NewPanel(th)
	panel.SetSize(12, 4)
	panel.Text("one")
	lines := strings.Split(panel.View(), "\n")
	if len(lines) != 4 {
		t.Fatalf("panel rendered %d lines, want 4", len(lines))
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got != 12 {
			t.Errorf("padded line %d is %d cells, want 12", i, got)
		}
	}
}

// TestPanel_EveryCellCarriesABackground is the anti-ragged assertion. A line
// that is the correct width can still leave its pad cells on the terminal's
// own background, which is the defect the panel exists to remove. This test
// walks the real escape sequences and fails on any printable cell that no
// background covers.
func TestPanel_EveryCellCarriesABackground(t *testing.T) {
	doc := loadPanelRenderDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			panel := buildPanel(t, doc, c)
			for i, line := range strings.Split(panel.View(), "\n") {
				if got := testutil.UnpaintedCells(line); got != 0 {
					t.Errorf("line %d leaves %d cell(s) unpainted: %q", i, got, line)
				}
			}
		})
	}
}

// TestUnpaintedCells_ReportsAForegroundOnlyLine proves the assertion above can
// fail. A bare foreground-only style is the exact input the panel replaces,
// and it must count as unpainted.
func TestUnpaintedCells_ReportsAForegroundOnlyLine(t *testing.T) {
	t.Parallel()
	styles := theme.New(theme.ModeDark).Styles()
	line := styles.Muted.Render("ragged")
	if got := testutil.UnpaintedCells(line); got != 6 {
		t.Errorf("a foreground-only line of 6 cells counted %d unpainted cells: %q", got, line)
	}
}
