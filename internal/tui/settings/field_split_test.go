package settings

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	requiredFieldSplitCases      = 2
	requiredFieldSplitErrorCases = 1
)

type fieldSplitExampleLine struct {
	Kind guideFixtureKind `yaml:"kind"`
	Text string           `yaml:"text"`
}

type fieldSplitCase struct {
	Name   string `yaml:"name"`
	Width  int    `yaml:"width"`
	Height int    `yaml:"height"`
}

type fieldSplitErrorCase struct {
	Name        string `yaml:"name"`
	Error       string `yaml:"error"`
	WantVisible string `yaml:"wantVisible"`
}

type fieldSplitDocument struct {
	ExpectedCaseCount      int                     `yaml:"expectedCaseCount"`
	ExpectedErrorCaseCount int                     `yaml:"expectedErrorCaseCount"`
	Heading                string                  `yaml:"heading"`
	Description            string                  `yaml:"description"`
	ExampleLines           []fieldSplitExampleLine `yaml:"exampleLines"`
	Cases                  []fieldSplitCase        `yaml:"cases"`
	ErrorCases             []fieldSplitErrorCase   `yaml:"errorCases"`
}

//go:embed testdata/field_split.yaml
var fieldSplitFixtureData []byte

func loadFieldSplitDocument(t *testing.T) fieldSplitDocument {
	t.Helper()
	var document fieldSplitDocument
	decoder := yaml.NewDecoder(bytes.NewReader(fieldSplitFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode testdata/field_split.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("testdata/field_split.yaml must contain exactly one document: %v", err)
	}
	if document.ExpectedCaseCount != requiredFieldSplitCases || len(document.Cases) != requiredFieldSplitCases {
		t.Fatalf("field split cases: declared=%d actual=%d required=%d",
			document.ExpectedCaseCount, len(document.Cases), requiredFieldSplitCases)
	}
	if document.ExpectedErrorCaseCount != requiredFieldSplitErrorCases || len(document.ErrorCases) != requiredFieldSplitErrorCases {
		t.Fatalf("field split error cases: declared=%d actual=%d required=%d",
			document.ExpectedErrorCaseCount, len(document.ErrorCases), requiredFieldSplitErrorCases)
	}
	if strings.TrimSpace(document.Heading) == "" || strings.TrimSpace(document.Description) == "" {
		t.Fatalf("field split fixture leaves heading or description empty")
	}
	names := map[string]bool{}
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] || row.Width <= 0 || row.Height <= 0 {
			t.Fatalf("field split case is incomplete or duplicated: %#v", row)
		}
		names[row.Name] = true
	}
	return document
}

func fieldSplitExampleLines(document fieldSplitDocument) []GuideExampleLine {
	lines := make([]GuideExampleLine, 0, len(document.ExampleLines))
	for _, row := range document.ExampleLines {
		kind, _ := row.Kind.productionKind()
		lines = append(lines, GuideExampleLine{Kind: kind, Text: row.Text})
	}
	return lines
}

// fieldSplitRegistry mounts one radio field behind [WithExamplePane], mirroring
// how the kickstart privacy step wires its redaction-level control beside its
// scrollable example.
func fieldSplitRegistry(document fieldSplitDocument, example GuideExampleFunc) Registry {
	accessor := Accessor[string]{
		Get: func(*config.Config) string { return "standard" },
		Set: func(*config.Config, string) {},
	}
	return Registry{Sections: []Section{{
		Key:   "split-preview",
		Title: "split preview",
		Fields: []Field{
			WithExamplePane(
				WithDescription(
					Radio("split-policy", document.Heading, accessor,
						Option[string]{Label: "standard", Value: "standard"},
						Option[string]{Label: "maximum", Value: "maximum"}),
					document.Description),
				example,
			),
		},
	}}}
}

// TestSplitFieldControlAlwaysVisibleAndExampleScrolls proves the two halves of
// WithExamplePane's contract: the control renders on every draw with no
// paging, and a right pane too tall for its height genuinely scrolls when
// focused - a stale rendering that ignored key input would leave the view
// unchanged.
func TestSplitFieldControlAlwaysVisibleAndExampleScrolls(t *testing.T) {
	document := loadFieldSplitDocument(t)
	example := func(*Draft) ([]GuideExampleLine, error) {
		return fieldSplitExampleLines(document), nil
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			draft, err := NewDraft("/tmp/field-split/config.yaml", config.BaseConfig())
			if err != nil {
				t.Fatalf("open field split draft: %v", err)
			}
			flow := NewFlow(theme.New(theme.ModeDark), fieldSplitRegistry(document, example), draft)
			flow.SetSize(row.Width, row.Height)

			before := ansi.Strip(flow.View())
			if !strings.Contains(before, "(•) standard") {
				t.Fatalf("split control not visible with no keys pressed at %dx%d:\n%s", row.Width, row.Height, before)
			}
			if !strings.Contains(before, "CREDENTIAL") {
				t.Fatalf("split example not visible with no keys pressed at %dx%d:\n%s", row.Width, row.Height, before)
			}

			flow, _ = flow.Update(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
			flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
			after := ansi.Strip(flow.View())
			if !strings.Contains(after, "(•) standard") {
				t.Fatalf("split control scrolled off after focusing and paging the right pane at %dx%d:\n%s",
					row.Width, row.Height, after)
			}
			if after == before {
				t.Fatalf("split right pane did not change after focus+page-down at %dx%d:\n%s", row.Width, row.Height, after)
			}
		})
	}
}

// TestSplitFieldExampleErrorFailsClosedWithoutDisplacingControl proves an
// example provider's error renders the split's own actionable "preview
// unavailable" message in the right pane while the field's own control stays
// mounted and interactive on the left - never a panic, and never invented
// success output.
func TestSplitFieldExampleErrorFailsClosedWithoutDisplacingControl(t *testing.T) {
	document := loadFieldSplitDocument(t)
	for _, row := range document.ErrorCases {
		t.Run(row.Name, func(t *testing.T) {
			example := func(*Draft) ([]GuideExampleLine, error) {
				return nil, fmt.Errorf("%s", row.Error)
			}
			draft, err := NewDraft("/tmp/field-split-error/config.yaml", config.BaseConfig())
			if err != nil {
				t.Fatalf("open field split error draft: %v", err)
			}
			flow := NewFlow(theme.New(theme.ModeDark), fieldSplitRegistry(document, example), draft)
			flow.SetSize(80, 24)
			view := ansi.Strip(flow.View())
			if !strings.Contains(view, row.WantVisible) {
				t.Errorf("split example error does not contain %q:\n%s", row.WantVisible, view)
			}
			if !strings.Contains(view, "(•) standard") {
				t.Errorf("split example error displaced the canonical control:\n%s", view)
			}
			if !strings.Contains(view, row.Error) {
				t.Errorf("split example error omits the underlying failure reason %q:\n%s", row.Error, view)
			}
		})
	}
}
