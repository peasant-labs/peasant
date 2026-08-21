package kickstart

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/redact"
)

const (
	privacyAdapterSampleRows        = 4
	privacyAdapterFailureRows       = 4
	privacyAdapterLabelMutationRows = 3
	privacyAdapterViewportRows      = 2
	privacyAdapterViewportMutations = 1
	privacyAdapterLicenseRows       = 4
)

type adapterFailureKind string

const (
	adapterFailureConstructor adapterFailureKind = "constructor-error"
	adapterFailureUnknown     adapterFailureKind = "unknown-category"
	adapterFailureMissing     adapterFailureKind = "missing-category"
	adapterFailureUnchanged   adapterFailureKind = "unchanged-output"
)

func (k adapterFailureKind) valid() bool {
	switch k {
	case adapterFailureConstructor, adapterFailureUnknown, adapterFailureMissing, adapterFailureUnchanged:
		return true
	default:
		return false
	}
}

type adapterSampleFixture struct {
	Name          string                `yaml:"name"`
	Category      redact.Category       `yaml:"category"`
	CategoryLabel redact.CategoryString `yaml:"categoryLabel"`
	Before        string                `yaml:"before"`
}

type adapterLabelMutationFixture struct {
	Name          string                `yaml:"name"`
	Kind          string                `yaml:"kind"`
	SampleName    string                `yaml:"sampleName"`
	CategoryLabel redact.CategoryString `yaml:"categoryLabel"`
}

type adapterViewportFixture struct {
	Name           string   `yaml:"name"`
	Width          int      `yaml:"width"`
	Height         int      `yaml:"height"`
	Keys           []string `yaml:"keys"`
	WantContains   []string `yaml:"wantContains"`
	WantMissing    []string `yaml:"wantMissing"`
	MutationProbe  bool     `yaml:"mutationProbe"`
	MutationReveal string   `yaml:"mutationReveal"`
}

type adapterFailureFixture struct {
	Name         string             `yaml:"name"`
	Kind         adapterFailureKind `yaml:"kind"`
	WantContains []string           `yaml:"wantContains"`
}

type adapterLicenseFixture struct {
	Name      string         `yaml:"name"`
	Value     config.License `yaml:"value"`
	Label     string         `yaml:"label"`
	Guidance  string         `yaml:"guidance"`
	IsDefault bool           `yaml:"isDefault"`
}

type adapterFixtureDocument struct {
	ExpectedPrivacySampleCount    int                           `yaml:"expectedPrivacySampleCount"`
	PrivacySamples                []adapterSampleFixture        `yaml:"privacySamples"`
	ExpectedFailureCount          int                           `yaml:"expectedFailureCount"`
	Failures                      []adapterFailureFixture       `yaml:"failures"`
	ExpectedLabelMutationCount    int                           `yaml:"expectedLabelMutationCount"`
	LabelMutations                []adapterLabelMutationFixture `yaml:"labelMutations"`
	ExpectedViewportCount         int                           `yaml:"expectedViewportCount"`
	ExpectedViewportMutationCount int                           `yaml:"expectedViewportMutationCount"`
	Viewport                      []adapterViewportFixture      `yaml:"viewport"`
	ExpectedLicenseCount          int                           `yaml:"expectedLicenseCount"`
	Licenses                      []adapterLicenseFixture       `yaml:"licenses"`
}

//go:embed testdata/guided/privacy_license.yaml
var privacyAdapterFixtureData []byte

