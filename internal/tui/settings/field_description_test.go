package settings

import (
	"bytes"
	_ "embed"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/field_descriptions.yaml
var fieldDescriptionData []byte

// fieldDescriptionCase is one render expectation over the described-field flow.
type fieldDescriptionCase struct {
	Name string `yaml:"name"`
	// Step is the flow step to render (0 = toggle section, 1 = radio section).
	Step int `yaml:"step"`
	// Present fragments must appear in the rendered body.
	Present []string `yaml:"present"`
	// Absent fragments must NOT appear in the rendered body.
	Absent []string `yaml:"absent"`
	// LabelOnce, when set, must appear exactly once in the body — proving a lone
	// toggle's label is not doubled by a section header.
	LabelOnce string `yaml:"labelOnce"`
}

type fieldDescriptionDoc struct {
	ExpectedCaseCount int                    `yaml:"expectedCaseCount"`
	Cases             []fieldDescriptionCase `yaml:"cases"`
}

func loadFieldDescriptionDoc(t *testing.T) fieldDescriptionDoc {
	t.Helper()
	var doc fieldDescriptionDoc
	dec := yaml.NewDecoder(bytes.NewReader(fieldDescriptionData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/field_descriptions.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("field_descriptions.yaml must hold exactly one document")
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	return doc
}

// describedRegistry is a two-step flow exercising both new render paths: a lone
// toggle carrying an always-shown description, and a radio carrying both a field
// description and per-option help.
func describedRegistry() Registry {
	return Registry{Sections: []Section{
		{
			Key:   "ingest",
			Title: "auto-ingest",
			Fields: []Field{
				WithDescription(
					Toggle("auto-ingest", "auto-ingest new branches", connectedAccessor()),
					"turn this on to import new branches without asking again."),
			},
		},
		{
			Key:   "privacy",
			Title: "privacy",
			Fields: []Field{
				WithDescription(
					Radio("redaction", "redaction level", urlAccessor(),
						Option[string]{Label: "standard", Value: "standard", Description: "removes file paths and personal data."},
						Option[string]{Label: "maximum", Value: "maximum", Description: "removes everything at maximum."},
					),
					"choose how much sensitive data peasant removes."),
			},
		},
	}}
}

func renderDescribedStep(t *testing.T, step int) string {
	t.Helper()
	d, err := NewDraft("/tmp/settings-desc/config.yaml", config.BaseConfig())
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	f := NewFlow(theme.New(theme.ModeDark), describedRegistry(), d)
	f.SetSize(80, 16)
	for i := 0; i < step; i++ {
		f = send(f, "tab")
	}
	return ansi.Strip(f.View())
}

func TestFlow_FieldDescriptionsRender(t *testing.T) {
	doc := loadFieldDescriptionDoc(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			body := renderDescribedStep(t, c.Step)
			for _, want := range c.Present {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q\n---\n%s", want, body)
				}
			}
			for _, notWant := range c.Absent {
				if strings.Contains(body, notWant) {
					t.Errorf("body unexpectedly contains %q\n---\n%s", notWant, body)
				}
			}
			if c.LabelOnce != "" {
				if got := strings.Count(body, c.LabelOnce); got != 1 {
					t.Errorf("label %q appears %d times, want exactly 1 (doubled header?)\n---\n%s", c.LabelOnce, got, body)
				}
			}
		})
	}
}
