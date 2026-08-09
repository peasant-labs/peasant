package kickstart_test

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

const (
	expectedPrivacySampleRows        = 4
	expectedPrivacyFailureRows       = 4
	expectedPrivacyLabelMutationRows = 3
	expectedLicenseRows              = 4
)

type privacyFailureKind string

const (
	privacyFailureConstructor privacyFailureKind = "constructor-error"
	privacyFailureUnknown     privacyFailureKind = "unknown-category"
	privacyFailureMissing     privacyFailureKind = "missing-category"
	privacyFailureUnchanged   privacyFailureKind = "unchanged-output"
)

func (k privacyFailureKind) valid() bool {
	switch k {
	case privacyFailureConstructor, privacyFailureUnknown, privacyFailureMissing, privacyFailureUnchanged:
		return true
	default:
		return false
	}
}

type privacySampleFixture struct {
	Name          string                `yaml:"name"`
	Category      redact.Category       `yaml:"category"`
	CategoryLabel redact.CategoryString `yaml:"categoryLabel"`
	Before        string                `yaml:"before"`
}

type privacyLabelMutationKind string

const (
	privacyLabelMutationRaw     privacyLabelMutationKind = "raw"
	privacyLabelMutationInvalid privacyLabelMutationKind = "invalid"
	privacyLabelMutationZero    privacyLabelMutationKind = "zero"
)

func (k privacyLabelMutationKind) valid() bool {
	return k == privacyLabelMutationRaw || k == privacyLabelMutationInvalid || k == privacyLabelMutationZero
}

type privacyLabelMutationFixture struct {
	Name          string                   `yaml:"name"`
	Kind          privacyLabelMutationKind `yaml:"kind"`
	SampleName    string                   `yaml:"sampleName"`
	CategoryLabel redact.CategoryString    `yaml:"categoryLabel"`
}

type privacyFailureFixture struct {
	Name         string             `yaml:"name"`
	Kind         privacyFailureKind `yaml:"kind"`
	WantContains []string           `yaml:"wantContains"`
}

type licenseFixture struct {
	Name      string         `yaml:"name"`
	Value     config.License `yaml:"value"`
	Label     string         `yaml:"label"`
	Guidance  string         `yaml:"guidance"`
	IsDefault bool           `yaml:"isDefault"`
}

type privacyLicenseDocument struct {
	ExpectedPrivacySampleCount int                           `yaml:"expectedPrivacySampleCount"`
	PrivacySamples             []privacySampleFixture        `yaml:"privacySamples"`
	ExpectedFailureCount       int                           `yaml:"expectedFailureCount"`
	Failures                   []privacyFailureFixture       `yaml:"failures"`
	ExpectedLabelMutationCount int                           `yaml:"expectedLabelMutationCount"`
	LabelMutations             []privacyLabelMutationFixture `yaml:"labelMutations"`
	ExpectedLicenseCount       int                           `yaml:"expectedLicenseCount"`
	Licenses                   []licenseFixture              `yaml:"licenses"`
}

func validatePrivacySampleCategoryLabel(row privacySampleFixture) error {
	if err := row.Category.Validate(); err != nil {
		return fmt.Errorf("privacy sample %q category: %w", row.Name, err)
	}
	canonical := row.Category.String()
	if canonical == "" {
		return fmt.Errorf("privacy sample %q category %q has no canonical public label", row.Name, row.Category)
	}
	if row.CategoryLabel == "" {
		return fmt.Errorf("privacy sample %q has an empty public category label", row.Name)
	}
	if row.CategoryLabel != canonical {
		return fmt.Errorf("privacy sample %q label=%q, want canonical %q", row.Name, row.CategoryLabel, canonical)
	}
	return nil
}

//go:embed testdata/guided/privacy_license.yaml
var privacyLicenseData []byte