func loadPrivacyAdapterFixture(t *testing.T) adapterFixtureDocument {
	t.Helper()
	var document adapterFixtureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(privacyAdapterFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode privacy adapter fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("privacy adapter fixture must contain exactly one YAML document")
	}
	if document.ExpectedPrivacySampleCount != privacyAdapterSampleRows || len(document.PrivacySamples) != privacyAdapterSampleRows {
		t.Fatalf("privacy adapter samples: declared=%d actual=%d required=%d",
			document.ExpectedPrivacySampleCount, len(document.PrivacySamples), privacyAdapterSampleRows)
	}
	if document.ExpectedFailureCount != privacyAdapterFailureRows || len(document.Failures) != privacyAdapterFailureRows {
		t.Fatalf("privacy adapter failures: declared=%d actual=%d required=%d",
			document.ExpectedFailureCount, len(document.Failures), privacyAdapterFailureRows)
	}
	if document.ExpectedLabelMutationCount != privacyAdapterLabelMutationRows || len(document.LabelMutations) != privacyAdapterLabelMutationRows {
		t.Fatalf("privacy adapter label mutations: declared=%d actual=%d required=%d",
			document.ExpectedLabelMutationCount, len(document.LabelMutations), privacyAdapterLabelMutationRows)
	}
	if document.ExpectedViewportCount != privacyAdapterViewportRows || len(document.Viewport) != privacyAdapterViewportRows {
		t.Fatalf("privacy adapter viewport rows: declared=%d actual=%d required=%d",
			document.ExpectedViewportCount, len(document.Viewport), privacyAdapterViewportRows)
	}
	viewportMutations := 0
	for _, row := range document.Viewport {
		if strings.TrimSpace(row.Name) == "" || row.Width != 80 || row.Height != 18 || len(row.WantContains) == 0 || len(row.WantMissing) == 0 {
			t.Fatalf("privacy adapter viewport row is incomplete: %#v", row)
		}
		if row.MutationProbe {
			viewportMutations++
		}
	}
	if document.ExpectedViewportMutationCount != privacyAdapterViewportMutations || viewportMutations != privacyAdapterViewportMutations {
		t.Fatalf("privacy adapter viewport mutations: declared=%d actual=%d required=%d",
			document.ExpectedViewportMutationCount, viewportMutations, privacyAdapterViewportMutations)
	}
	if document.ExpectedLicenseCount != privacyAdapterLicenseRows || len(document.Licenses) != privacyAdapterLicenseRows {
		t.Fatalf("privacy adapter licenses: declared=%d actual=%d required=%d",
			document.ExpectedLicenseCount, len(document.Licenses), privacyAdapterLicenseRows)
	}
	seenSamples := map[string]bool{}
	for _, row := range document.PrivacySamples {
		if strings.TrimSpace(row.Name) == "" || seenSamples[row.Name] || strings.TrimSpace(row.Before) == "" {
			t.Fatalf("privacy adapter sample is incomplete or duplicated: %#v", row)
		}
		seenSamples[row.Name] = true
		if err := row.Category.Validate(); err != nil {
			t.Fatalf("privacy adapter sample %q category: %v", row.Name, err)
		}
		if row.CategoryLabel == "" || row.CategoryLabel != row.Category.String() {
			t.Fatalf("privacy adapter sample %q label=%q, want %q", row.Name, row.CategoryLabel, row.Category.String())
		}
	}
	seenFailures := map[string]bool{}
	for _, row := range document.Failures {
		if strings.TrimSpace(row.Name) == "" || seenFailures[row.Name] || !row.Kind.valid() || len(row.WantContains) == 0 {
			t.Fatalf("privacy adapter failure is incomplete or duplicated: %#v", row)
		}
		seenFailures[row.Name] = true
	}
	return document
}

type fixturePrivacyRedactor struct {
	categories map[string]redact.Category
	unchanged  bool
}

func (r fixturePrivacyRedactor) Detect(input string) []redact.Match {
	category, ok := r.categories[input]
	if !ok {
		return nil
	}
	return []redact.Match{{Category: category, MatchedText: input, Length: len(input)}}
}

func (r fixturePrivacyRedactor) RedactText(input string) string {
	if r.unchanged {
		return input
	}
	return "<synthetic-redaction>"
}

func TestPrivacyExampleAdapterFailsClosedThroughMountedFlow(t *testing.T) {
	document := loadPrivacyAdapterFixture(t)
	for _, row := range document.Failures {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			samples := make([]privacyExampleSample, 0, len(document.PrivacySamples))
			categories := make(map[string]redact.Category, len(document.PrivacySamples))
			for _, fixture := range document.PrivacySamples {
				samples = append(samples, privacyExampleSample{Category: fixture.Category, Before: fixture.Before})
				categories[fixture.Before] = fixture.Category
			}
			redactor := fixturePrivacyRedactor{categories: categories}
			factory := privacyRedactorFactory(func(redact.RedactionLevel) (privacyTextRedactor, error) {
				return redactor, nil
			})
			switch row.Kind {
			case adapterFailureConstructor:
				factory = func(redact.RedactionLevel) (privacyTextRedactor, error) {
					return nil, errors.New("synthetic constructor failure")
				}
			case adapterFailureUnknown:
				samples[0].Category = redact.Category("unknown")
			case adapterFailureMissing:
				samples = samples[:len(samples)-1]
			case adapterFailureUnchanged:
				redactor.unchanged = true
				factory = func(redact.RedactionLevel) (privacyTextRedactor, error) { return redactor, nil }
			}

			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
				t.Fatalf("seed privacy adapter config: %v", err)
			}
			draft, err := settings.NewDraft(path, config.BaseConfig())
			if err != nil {
				t.Fatalf("open privacy adapter draft: %v", err)
			}
			registry := settings.Registry{Sections: []settings.Section{{
				Key:   "privacy",
				Title: "privacy",
				Guide: &settings.Guide{Example: privacyGuideExample(samples, factory)},
				Fields: []settings.Field{settings.Info("privacy-field", func(*settings.Draft) string {
					return "privacy field remains mounted"
				})},
			}}}
			flow := settings.NewFlow(theme.New(theme.ModeDark), registry, draft)
			flow.SetSize(180, 40)
			view := flow.View()
			for _, want := range row.WantContains {
				if !strings.Contains(view, want) {
					t.Errorf("mounted fail-closed privacy example does not contain %q:\n%s", want, view)
				}
			}
			if !strings.Contains(view, "privacy field remains mounted") {
				t.Errorf("privacy example error displaced the canonical field instead of framing it:\n%s", view)
			}
			if strings.Contains(view, "synthetic-redaction") {
				t.Errorf("privacy example rendered invented success output after %s:\n%s", row.Kind, view)
			}
		})
	}
}
