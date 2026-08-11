package settings

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	requiredGuideRenderCases     = 4
	requiredGuideExampleLines    = 5
	requiredGuideValidationCases = 6
	requiredGuideErrorCases      = 1
)

type guideFixtureKind string

const (
	guideFixtureUnknown guideFixtureKind = "unknown"
	guideFixtureText    guideFixtureKind = "text"
	guideFixtureLabel   guideFixtureKind = "label"
	guideFixtureBefore  guideFixtureKind = "before"
	guideFixtureAfter   guideFixtureKind = "after"
	guideFixtureSpacer  guideFixtureKind = "spacer"
)

func (k guideFixtureKind) productionKind() (GuideExampleLineKind, bool) {
	switch k {
	case guideFixtureUnknown:
		return GuideExampleLineUnknown, true
	case guideFixtureText:
		return GuideExampleLineText, true
	case guideFixtureLabel:
		return GuideExampleLineLabel, true
	case guideFixtureBefore:
		return GuideExampleLineBefore, true
	case guideFixtureAfter:
		return GuideExampleLineAfter, true
	case guideFixtureSpacer:
		return GuideExampleLineSpacer, true
	default:
		return GuideExampleLineUnknown, false
	}
}

type guideRenderCase struct {
	Name  string `yaml:"name"`
	Theme string `yaml:"theme"`
	Width int    `yaml:"width"`
}

type guideExampleFixtureLine struct {
	Kind guideFixtureKind `yaml:"kind"`
	Text string           `yaml:"text"`
}

type guideValidationCase struct {
	Name         string           `yaml:"name"`
	Kind         guideFixtureKind `yaml:"kind"`
	Text         string           `yaml:"text"`
	WantFragment string           `yaml:"wantFragment"`
}

type guideErrorCase struct {
	Name        string `yaml:"name"`
	Error       string `yaml:"error"`
	WantVisible string `yaml:"wantVisible"`
}

type guideRenderDocument struct {
	ExpectedRenderCaseCount     int                       `yaml:"expectedRenderCaseCount"`
	ExpectedExampleLineCount    int                       `yaml:"expectedExampleLineCount"`
	ExpectedValidationCaseCount int                       `yaml:"expectedValidationCaseCount"`
	ExpectedErrorCaseCount      int                       `yaml:"expectedErrorCaseCount"`
	Heading                     string                    `yaml:"heading"`
	Intro                       string                    `yaml:"intro"`
	Hint                        string                    `yaml:"hint"`
	Description                 string                    `yaml:"description"`
	SecondHeading               string                    `yaml:"secondHeading"`
	SecondControl               string                    `yaml:"secondControl"`
	ExampleLines                []guideExampleFixtureLine `yaml:"exampleLines"`
	RenderCases                 []guideRenderCase         `yaml:"renderCases"`
	ValidationCases             []guideValidationCase     `yaml:"validationCases"`
	ErrorCases                  []guideErrorCase          `yaml:"errorCases"`
}

//go:embed testdata/guide_render.yaml
var guideRenderFixtureData []byte

