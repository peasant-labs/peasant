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

const (
	requiredFitLineCaseCount    = 6
	requiredFitLineTailCount    = 4
	requiredFitLineThemeCount   = 2
	requiredWideTailFixtureName = "keeps-wide-suffix-when-ellipsis-underfills"
)

type fitLineTheme string

const (
	fitLineThemeDark  fitLineTheme = "dark"
	fitLineThemeLight fitLineTheme = "light"
)

func (value fitLineTheme) mode() (theme.Mode, bool) {
	switch value {
	case fitLineThemeDark:
		return theme.ModeDark, true
	case fitLineThemeLight:
		return theme.ModeLight, true
	default:
		return theme.Mode(""), false
	}
}

type fitLineCase struct {
	Name   string `yaml:"name"`
	Text   string `yaml:"text"`
	Width  int    `yaml:"width"`
	Fitted string `yaml:"fitted"`
}

type fitLineDocument struct {
	ExpectedCaseCount  int            `yaml:"expectedCaseCount"`
	ExpectedTailCount  int            `yaml:"expectedTailCount"`
	ExpectedThemeCount int            `yaml:"expectedThemeCount"`
	Themes             []fitLineTheme `yaml:"themes"`
	Cases              []fitLineCase  `yaml:"cases"`
	TailCases          []fitLineCase  `yaml:"tailCases"`
}

//go:embed testdata/fit_line.yaml
var fitLineFixtureData []byte

func decodeFitLineDocument(data []byte) (fitLineDocument, error) {
	var document fitLineDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/fit_line.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("testdata/fit_line.yaml must contain exactly one document: %w", err)
	}
	if document.ExpectedCaseCount != requiredFitLineCaseCount || len(document.Cases) != requiredFitLineCaseCount {
		return document, fmt.Errorf("fit-line rows: declared=%d actual=%d required=%d",
			document.ExpectedCaseCount, len(document.Cases), requiredFitLineCaseCount)
	}
	if document.ExpectedTailCount != requiredFitLineTailCount || len(document.TailCases) != requiredFitLineTailCount {
		return document, fmt.Errorf("fit-line tail rows: declared=%d actual=%d required=%d",
			document.ExpectedTailCount, len(document.TailCases), requiredFitLineTailCount)
	}
	if document.ExpectedThemeCount != requiredFitLineThemeCount || len(document.Themes) != requiredFitLineThemeCount {
		return document, fmt.Errorf("fit-line themes: declared=%d actual=%d required=%d",
			document.ExpectedThemeCount, len(document.Themes), requiredFitLineThemeCount)
	}
	themes := map[fitLineTheme]bool{}
	for _, value := range document.Themes {
		if _, ok := value.mode(); !ok || themes[value] {
			return document, fmt.Errorf("fit-line theme %q is unknown or duplicated", value)
		}
		themes[value] = true
	}
	names := map[string]bool{}
	for _, row := range append(append([]fitLineCase(nil), document.Cases...), document.TailCases...) {
		if row.Name == "" || names[row.Name] {
			return document, fmt.Errorf("fit-line row name %q is empty or duplicated", row.Name)
		}
		names[row.Name] = true
		if row.Width > 0 && ansi.StringWidth(row.Fitted) != row.Width {
			return document, fmt.Errorf("fit-line row %q fitted width=%d, want %d", row.Name, ansi.StringWidth(row.Fitted), row.Width)
		}
		if row.Width <= 0 && row.Fitted != "" {
			return document, fmt.Errorf("fit-line row %q has non-empty output at width %d", row.Name, row.Width)
		}
	}
	if !names[requiredWideTailFixtureName] {
		return document, fmt.Errorf("fit-line fixture is missing required tail row %q", requiredWideTailFixtureName)
	}
	return document, nil
}