func loadPrivacyLicenseDocument(t *testing.T) privacyLicenseDocument {
	t.Helper()
	var document privacyLicenseDocument
	decodeSingleKnownFieldsDocument(t, "testdata/guided/privacy_license.yaml", privacyLicenseData, &document)
	if document.ExpectedPrivacySampleCount != expectedPrivacySampleRows || len(document.PrivacySamples) != expectedPrivacySampleRows {
		t.Fatalf("privacy samples: declared=%d actual=%d required=%d",
			document.ExpectedPrivacySampleCount, len(document.PrivacySamples), expectedPrivacySampleRows)
	}
	if document.ExpectedFailureCount != expectedPrivacyFailureRows || len(document.Failures) != expectedPrivacyFailureRows {
		t.Fatalf("privacy failures: declared=%d actual=%d required=%d",
			document.ExpectedFailureCount, len(document.Failures), expectedPrivacyFailureRows)
	}
	if document.ExpectedLabelMutationCount != expectedPrivacyLabelMutationRows || len(document.LabelMutations) != expectedPrivacyLabelMutationRows {
		t.Fatalf("privacy label mutations: declared=%d actual=%d required=%d",
			document.ExpectedLabelMutationCount, len(document.LabelMutations), expectedPrivacyLabelMutationRows)
	}
	if document.ExpectedLicenseCount != expectedLicenseRows || len(document.Licenses) != expectedLicenseRows {
		t.Fatalf("licenses: declared=%d actual=%d required=%d",
			document.ExpectedLicenseCount, len(document.Licenses), expectedLicenseRows)
	}

	privacyNames := map[string]bool{}
	seenCategories := map[redact.Category]bool{}
	for _, row := range document.PrivacySamples {
		if strings.TrimSpace(row.Name) == "" || privacyNames[row.Name] || strings.TrimSpace(row.Before) == "" {
			t.Fatalf("privacy sample is missing a unique name or synthetic input: %#v", row)
		}
		privacyNames[row.Name] = true
		if err := validatePrivacySampleCategoryLabel(row); err != nil {
			t.Fatal(err)
		}
		if seenCategories[row.Category] {
			t.Fatalf("privacy category %q has more than one claimed sample", row.Category)
		}
		seenCategories[row.Category] = true
	}
	for _, category := range redact.AllCategories() {
		if !seenCategories[category] {
			t.Fatalf("privacy fixture has no sample for claimed category %q", category)
		}
	}

	failureNames := map[string]bool{}
	for _, row := range document.Failures {
		if strings.TrimSpace(row.Name) == "" || failureNames[row.Name] || !row.Kind.valid() || len(row.WantContains) == 0 {
			t.Fatalf("privacy failure row is incomplete or duplicated: %#v", row)
		}
		failureNames[row.Name] = true
	}

	mutationNames := map[string]bool{}
	mutationKinds := map[privacyLabelMutationKind]bool{}
	for _, row := range document.LabelMutations {
		if strings.TrimSpace(row.Name) == "" || mutationNames[row.Name] || !row.Kind.valid() ||
			strings.TrimSpace(row.SampleName) == "" || mutationKinds[row.Kind] || !privacyNames[row.SampleName] {
			t.Fatalf("privacy label mutation row is incomplete or duplicated: %#v", row)
		}
		mutationNames[row.Name] = true
		mutationKinds[row.Kind] = true
		switch row.Kind {
		case privacyLabelMutationRaw:
			if row.CategoryLabel == "" {
				t.Fatalf("raw privacy label mutation %q is empty", row.Name)
			}
		case privacyLabelMutationInvalid:
			if row.CategoryLabel == "" {
				t.Fatalf("invalid privacy label mutation %q is empty", row.Name)
			}
		case privacyLabelMutationZero:
			if row.CategoryLabel != "" {
				t.Fatalf("zero privacy label mutation %q has label %q", row.Name, row.CategoryLabel)
			}
		}
	}

	licenseNames := map[string]bool{}
	defaultRows := 0
	for _, row := range document.Licenses {
		if strings.TrimSpace(row.Name) == "" || licenseNames[row.Name] || strings.TrimSpace(row.Label) == "" || strings.TrimSpace(row.Guidance) == "" {
			t.Fatalf("license row is incomplete or duplicated: %#v", row)
		}
		licenseNames[row.Name] = true
		if row.IsDefault {
			defaultRows++
			if row.Value != "" {
				t.Fatalf("license row %q marks non-empty value %q as the default", row.Name, row.Value)
			}
		}
		if row.Value != "" {
			known := false
			for _, candidate := range schema.AllLicenses {
				known = known || candidate == row.Value
			}
			if !known {
				t.Fatalf("license row %q has unsupported value %q", row.Name, row.Value)
			}
		}
	}
	if defaultRows != 1 {
		t.Fatalf("license fixture marks %d defaults, want exactly one no-license default", defaultRows)
	}
	return document
}