func decodeGuideRenderDocument(data []byte) (guideRenderDocument, error) {
	var document guideRenderDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/guide_render.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("testdata/guide_render.yaml must contain exactly one document: %w", err)
	}
	if document.ExpectedRenderCaseCount != requiredGuideRenderCases || len(document.RenderCases) != requiredGuideRenderCases {
		return document, fmt.Errorf("guide render rows: declared=%d actual=%d required=%d",
			document.ExpectedRenderCaseCount, len(document.RenderCases), requiredGuideRenderCases)
	}
	if document.ExpectedExampleLineCount != requiredGuideExampleLines || len(document.ExampleLines) != requiredGuideExampleLines {
		return document, fmt.Errorf("guide example rows: declared=%d actual=%d required=%d",
			document.ExpectedExampleLineCount, len(document.ExampleLines), requiredGuideExampleLines)
	}
	if document.ExpectedValidationCaseCount != requiredGuideValidationCases || len(document.ValidationCases) != requiredGuideValidationCases {
		return document, fmt.Errorf("guide validation rows: declared=%d actual=%d required=%d",
			document.ExpectedValidationCaseCount, len(document.ValidationCases), requiredGuideValidationCases)
	}
	if document.ExpectedErrorCaseCount != requiredGuideErrorCases || len(document.ErrorCases) != requiredGuideErrorCases {
		return document, fmt.Errorf("guide error rows: declared=%d actual=%d required=%d",
			document.ExpectedErrorCaseCount, len(document.ErrorCases), requiredGuideErrorCases)
	}
	if strings.TrimSpace(document.Heading) == "" || strings.TrimSpace(document.Intro) == "" ||
		strings.TrimSpace(document.Hint) == "" || strings.TrimSpace(document.Description) == "" ||
		strings.TrimSpace(document.SecondHeading) == "" || strings.TrimSpace(document.SecondControl) == "" {
		return document, fmt.Errorf("guide render fixture leaves heading, intro, hint, description, or second-field evidence empty")
	}
	names := map[string]bool{}
	pairs := map[string]bool{}
	for _, row := range document.RenderCases {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] || (row.Theme != "dark" && row.Theme != "light") || row.Width <= 0 {
			return document, fmt.Errorf("guide render row is incomplete or duplicated: %#v", row)
		}
		names[row.Name] = true
		pair := fmt.Sprintf("%s/%d", row.Theme, row.Width)
		if pairs[pair] {
			return document, fmt.Errorf("guide render pair %q is duplicated", pair)
		}
		pairs[pair] = true
	}
	for _, row := range document.ExampleLines {
		if _, ok := row.Kind.productionKind(); !ok || row.Kind == guideFixtureUnknown {
			return document, fmt.Errorf("guide example fixture has unsupported kind %q", row.Kind)
		}
	}
	validationNames := map[string]bool{}
	for _, row := range document.ValidationCases {
		if strings.TrimSpace(row.Name) == "" || validationNames[row.Name] || strings.TrimSpace(row.WantFragment) == "" {
			return document, fmt.Errorf("guide validation row is incomplete or duplicated: %#v", row)
		}
		validationNames[row.Name] = true
		if _, ok := row.Kind.productionKind(); !ok {
			return document, fmt.Errorf("guide validation row %q has unsupported kind %q", row.Name, row.Kind)
		}
	}
	errorNames := map[string]bool{}
	for _, row := range document.ErrorCases {
		if strings.TrimSpace(row.Name) == "" || errorNames[row.Name] || row.Error == "" || strings.TrimSpace(row.WantVisible) == "" {
			return document, fmt.Errorf("guide error row is incomplete or duplicated: %#v", row)
		}
		errorNames[row.Name] = true
	}
	return document, nil
}

