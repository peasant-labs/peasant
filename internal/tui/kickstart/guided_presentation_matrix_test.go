package kickstart_test

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

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	requiredGuidedPresentationSections = 5
	requiredGuidedPresentationThemes   = 2
	requiredGuidedPresentationSizes    = 2
	requiredGuidedPresentationCases    = 20
	requiredGuidedPrivacyMarkers       = 2
)

type guidedPresentationSection struct {
	Key     string   `yaml:"key"`
	Heading string   `yaml:"heading"`
	Control string   `yaml:"control"`
	Intro   string   `yaml:"intro"`
	Markers []string `yaml:"markers"`
}

type guidedPresentationCase struct {
	Name    string `yaml:"name"`
	Section string `yaml:"section"`
	Theme   string `yaml:"theme"`
	Width   int    `yaml:"width"`
	Height  int    `yaml:"height"`
}

type guidedPresentationDocument struct {
	ExpectedSectionCount int                         `yaml:"expectedSectionCount"`
	ExpectedThemeCount   int                         `yaml:"expectedThemeCount"`
	ExpectedSizeCount    int                         `yaml:"expectedSizeCount"`
	ExpectedCaseCount    int                         `yaml:"expectedCaseCount"`
	Sections             []guidedPresentationSection `yaml:"sections"`
	Cases                []guidedPresentationCase    `yaml:"cases"`
}

//go:embed testdata/guided/presentation_matrix.yaml
var guidedPresentationFixtureData []byte

func decodeGuidedPresentationDocument(data []byte) (guidedPresentationDocument, error) {
	var document guidedPresentationDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode guided presentation matrix: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("guided presentation matrix must contain exactly one document: %w", err)
	}
	if document.ExpectedSectionCount != requiredGuidedPresentationSections || len(document.Sections) != requiredGuidedPresentationSections {
		return document, fmt.Errorf("guided presentation sections: declared=%d actual=%d required=%d",
			document.ExpectedSectionCount, len(document.Sections), requiredGuidedPresentationSections)
	}
	if document.ExpectedThemeCount != requiredGuidedPresentationThemes || document.ExpectedSizeCount != requiredGuidedPresentationSizes {
		return document, fmt.Errorf("guided presentation dimensions themes/sizes=%d/%d, require %d/%d",
			document.ExpectedThemeCount, document.ExpectedSizeCount,
			requiredGuidedPresentationThemes, requiredGuidedPresentationSizes)
	}
	if document.ExpectedCaseCount != requiredGuidedPresentationCases || len(document.Cases) != requiredGuidedPresentationCases {
		return document, fmt.Errorf("guided presentation cases: declared=%d actual=%d required=%d",
			document.ExpectedCaseCount, len(document.Cases), requiredGuidedPresentationCases)
	}

	required := map[string]bool{
		kickstart.SectionAutoIngest:  true,
		kickstart.SectionPrivacy:     true,
		kickstart.SectionLicense:     true,
		kickstart.SectionDestination: true,
		kickstart.SectionRetention:   true,
	}
	sections := map[string]guidedPresentationSection{}
	for _, section := range document.Sections {
		if !required[section.Key] || sections[section.Key].Key != "" || strings.TrimSpace(section.Heading) == "" ||
			strings.TrimSpace(section.Control) == "" || strings.TrimSpace(section.Intro) == "" {
			return document, fmt.Errorf("guided presentation section is incomplete, unknown, or duplicated: %#v", section)
		}
		markers := map[string]bool{}
		for _, marker := range section.Markers {
			if strings.TrimSpace(marker) == "" || markers[marker] {
				return document, fmt.Errorf("guided presentation section %q has an empty or duplicate marker %q", section.Key, marker)
			}
			markers[marker] = true
		}
		sections[section.Key] = section
	}
	for key := range required {
		if sections[key].Key == "" {
			return document, fmt.Errorf("guided presentation matrix omits canonical section %q", key)
		}
	}
	privacyMarkers := map[string]bool{}
	for _, marker := range sections[kickstart.SectionPrivacy].Markers {
		privacyMarkers[marker] = true
	}
	if len(privacyMarkers) != requiredGuidedPrivacyMarkers || !privacyMarkers["- before:"] || !privacyMarkers["+ after:"] {
		return document, fmt.Errorf("guided privacy markers=%v, require visible before and after tokens", sections[kickstart.SectionPrivacy].Markers)
	}

	names := map[string]bool{}
	pairs := map[string]int{}
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] || sections[row.Section].Key == "" ||
			(row.Theme != "dark" && row.Theme != "light") ||
			!((row.Width == 80 && row.Height == 24) || (row.Width == 120 && row.Height == 40)) {
			return document, fmt.Errorf("guided presentation case is incomplete, invalid, or duplicated: %#v", row)
		}
		names[row.Name] = true
		pairs[row.Section+"/"+row.Theme+fmt.Sprintf("/%dx%d", row.Width, row.Height)]++
	}
	for key := range sections {
		for _, mode := range []string{"dark", "light"} {
			for _, size := range []string{"80x24", "120x40"} {
				pair := key + "/" + mode + "/" + size
				if pairs[pair] != 1 {
					return document, fmt.Errorf("guided presentation pair %q has %d rows, want exactly one", pair, pairs[pair])
				}
			}
		}
	}
	return document, nil
}