func TestPrivacyGuideUsesRealStandardRedactor(t *testing.T) {
	document := loadPrivacyLicenseDocument(t)
	draft, _ := newGuidedDraft(t)
	section := findSection(t, kickstart.BuildRegistry(kickstart.Options{
		Source: scannerfix.NewFixtureTreeSource("standard"),
	}), kickstart.SectionPrivacy)
	if section.Guide == nil || section.Guide.Example == nil {
		t.Fatal("canonical privacy section has no live redactor example provider")
	}

	view, err := section.Guide.Example(theme.New(theme.ModeDark), draft)
	if err != nil {
		t.Fatalf("render real privacy guide example: %v", err)
	}
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("construct independent Standard redactor oracle: %v", err)
	}
	for _, row := range document.PrivacySamples {
		matches := redactor.Detect(row.Before)
		claimed := false
		for _, match := range matches {
			if err := match.Category.Validate(); err != nil {
				t.Fatalf("real redactor returned invalid category for %q: %v", row.Name, err)
			}
			claimed = claimed || match.Category == row.Category
		}
		if !claimed {
			t.Fatalf("synthetic sample %q does not exercise claimed category %q", row.Name, row.Category)
		}
		after := redactor.RedactText(row.Before)
		if after == row.Before {
			t.Fatalf("real Standard redactor left synthetic %s sample unchanged", row.Category)
		}
		for _, want := range []string{row.CategoryLabel.String(), "before: " + row.Before, "after: " + after} {
			if !strings.Contains(view, want) {
				t.Errorf("privacy example does not contain runtime-derived %q:\n%s", want, view)
			}
		}
		for _, line := range strings.Split(view, "\n") {
			if line == string(row.Category) {
				t.Errorf("privacy example renders raw storage category %q instead of %q:\n%s", row.Category, row.CategoryLabel, view)
			}
		}
	}
}

func TestPrivacyCategoryLabelFixtureMutationsFailClosed(t *testing.T) {
	document := loadPrivacyLicenseDocument(t)
	byName := make(map[string]privacySampleFixture, len(document.PrivacySamples))
	for _, sample := range document.PrivacySamples {
		byName[sample.Name] = sample
	}
	for _, mutation := range document.LabelMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			sample := byName[mutation.SampleName]
			sample.CategoryLabel = mutation.CategoryLabel
			if err := validatePrivacySampleCategoryLabel(sample); err == nil {
				t.Fatalf("privacy label mutation %q unexpectedly passed canonical validation", mutation.Name)
			}
		})
	}
}

func TestLicenseGuidanceUsesLoadedDraftAndNoLicenseDefault(t *testing.T) {
	document := loadPrivacyLicenseDocument(t)
	if got := config.BaseConfig().Push.License; got != "" {
		t.Fatalf("base config license = %q, want empty no-license default", got)
	}
	for _, row := range document.Licenses {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			loaded := config.BaseConfig()
			loaded.Push.License = row.Value
			if err := config.SaveAtomic(path, loaded); err != nil {
				t.Fatalf("seed loaded license: %v", err)
			}
			draft, err := settings.NewDraft(path, loaded)
			if err != nil {
				t.Fatalf("open loaded license draft: %v", err)
			}
			registry := kickstart.BuildRegistry(kickstart.Options{Source: scannerfix.NewFixtureTreeSource("standard")})
			flow := settings.NewFlow(theme.New(theme.ModeDark), registry, draft)
			flow.SetSize(180, 50)
			flow = advanceFlowToSection(t, flow, kickstart.SectionLicense, len(registry.Sections))
			view := stripRender(flow.View())
			for _, want := range []string{"(•) " + row.Label, row.Guidance} {
				if !strings.Contains(view, want) {
					t.Errorf("loaded license %q does not render %q as selected guidance:\n%s", row.Value, want, view)
				}
			}
		})
	}
}
