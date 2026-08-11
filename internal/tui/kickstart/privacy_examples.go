package kickstart

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/redact"
)

// privacyExampleSample is synthetic text paired with the semantic category it
// must exercise. Samples are safe to compile into the binary and must never be
// copied from a real transcript or local path.
type privacyExampleSample struct {
	Category redact.Category
	Before   string
}

// privacyTextRedactor is the smallest external redactor surface the guide needs.
// Production uses github.com/peasant-labs/redact; tests may replace only this
// external boundary to prove fail-closed handling.
type privacyTextRedactor interface {
	Detect(string) []redact.Match
	RedactText(string) string
}

type privacyRedactorFactory func(redact.RedactionLevel) (privacyTextRedactor, error)

var standardPrivacySamples = []privacyExampleSample{
	{Category: redact.CategorySecrets, Before: "sk-ant-api03-EXAMPLEKEY0000000000000"},
	{Category: redact.CategoryPII, Before: "user@example.com"},
	{Category: redact.CategoryPaths, Before: "/Users/example/projects/sample/"},
	{Category: redact.CategoryProject, Before: `"github.com/example-org/example-repo"`},
}

func realPrivacyRedactor(level redact.RedactionLevel) (privacyTextRedactor, error) {
	return redact.NewRedactor(level, nil, redact.XDGPaths{})
}

// privacyGuideExample returns the Guide boundary that renders Standard's real
// before/after behavior. The draft is deliberately not used to weaken the
// level: Standard is the privacy policy offered by this guided flow.
func privacyGuideExample(samples []privacyExampleSample, factory privacyRedactorFactory) settings.GuideExampleFunc {
	return func(_ *settings.Draft) ([]settings.GuideExampleLine, error) {
		return renderPrivacyExamples(redact.Standard, samples, factory)
	}
}

func renderPrivacyExamples(level redact.RedactionLevel, samples []privacyExampleSample, factory privacyRedactorFactory) ([]settings.GuideExampleLine, error) {
	if factory == nil {
		return nil, privacyExampleError(
			"the Standard privacy example redactor cannot be constructed",
			"the redactor factory boundary is nil",
			"wire the real github.com/peasant-labs/redact constructor and retry kickstart")
	}

	claimed := redact.AllCategories()
	seen := make(map[redact.Category]int, len(claimed))
	for index, sample := range samples {
		if err := sample.Category.Validate(); err != nil {
			return nil, privacyExampleError(
				fmt.Sprintf("synthetic privacy sample %d declares unknown category %q", index, sample.Category),
				err.Error(),
				"assign the sample to secrets, pii, paths, or project before rendering the guide")
		}
		if strings.TrimSpace(sample.Before) == "" {
			return nil, privacyExampleError(
				fmt.Sprintf("synthetic privacy sample for %s has no input", sample.Category),
				"an empty sample cannot prove that a redaction rule fires",
				"add safe synthetic input that exercises the declared category")
		}
		seen[sample.Category]++
	}
	for _, category := range claimed {
		if seen[category] != 1 {
			return nil, privacyExampleError(
				fmt.Sprintf("Standard privacy guidance has %d samples for claimed category %s", seen[category], category),
				"every claimed category must have exactly one independent demonstration",
				"restore one safe synthetic sample for each category returned by redact.AllCategories")
		}
	}

	redactor, err := factory(level)
	if err != nil {
		return nil, privacyExampleError(
			fmt.Sprintf("the %s privacy example redactor could not be constructed", level),
			err.Error(),
			"repair the redactor configuration or constructor before showing privacy claims")
	}
	if redactor == nil {
		return nil, privacyExampleError(
			fmt.Sprintf("the %s privacy example redactor is nil", level),
			"the constructor returned no usable redactor without reporting an error",
			"return a working redactor or a concrete constructor error")
	}

	lines := make([]settings.GuideExampleLine, 0, len(samples)*3)
	for _, sample := range samples {
		categoryLabel, err := canonicalPrivacyCategoryLabel(sample.Category)
		if err != nil {
			return nil, err
		}
		matches := redactor.Detect(sample.Before)
		categoryFound := false
		for _, match := range matches {
			if err := match.Category.Validate(); err != nil {
				return nil, privacyExampleError(
					fmt.Sprintf("the %s redactor returned unknown category %q", level, match.Category),
					err.Error(),
					"repair the redaction rule category before rendering privacy guidance")
			}
			categoryFound = categoryFound || match.Category == sample.Category
		}
		if !categoryFound {
			return nil, privacyExampleError(
				fmt.Sprintf("the synthetic %s sample did not trigger its claimed category", sample.Category),
				"the real redactor detection result contains no matching category",
				"update the safe sample if its rule shape changed, or repair the missing redaction rule")
		}
		after := redactor.RedactText(sample.Before)
		if after == sample.Before {
			return nil, privacyExampleError(
				fmt.Sprintf("the synthetic %s sample remained unchanged at the %s level", sample.Category, level),
				"detection claimed the category but the real redaction output did not change",
				"repair the redactor replacement behavior instead of hard-coding example output")
		}
		lines = append(lines,
			settings.GuideExampleLine{Kind: settings.GuideExampleLineLabel, Text: categoryLabel.String()},
			settings.GuideExampleLine{Kind: settings.GuideExampleLineBefore, Text: sample.Before},
			settings.GuideExampleLine{Kind: settings.GuideExampleLineAfter, Text: after},
		)
	}
	return lines, nil
}

func canonicalPrivacyCategoryLabel(category redact.Category) (redact.CategoryString, error) {
	if err := category.Validate(); err != nil {
		return "", privacyExampleError(
			fmt.Sprintf("privacy guidance cannot render unknown category %q", category),
			err.Error(),
			"repair the redaction rule category before rendering privacy guidance")
	}
	label := category.String()
	if label == "" {
		return "", privacyExampleError(
			fmt.Sprintf("privacy guidance has no canonical label for category %q", category),
			"the validated category resolved to an empty public CategoryString",
			"restore the canonical CREDENTIAL, PII, PATH, or INTERNAL category mapping")
	}
	return label, nil
}

func privacyExampleError(what, why, fix string) error {
	return fmt.Errorf(
		"privacy example unavailable.\n"+
			"what: %s.\n"+
			"why: %s.\n"+
			"where: kickstart privacy guidance.\n"+
			"when: while preparing synthetic before/after examples before the privacy choice is shown.\n"+
			"means: no example output is shown, so kickstart does not claim an unverified privacy result.\n"+
			"fix: %s.",
		what, why, fix)
}