func loadGuidedPresentationDocument(t *testing.T) guidedPresentationDocument {
	t.Helper()
	document, err := decodeGuidedPresentationDocument(guidedPresentationFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func mutateGuidedPresentationFixture(t *testing.T, data, old, replacement []byte) []byte {
	t.Helper()
	if count := bytes.Count(data, old); count != 1 {
		t.Fatalf("guided-presentation mutation source %q occurs %d times, want exactly one", old, count)
	}
	return bytes.Replace(data, old, replacement, 1)
}

func guidedPresentationTheme(t *testing.T, name string) theme.Theme {
	t.Helper()
	if name == "dark" {
		return theme.New(theme.ModeDark)
	}
	if name == "light" {
		return theme.New(theme.ModeLight)
	}
	t.Fatalf("unknown guided presentation theme %q", name)
	return theme.Theme{}
}

func guidedPresentationSectionByKey(t *testing.T, document guidedPresentationDocument, key string) guidedPresentationSection {
	t.Helper()
	for _, section := range document.Sections {
		if section.Key == key {
			return section
		}
	}
	t.Fatalf("guided presentation fixture has no section %q", key)
	return guidedPresentationSection{}
}

func exactMountedLine(view, text string) int {
	for index, line := range strings.Split(view, "\n") {
		plain := strings.TrimSpace(strings.Trim(ansi.Strip(line), "│"))
		if plain == text {
			return index
		}
	}
	return -1
}

func containingMountedLine(view, text string) int {
	for index, line := range strings.Split(view, "\n") {
		if strings.Contains(ansi.Strip(line), text) {
			return index
		}
	}
	return -1
}

func TestGuidedPresentationMatrixMountsEverySectionInBothThemesAndSizes(t *testing.T) {
	document := loadGuidedPresentationDocument(t)
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			fixture := guidedPresentationSectionByKey(t, document, row.Section)
			draft, _ := newGuidedDraft(t)
			draft.Working().Selection.Mode = config.SelectionModeSelected
			registry := kickstart.BuildRegistry(kickstart.Options{
				Source:                scannerfix.NewFixtureTreeSource("standard"),
				VillageConnected:      true,
				ClaudeSessionsPresent: true,
			})
			section := findSection(t, registry, row.Section)
			if section.Guide == nil {
				t.Fatalf("canonical section %q has no guide", row.Section)
			}
			if section.Guide.Intro != fixture.Intro {
				t.Fatalf("canonical section %q guide intro=%q, want %q", row.Section, section.Guide.Intro, fixture.Intro)
			}
			flow := settings.NewFlow(guidedPresentationTheme(t, row.Theme), settings.Registry{Sections: []settings.Section{section}}, draft)
			flow.SetSize(row.Width, row.Height)
			view := flow.View()

			headingAt := exactMountedLine(view, fixture.Heading)
			introAt := exactMountedLine(view, fixture.Intro)
			controlAt := containingMountedLine(view, fixture.Control)
			if headingAt < 0 || introAt < 0 || controlAt < 0 || !(headingAt < introAt && introAt < controlAt) {
				t.Fatalf("mounted order heading/intro/control=%d/%d/%d for %q:\n%s",
					headingAt, introAt, controlAt, row.Section, ansi.Strip(view))
			}
			if count := strings.Count(ansi.Strip(view), fixture.Intro); count != 1 {
				t.Fatalf("mounted section %q guide intro count=%d, want exactly one", row.Section, count)
			}
			if count := strings.Count(ansi.Strip(view), fixture.Heading); count != 1 {
				t.Fatalf("mounted section %q heading count=%d, want exactly one", row.Section, count)
			}
			for _, marker := range fixture.Markers {
				if !strings.Contains(ansi.Strip(view), marker) {
					t.Errorf("mounted section %q omits required semantic marker %q", row.Section, marker)
				}
			}
			for lineIndex, line := range strings.Split(view, "\n") {
				if width := lipgloss.Width(line); width != row.Width {
					t.Errorf("mounted section %q line %d width=%d, want %d", row.Section, lineIndex, width, row.Width)
				}
			}
			if lines := strings.Count(view, "\n") + 1; lines != row.Height {
				t.Errorf("mounted section %q height=%d, want %d", row.Section, lines, row.Height)
			}

			th := guidedPresentationTheme(t, row.Theme)
			innerWidth := row.Width - 2
			fittedIntro := fixture.Intro + strings.Repeat(" ", innerWidth-ansi.StringWidth(fixture.Intro))
			surfaceRun := strings.TrimSuffix(th.Styles().Surface.Render(fittedIntro), "\x1b[m")
			rawIntro := strings.Split(view, "\n")[introAt]
			if !strings.Contains(rawIntro, surfaceRun) {
				t.Errorf("mounted section %q surface background does not reach the final inner cell", row.Section)
			}
			if len(section.Guide.Hints) > 0 {
				hintAt := containingMountedLine(view, section.Guide.Hints[0])
				if hintAt < 0 {
					t.Fatalf("mounted section %q does not show its first muted hint", row.Section)
				}
				probe := th.Styles().Surface.Render("x")
				surfacePrefix := probe[:strings.Index(probe, "x")]
				if strings.Contains(strings.Split(view, "\n")[hintAt], surfacePrefix) {
					t.Errorf("mounted section %q promoted its transparent muted hint to a surface row", row.Section)
				}
			}
		})
	}
}

