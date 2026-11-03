package config_test

import (
	"embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
)

//go:embed testdata/custom_pattern_syntax*.yaml
var customPatternSyntaxFixtures embed.FS

type customPatternSyntaxCase struct {
	Name         string `yaml:"name"`
	Pattern      string `yaml:"pattern"`
	WantError    string `yaml:"wantError"`
	Input        string `yaml:"input"`
	WantRedacted string `yaml:"wantRedacted"`
}

func loadCustomPatternSyntaxFixtures(t *testing.T) []customPatternSyntaxCase {
	t.Helper()
	data, err := customPatternSyntaxFixtures.ReadFile("testdata/custom_pattern_syntax.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []customPatternSyntaxCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(data, &doc); err != nil {
		t.Fatal(err)
	}
	data, err = customPatternSyntaxFixtures.ReadFile("testdata/custom_pattern_syntax.manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(data, "custom pattern syntax")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range doc.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "custom pattern syntax"); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func TestParseCustomPatternExecutorSyntax(t *testing.T) {
	t.Parallel()
	for _, c := range loadCustomPatternSyntaxFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			// Quote with YAML single quotes so escapes reach the pattern unchanged.
			document := "version: 1\nredaction:\n  custom_patterns:\n    - id: grammar-probe\n      category: project\n      replacement: '<PROJECT_ALIAS>'\n      pattern: '" + strings.ReplaceAll(c.Pattern, "'", "''") + "'\n"
			cfg, err := config.Parse([]byte(document))
			if c.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), c.WantError) {
					t.Fatalf("Parse pattern %q: %v, want error containing %q", c.Pattern, err, c.WantError)
				}
				// Independently prove the pinned executor also refuses the pattern.
				_, err = redact.NewRedactor(redact.Standard, []redact.UserPattern{{ID: "grammar-probe", Category: redact.CategoryProject, Pattern: c.Pattern}}, redact.XDGPaths{})
				if err == nil {
					t.Fatalf("redact executor accepted refused pattern %q", c.Pattern)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			patterns, err := config.CustomPatternsToUserPatterns(cfg.Redaction.CustomPatterns)
			if err != nil {
				t.Fatal(err)
			}
			executor, err := redact.NewRedactor(redact.Standard, patterns, redact.XDGPaths{})
			if err != nil {
				t.Fatalf("executor rejected a configured pattern: %v", err)
			}
			if got := executor.RedactText(c.Input); got != c.WantRedacted {
				t.Fatalf("RedactText(%q) = %q, want %q", c.Input, got, c.WantRedacted)
			}
		})
	}
}
