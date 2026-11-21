package clicommand_test

import (
	_ "embed"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/clicommand"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/render.yaml
var renderYAML []byte

type renderBinding struct {
	ConfigPath string `yaml:"configPath"`
	ConfigDir  string `yaml:"configDir"`
	DataDir    string `yaml:"dataDir"`
	StateDir   string `yaml:"stateDir"`
}

type renderCase struct {
	Name       string        `yaml:"name"`
	Binding    renderBinding `yaml:"binding"`
	Root       string        `yaml:"root"`
	Prefix     string        `yaml:"prefix"`
	ManualPush string        `yaml:"manualPush"`
}

type quoteCase struct {
	Name   string `yaml:"name"`
	Value  string `yaml:"value"`
	Quoted string `yaml:"quoted"`
}

type verbatimCase struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type renderFixture struct {
	Render          []renderCase   `yaml:"render"`
	Quote           []quoteCase    `yaml:"quote"`
	UnquoteVerbatim []verbatimCase `yaml:"unquoteVerbatim"`
}

func loadRenderFixture(t *testing.T) renderFixture {
	t.Helper()
	var f renderFixture
	if err := testutil.DecodeFixtureYAML(renderYAML, &f); err != nil {
		t.Fatalf("decode render fixture: %v", err)
	}
	if len(f.Render) == 0 || len(f.Quote) == 0 || len(f.UnquoteVerbatim) == 0 {
		t.Fatal("render fixture must carry render, quote, and unquoteVerbatim cases")
	}
	return f
}

func TestPrefixAndManualPushRenderTheBoundPaths(t *testing.T) {
	t.Parallel()
	for _, c := range loadRenderFixture(t).Render {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			binding := clicommand.Binding(c.Binding)
			if got := clicommand.Prefix(binding); got != c.Prefix {
				t.Errorf("Prefix = %q, want %q", got, c.Prefix)
			}
			if got := clicommand.ManualPush(c.Root, binding); got != c.ManualPush {
				t.Errorf("ManualPush = %q, want %q", got, c.ManualPush)
			}
			// The argument-list rendering of the same tokens carries every value
			// verbatim, so a path survives an exec-style caller untouched.
			argv := clicommand.RenderArgv(clicommand.BoundTokens(binding))
			for _, value := range []string{binding.ConfigPath, binding.ConfigDir, binding.DataDir, binding.StateDir} {
				if value != "" && !slices.Contains(argv, value) {
					t.Errorf("RenderArgv = %q, missing verbatim %q", argv, value)
				}
			}
		})
	}
}

func TestShellQuoteRoundTrips(t *testing.T) {
	t.Parallel()
	for _, c := range loadRenderFixture(t).Quote {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if got := clicommand.ShellQuote(c.Value); got != c.Quoted {
				t.Fatalf("ShellQuote(%q) = %q, want %q", c.Value, got, c.Quoted)
			}
			if got := clicommand.ShellUnquote(c.Quoted); got != c.Value {
				t.Fatalf("ShellUnquote(%q) = %q, want %q", c.Quoted, got, c.Value)
			}
		})
	}
}

func TestShellUnquoteLeavesForeignTextAlone(t *testing.T) {
	t.Parallel()
	for _, c := range loadRenderFixture(t).UnquoteVerbatim {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if got := clicommand.ShellUnquote(c.Value); got != c.Value {
				t.Fatalf("ShellUnquote(%q) = %q, want it unchanged", c.Value, got)
			}
		})
	}
}