func TestGuidedPresentationFixtureRejectsMissingCanonicalSection(t *testing.T) {
	mutated := mutateGuidedPresentationFixture(t, guidedPresentationFixtureData, []byte("expectedSectionCount: 5"), []byte("expectedSectionCount: 4"))
	mutated = mutateGuidedPresentationFixture(t, mutated, []byte("  - key: retention\n    heading: how long claude code keeps its transcripts\n    control: '( ) 30 days'\n    intro: choose how long claude code keeps its source transcript files.\n"), nil)
	if _, err := decodeGuidedPresentationDocument(mutated); err == nil {
		t.Fatal("guided presentation fixture accepted removal of a canonical guided section")
	}
}

func TestGuidedPresentationFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), guidedPresentationFixtureData...), []byte("\nunknownField: true\n")...)
	if bytes.Equal(mutated, guidedPresentationFixtureData) {
		t.Fatal("guided-presentation unknown-field mutation did not alter the fixture")
	}
	if _, err := decodeGuidedPresentationDocument(mutated); err == nil {
		t.Fatal("guided presentation fixture accepted an unknown field")
	}
}

func TestGuidedPresentationFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), guidedPresentationFixtureData...), []byte("\n---\n{}\n")...)
	if bytes.Equal(mutated, guidedPresentationFixtureData) {
		t.Fatal("guided-presentation trailing-document mutation did not alter the fixture")
	}
	if _, err := decodeGuidedPresentationDocument(mutated); err == nil {
		t.Fatal("guided presentation fixture accepted a trailing document")
	}
}