func loadFitLineDocument(t *testing.T) fitLineDocument {
	t.Helper()
	document, err := decodeFitLineDocument(fitLineFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func mutateFitLineFixture(t *testing.T, data, old, replacement []byte) []byte {
	t.Helper()
	if count := bytes.Count(data, old); count != 1 {
		t.Fatalf("fit-line mutation source %q occurs %d times, want exactly one", old, count)
	}
	return bytes.Replace(data, old, replacement, 1)
}

func TestFitLineAppliesStyleAfterCellFitting(t *testing.T) {
	document := loadFitLineDocument(t)
	for _, renderTheme := range document.Themes {
		renderTheme := renderTheme
		t.Run(string(renderTheme), func(t *testing.T) {
			mode, ok := renderTheme.mode()
			if !ok {
				t.Fatalf("unknown fit-line theme %q", renderTheme)
			}
			style := theme.New(mode).Styles().Surface
			for _, row := range document.Cases {
				row := row
				t.Run(row.Name, func(t *testing.T) {
					got := kit.FitLine(style, row.Text, row.Width)
					want := ""
					if row.Width > 0 {
						want = style.Render(row.Fitted)
					}
					if got != want {
						t.Errorf("FitLine() = %q, want one style application over fitted text %q", got, want)
					}
					if width := lipgloss.Width(got); width != max(row.Width, 0) {
						t.Errorf("FitLine() width=%d, want %d", width, max(row.Width, 0))
					}
				})
			}
		})
	}
}

func TestFitLineTailKeepsLatestGraphemesAndCaret(t *testing.T) {
	document := loadFitLineDocument(t)
	for _, renderTheme := range document.Themes {
		renderTheme := renderTheme
		t.Run(string(renderTheme), func(t *testing.T) {
			mode, ok := renderTheme.mode()
			if !ok {
				t.Fatalf("unknown fit-line theme %q", renderTheme)
			}
			style := theme.New(mode).Styles().Surface
			for _, row := range document.TailCases {
				row := row
				t.Run(row.Name, func(t *testing.T) {
					got := kit.FitLineTail(style, row.Text, row.Width)
					want := ""
					if row.Width > 0 {
						want = style.Render(row.Fitted)
					}
					if got != want {
						t.Errorf("FitLineTail() = %q, want one style application over fitted text %q", got, want)
					}
					if width := ansi.StringWidth(got); width != max(row.Width, 0) {
						t.Errorf("FitLineTail() width=%d, want %d", width, max(row.Width, 0))
					}
				})
			}
		})
	}
}

func TestFitLineFixtureRejectsCoordinatedRowRemoval(t *testing.T) {
	mutated := mutateFitLineFixture(t, fitLineFixtureData, []byte("expectedCaseCount: 6"), []byte("expectedCaseCount: 5"))
	mutated = mutateFitLineFixture(t, mutated, []byte("  - name: non-positive-width-is-empty\n    text: ignored\n    width: 0\n    fitted: ''\n"), nil)
	if _, err := decodeFitLineDocument(mutated); err == nil {
		t.Fatal("fit-line fixture accepted a row removal coordinated with its declared count")
	}
}

func TestFitLineFixtureRejectsCoordinatedWideTailRemoval(t *testing.T) {
	mutated := mutateFitLineFixture(t, fitLineFixtureData, []byte("expectedTailCount: 4"), []byte("expectedTailCount: 3"))
	mutated = mutateFitLineFixture(t, mutated, []byte("  - name: keeps-wide-suffix-when-ellipsis-underfills\n    text: 'search: x界▏'\n    width: 3\n    fitted: '界▏'\n"), nil)
	if _, err := decodeFitLineDocument(mutated); err == nil {
		t.Fatal("fit-line fixture accepted removal of the wide-suffix regression row coordinated with its declared count")
	}
}

func TestFitLineFixtureRejectsCoordinatedThemeRemoval(t *testing.T) {
	mutated := mutateFitLineFixture(t, fitLineFixtureData, []byte("expectedThemeCount: 2"), []byte("expectedThemeCount: 1"))
	mutated = mutateFitLineFixture(t, mutated, []byte("themes: [dark, light]"), []byte("themes: [dark]"))
	if _, err := decodeFitLineDocument(mutated); err == nil {
		t.Fatal("fit-line fixture accepted a theme removal coordinated with its declared count")
	}
}

func TestFitLineFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), fitLineFixtureData...), []byte("\nunknownField: true\n")...)
	if bytes.Equal(mutated, fitLineFixtureData) {
		t.Fatal("fit-line unknown-field mutation did not alter the fixture")
	}
	if _, err := decodeFitLineDocument(mutated); err == nil {
		t.Fatal("fit-line fixture accepted an unknown field")
	}
}

func TestFitLineFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), fitLineFixtureData...), []byte("\n---\n{}\n")...)
	if bytes.Equal(mutated, fitLineFixtureData) {
		t.Fatal("fit-line trailing-document mutation did not alter the fixture")
	}
	if _, err := decodeFitLineDocument(mutated); err == nil {
		t.Fatal("fit-line fixture accepted a trailing document")
	}
}