func loadGuideRenderDocument(t *testing.T) guideRenderDocument {
	t.Helper()
	document, err := decodeGuideRenderDocument(guideRenderFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func mutateGuideRenderFixture(t *testing.T, data, old, replacement []byte) []byte {
	t.Helper()
	if count := bytes.Count(data, old); count != 1 {
		t.Fatalf("guide-render mutation source %q occurs %d times, want exactly one", old, count)
	}
	return bytes.Replace(data, old, replacement, 1)
}

func productionGuideLines(t *testing.T, rows []guideExampleFixtureLine) []GuideExampleLine {
	t.Helper()
	lines := make([]GuideExampleLine, 0, len(rows))
	for _, row := range rows {
		kind, ok := row.Kind.productionKind()
		if !ok {
			t.Fatalf("unsupported guide fixture kind %q", row.Kind)
		}
		lines = append(lines, GuideExampleLine{Kind: kind, Text: row.Text})
	}
	return lines
}

func guideRenderTheme(t *testing.T, name string) theme.Theme {
	t.Helper()
	switch name {
	case "dark":
		return theme.New(theme.ModeDark)
	case "light":
		return theme.New(theme.ModeLight)
	default:
		t.Fatalf("unknown guide render theme %q", name)
		return theme.Theme{}
	}
}

func guideRenderRegistry(document guideRenderDocument, example []GuideExampleLine) Registry {
	accessor := Accessor[string]{
		Get: func(*config.Config) string { return "standard" },
		Set: func(*config.Config, string) {},
	}
	secondaryAccessor := Accessor[string]{
		Get: func(*config.Config) string { return "keep" },
		Set: func(*config.Config, string) {},
	}
	return Registry{Sections: []Section{{
		Key:   "privacy-preview",
		Title: "privacy preview",
		Guide: &Guide{
			Intro: document.Intro,
			Hints: []string{document.Hint},
			Example: func(*Draft) ([]GuideExampleLine, error) {
				return append([]GuideExampleLine(nil), example...), nil
			},
		},
		Fields: []Field{
			WithDescription(
				Radio("privacy-policy", document.Heading, accessor,
					Option[string]{Label: "standard", Value: "standard"},
					Option[string]{Label: "maximum", Value: "maximum"}),
				document.Description),
			Radio("secondary-policy", document.SecondHeading, secondaryAccessor,
				Option[string]{Label: "keep", Value: "keep"},
				Option[string]{Label: "replace", Value: "replace"}),
		},
	}}}
}

func renderGuideFixtureBody(t *testing.T, document guideRenderDocument, row guideRenderCase) (Flow, theme.Theme, string) {
	t.Helper()
	th := guideRenderTheme(t, row.Theme)
	draft, err := NewDraft("/tmp/guide-render/config.yaml", config.BaseConfig())
	if err != nil {
		t.Fatalf("open guide render draft: %v", err)
	}
	flow := NewFlow(th, guideRenderRegistry(document, productionGuideLines(t, document.ExampleLines)), draft)
	flow.SetSize(row.Width+2, 30)
	return flow, th, flow.renderBody(row.Width, 27)
}

func TestGuideRenderOrderAndExactSemanticStyles(t *testing.T) {
	document := loadGuideRenderDocument(t)
	for _, row := range document.RenderCases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			_, th, body := renderGuideFixtureBody(t, document, row)
			plain := ansi.Strip(body)
			ordered := []string{
				document.Heading,
				clip(document.Intro, row.Width),
				clip("• "+document.Hint, row.Width),
				"CREDENTIAL",
				"- before: sk-example",
				"+ after: <CREDENTIAL>",
				"derived by the production boundary",
				clip(document.Description, row.Width),
				"(•) standard",
				document.SecondHeading,
				document.SecondControl,
			}
			last := -1
			for _, text := range ordered {
				at := strings.Index(plain, text)
				if at < 0 {
					t.Fatalf("guide render is missing %q:\n%s", text, plain)
				}
				if at <= last {
					t.Fatalf("guide render order changed at %q:\n%s", text, plain)
				}
				last = at
			}
			if count := strings.Count(plain, clip(document.Intro, row.Width)); count != 1 {
				t.Fatalf("guide intro count=%d, want exactly one", count)
			}

			styles := th.Styles()
			surface := th.Color(th.Palette.Surface)
			canvas := th.Color(th.Palette.Canvas)
			rawLines := strings.Split(body, "\n")
			wantLines := []string{
				kit.FitLine(styles.Header.Background(canvas), document.Heading, row.Width),
				kit.FitLine(styles.Surface, document.Intro, row.Width),
				styles.Muted.Render(clip("• "+document.Hint, row.Width)),
				kit.FitLine(styles.Header.Background(surface), "CREDENTIAL", row.Width),
				kit.FitLine(styles.DiffDel.Background(surface), "- before: sk-example", row.Width),
				kit.FitLine(styles.DiffAdd.Background(surface), "+ after: <CREDENTIAL>", row.Width),
				kit.FitLine(styles.Surface, "", row.Width),
				kit.FitLine(styles.Surface, "derived by the production boundary", row.Width),
				styles.Muted.Render(clip(document.Description, row.Width)),
			}
			for _, want := range wantLines {
				if !containsExactGuideLine(rawLines, want) {
					t.Errorf("guide render is missing exact semantic ANSI row %q", want)
				}
			}
		})
	}
}

func containsExactGuideLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func TestGuideRenderOracleRejectsGlyphOnlyStyleSwapAndOrderMutations(t *testing.T) {
	document := loadGuideRenderDocument(t)
	row := document.RenderCases[0]
	_, th, body := renderGuideFixtureBody(t, document, row)
	styles := th.Styles()
	surface := th.Color(th.Palette.Surface)
	before := kit.FitLine(styles.DiffDel.Background(surface), "- before: sk-example", row.Width)
	after := kit.FitLine(styles.DiffAdd.Background(surface), "+ after: <CREDENTIAL>", row.Width)
	if !containsExactGuideLine(strings.Split(body, "\n"), before) || !containsExactGuideLine(strings.Split(body, "\n"), after) {
		t.Fatal("guide mutation oracle precondition failed on canonical diff rows")
	}

	glyphOnly := strings.Replace(body, before, ansi.Strip(before), 1)
	if containsExactGuideLine(strings.Split(glyphOnly, "\n"), before) {
		t.Fatal("guide oracle accepted a glyph-only before row without its deletion token")
	}
	styleSwap := strings.Replace(body, before,
		kit.FitLine(styles.DiffAdd.Background(surface), "- before: sk-example", row.Width), 1)
	if containsExactGuideLine(strings.Split(styleSwap, "\n"), before) {
		t.Fatal("guide oracle accepted a before row rendered with the addition token")
	}
	orderSwap := strings.Replace(body, before+"\n"+after, after+"\n"+before, 1)
	plain := ansi.Strip(orderSwap)
	if strings.Index(plain, "+ after:") > strings.Index(plain, "- before:") {
		t.Fatal("guide order mutation did not move after before before")
	}
}

