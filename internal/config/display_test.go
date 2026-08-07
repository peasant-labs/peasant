package config_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/theme_validity.yaml
var themeValidityFixtureData []byte

type themeValidityDocument struct {
	ExpectedCaseCount int                 `yaml:"expectedCaseCount"`
	Cases             []themeValidityCase `yaml:"cases"`
}

type themeValidityCase struct {
	Name  string `yaml:"name"`
	Theme string `yaml:"theme"`
	Want  bool   `yaml:"want"`
}

// loadThemeValidityFixture decodes and validates testdata/theme_validity.yaml,
// mirroring the embed+KnownFields+single-document+declared-count idiom
// level_phrases_test.go establishes in this same package.
func loadThemeValidityFixture(data []byte) (themeValidityDocument, error) {
	var doc themeValidityDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/theme_validity.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/theme_validity.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf(
			"testdata/theme_validity.yaml: expectedCaseCount=%d but found %d cases (and must be non-zero)",
			doc.ExpectedCaseCount, len(doc.Cases))
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			return doc, fmt.Errorf("testdata/theme_validity.yaml: case name %q is missing or duplicated", c.Name)
		}
		seen[c.Name] = true
	}
	return doc, nil
}

func TestTheme_IsValid(t *testing.T) {
	t.Parallel()
	doc, err := loadThemeValidityFixture(themeValidityFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			th := config.Theme(c.Theme)
			if got := th.IsValid(); got != c.Want {
				t.Errorf("Theme(%q).IsValid() = %v, want %v", c.Theme, got, c.Want)
			}
		})
	}
}

func TestBaseConfig_DisplayThemeDefaultsToDark(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	if cfg.Display.Theme != config.ThemeDark {
		t.Fatalf("BaseConfig().Display.Theme = %q, want %q", cfg.Display.Theme, config.ThemeDark)
	}
}

func TestParse_MissingDisplaySectionKeepsBaseConfigDefault(t *testing.T) {
	t.Parallel()
	// A config.yaml written before this feature existed has no "display:"
	// key at all. Parse starts from BaseConfig() and unmarshals onto it, so
	// an absent key must leave the preset default untouched rather than
	// zeroing it.
	cfg, err := config.Parse([]byte("version: 1\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Display.Theme != config.ThemeDark {
		t.Fatalf("Parse of a config with no display section: Display.Theme = %q, want default %q", cfg.Display.Theme, config.ThemeDark)
	}
}

func TestParse_RejectsUnknownDisplayTheme(t *testing.T) {
	t.Parallel()
	_, err := config.Parse([]byte("version: 1\ndisplay:\n  theme: solarized\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown display.theme") {
		t.Fatalf("error = %v, want rejection of an unknown display.theme", err)
	}
}

func TestParse_AcceptsExplicitLightTheme(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte("version: 1\ndisplay:\n  theme: light\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Display.Theme != config.ThemeLight {
		t.Fatalf("Display.Theme = %q, want %q", cfg.Display.Theme, config.ThemeLight)
	}
}

// TestSaveAtomic_DisplayTheme_RoundTrip proves the setting survives a real
// write-then-read cycle through the same SaveAtomic + Parse path
// `peasant kickstart` and any future TUI settings screen use, not just an
// in-memory struct comparison.
func TestSaveAtomic_DisplayTheme_RoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.BaseConfig()
	cfg.Display.Theme = config.ThemeLight

	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	loaded, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse saved config: %v", err)
	}
	if loaded.Display.Theme != config.ThemeLight {
		t.Fatalf("round-tripped Display.Theme = %q, want %q", loaded.Display.Theme, config.ThemeLight)
	}
}

// TestModeFromConfig_AgreesWithConfigTheme is the cross-package agreement
// theme.ModeFromConfig's doc comment promises: every valid config.Theme
// round-trips through theme.ModeFromConfig to the theme.Mode of the same
// name, and an invalid one is rejected the same way config.Theme.IsValid
// already rejects it - the two enums cannot silently drift apart at their
// string boundary.
func TestModeFromConfig_AgreesWithConfigTheme(t *testing.T) {
	t.Parallel()
	for _, configTheme := range []config.Theme{config.ThemeDark, config.ThemeLight} {
		mode, err := theme.ModeFromConfig(configTheme.String())
		if err != nil {
			t.Fatalf("theme.ModeFromConfig(%q): %v", configTheme, err)
		}
		if mode.String() != configTheme.String() {
			t.Fatalf("theme.ModeFromConfig(%q) = %q, want the same name", configTheme, mode)
		}
		if !mode.IsValid() {
			t.Fatalf("theme.ModeFromConfig(%q) returned an invalid Mode %q", configTheme, mode)
		}
	}
	if _, err := theme.ModeFromConfig("solarized"); err == nil {
		t.Fatal("theme.ModeFromConfig(\"solarized\") did not error for a value config.Theme.IsValid also rejects")
	}
}