func TestFitLineFixtureRejectsANSIWidthMutation(t *testing.T) {
	mutated := mutateFitLineFixture(t, fitLineFixtureData, []byte("fitted: '界a  '"), []byte("fitted: '界a '"))
	if _, err := decodeFitLineDocument(mutated); err == nil {
		t.Fatal("fit-line fixture accepted a wide-cell expectation with the wrong display width")
	}
}

func TestFrameRestoresCanvasBehindTransparentStyledRows(t *testing.T) {
	document := loadFitLineDocument(t)
	for _, renderTheme := range document.Themes {
		renderTheme := renderTheme
		t.Run(string(renderTheme), func(t *testing.T) {
			mode, ok := renderTheme.mode()
			if !ok {
				t.Fatalf("unknown fit-line theme %q", renderTheme)
			}
			th := theme.New(mode)
			styles := th.Styles()
			frame := kit.NewFrame(th)
			frame.SetSize(12, 4)
			frame.SetContent(styles.Muted.Render("hint"))
			lines := strings.Split(frame.View(), "\n")
			if len(lines) != 4 {
				t.Fatalf("Frame rendered %d lines, want 4", len(lines))
			}

			baseProbe := styles.Base.Render("x")
			baseAt := strings.Index(baseProbe, "x")
			basePrefix, baseSuffix := baseProbe[:baseAt], baseProbe[baseAt+1:]
			muted := strings.TrimSuffix(styles.Muted.Render("hint"), baseSuffix)
			wantRun := basePrefix + muted + strings.Repeat(" ", 6) + baseSuffix
			if !strings.Contains(lines[1], wantRun) {
				t.Errorf("Frame did not restore its Canvas style after the muted row reset through the final cell: %q", lines[1])
			}
			if surfaceProbe := styles.Surface.Render("hint"); strings.Contains(lines[1], surfaceProbe) {
				t.Errorf("Frame changed a transparent muted row into a Surface row")
			}
		})
	}
}

func TestFrameRestoresCanvasAfterEveryPrestyledFooterReset(t *testing.T) {
	document := loadFitLineDocument(t)
	for _, renderTheme := range document.Themes {
		renderTheme := renderTheme
		t.Run(string(renderTheme), func(t *testing.T) {
			mode, ok := renderTheme.mode()
			if !ok {
				t.Fatalf("unknown fit-line theme %q", renderTheme)
			}
			th := theme.New(mode)
			styles := th.Styles()
			canvas := th.Color(th.Palette.Canvas)
			parent := styles.Muted.Background(canvas)
			footerKey := styles.Header.Render("ctrl+s")
			footerDescription := styles.Muted.Render(": save")
			frame := kit.NewFrame(th).WithFooter(footerKey + footerDescription)
			frame.SetSize(24, 4)
			lines := strings.Split(frame.View(), "\n")
			if len(lines) != 4 {
				t.Fatalf("Frame rendered %d lines, want 4", len(lines))
			}

			parentProbe := parent.Render("x")
			parentAt := strings.Index(parentProbe, "x")
			if parentAt < 0 {
				t.Fatalf("parent style probe does not contain its marker: %q", parentProbe)
			}
			parentPrefix, parentSuffix := parentProbe[:parentAt], parentProbe[parentAt+1:]
			if parentPrefix == "" || parentSuffix == "" {
				t.Fatalf("parent style probe has no ANSI envelope: %q", parentProbe)
			}
			if !strings.HasSuffix(footerKey, parentSuffix) || !strings.HasSuffix(footerDescription, parentSuffix) {
				t.Fatalf("nested footer styles do not end in the parent reset %q: key=%q description=%q", parentSuffix, footerKey, footerDescription)
			}
			keyBody := strings.TrimSuffix(footerKey, parentSuffix)
			descriptionBody := strings.TrimSuffix(footerDescription, parentSuffix)
			paddingWidth := frame.InnerWidth() - ansi.StringWidth(footerKey+footerDescription)
			if paddingWidth <= 0 {
				t.Fatalf("footer fixture needs trailing padding, got width %d", paddingWidth)
			}
			wantInterior := parentPrefix +
				keyBody + parentSuffix + parentPrefix +
				descriptionBody + strings.Repeat(" ", paddingWidth) + parentSuffix
			borderCell := lipgloss.NewStyle().
				Foreground(th.Color(th.Palette.Rule)).
				Background(canvas).
				Render("│")
			wantLine := borderCell + wantInterior + borderCell
			if lines[2] != wantLine {
				t.Errorf("Frame footer ANSI sequence did not preserve Canvas through every reset, trailing padding, and final inner cell:\n got: %q\nwant: %q", lines[2], wantLine)
			}
		})
	}
}