func TestGuideExampleContractRejectsInvalidTypedLines(t *testing.T) {
	document := loadGuideRenderDocument(t)
	for index, row := range document.ValidationCases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			kind, ok := row.Kind.productionKind()
			if !ok {
				t.Fatalf("unsupported validation kind %q", row.Kind)
			}
			err := validateGuideExampleLine(index, GuideExampleLine{Kind: kind, Text: row.Text})
			if err == nil || !strings.Contains(err.Error(), row.WantFragment) {
				t.Fatalf("invalid guide line error=%v, want fragment %q", err, row.WantFragment)
			}
			for _, part := range []string{"what:", "why:", "where:", "when:", "means:", "fix:"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("invalid guide line error omits %q: %v", part, err)
				}
			}

			draft, draftErr := NewDraft("/tmp/guide-invalid/config.yaml", config.BaseConfig())
			if draftErr != nil {
				t.Fatalf("open invalid-guide draft: %v", draftErr)
			}
			flow := NewFlow(theme.New(theme.ModeDark),
				guideRenderRegistry(document, []GuideExampleLine{{Kind: kind, Text: row.Text}}), draft)
			flow.SetSize(80, 24)
			view := ansi.Strip(flow.View())
			if !strings.Contains(view, "example unavailable; unverified output withheld") ||
				!strings.Contains(view, "(•) standard") {
				t.Errorf("invalid typed guide line did not fail closed while retaining the canonical control:\n%s", view)
			}
		})
	}
}

func TestGuideProviderErrorsCannotEmitTerminalControls(t *testing.T) {
	document := loadGuideRenderDocument(t)
	for _, row := range document.ErrorCases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			draft, err := NewDraft("/tmp/guide-error/config.yaml", config.BaseConfig())
			if err != nil {
				t.Fatalf("open guide-error draft: %v", err)
			}
			registry := guideRenderRegistry(document, productionGuideLines(t, document.ExampleLines))
			registry.Sections[0].Guide.Example = func(*Draft) ([]GuideExampleLine, error) {
				return nil, fmt.Errorf("%s", row.Error)
			}
			flow := NewFlow(theme.New(theme.ModeDark), registry, draft)
			flow.SetSize(80, 24)
			raw := flow.View()
			plain := ansi.Strip(raw)
			if !strings.Contains(plain, row.WantVisible) {
				t.Errorf("sanitized provider error omits %q:\n%s", row.WantVisible, plain)
			}
			for _, value := range plain {
				if value != '\n' && unicode.IsControl(value) {
					t.Errorf("sanitized provider error retained terminal control U+%04X in %q", value, plain)
				}
			}
			if strings.Contains(raw, "\x1b[31m") {
				t.Errorf("sanitized provider error retained provider ANSI styling: %q", raw)
			}
		})
	}
}

func TestGuideRenderFixtureRejectsCoordinatedCaseRemoval(t *testing.T) {
	mutated := mutateGuideRenderFixture(t, guideRenderFixtureData, []byte("expectedRenderCaseCount: 4"), []byte("expectedRenderCaseCount: 3"))
	mutated = mutateGuideRenderFixture(t, mutated, []byte("  - {name: light-wide, theme: light, width: 72}\n"), nil)
	if _, err := decodeGuideRenderDocument(mutated); err == nil {
		t.Fatal("guide render fixture accepted a case removal coordinated with its declared count")
	}
}

func TestGuideRenderFixtureRejectsCoordinatedValidationRemoval(t *testing.T) {
	mutated := mutateGuideRenderFixture(t, guideRenderFixtureData, []byte("expectedValidationCaseCount: 6"), []byte("expectedValidationCaseCount: 5"))
	mutated = mutateGuideRenderFixture(t, mutated, []byte("  - {name: embedded-tab, kind: after, text: \"left\\tright\", wantFragment: terminal control character}\n"), nil)
	if _, err := decodeGuideRenderDocument(mutated); err == nil {
		t.Fatal("guide render fixture accepted a validation row removal coordinated with its declared count")
	}
}

func TestGuideRenderFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), guideRenderFixtureData...), []byte("\nunknownField: true\n")...)
	if bytes.Equal(mutated, guideRenderFixtureData) {
		t.Fatal("guide-render unknown-field mutation did not alter the fixture")
	}
	if _, err := decodeGuideRenderDocument(mutated); err == nil {
		t.Fatal("guide-render fixture accepted an unknown field")
	}
}

func TestGuideRenderFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), guideRenderFixtureData...), []byte("\n---\n{}\n")...)
	if bytes.Equal(mutated, guideRenderFixtureData) {
		t.Fatal("guide-render trailing-document mutation did not alter the fixture")
	}
	if _, err := decodeGuideRenderDocument(mutated); err == nil {
		t.Fatal("guide-render fixture accepted a trailing document")
	}
}
