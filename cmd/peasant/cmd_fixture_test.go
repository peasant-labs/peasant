package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ---- YAML fixture schema ----

type fixtureFile struct {
	Commands []commandFixture `yaml:"commands"`
}

type commandFixture struct {
	Name        string          `yaml:"name"`
	Use         string          `yaml:"use"`
	Aliases     []string        `yaml:"aliases"`
	Flags       []flagFixture   `yaml:"flags"`
	Subcommands []subCmdFixture `yaml:"subcommands"`
	ExitCases   []exitFixture   `yaml:"exit_cases"`
}

type subCmdFixture struct {
	Use         string          `yaml:"use"`
	Flags       []flagFixture   `yaml:"flags"`
	Subcommands []subCmdFixture `yaml:"subcommands"`
	ExitCases   []exitFixture   `yaml:"exit_cases"`
}

type flagFixture struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	Default string `yaml:"default"`
	Hidden  bool   `yaml:"hidden"`
}

type exitFixture struct {
	Args           []string `yaml:"args"`
	ExpectError    bool     `yaml:"expect_error"`
	OutputContains []string `yaml:"output_contains"`
	ErrorContains  []string `yaml:"error_contains"`
}

// ---- builder registry (mirrors main.go) ----

// builderByName maps fixture Name to the corresponding BuildXxxCommand function.
var builderByName = map[string]func() *cobra.Command{
	"Harvest":   BuildHarvestCommand,
	"Web":       BuildWebCommand,
	"Push":      BuildPushCommand,
	"Metrics":   BuildMetricsCommand,
	"Models":    BuildModelsCommand,
	"Sessions":  BuildSessionsCommand,
	"TUI":       BuildTUICommand,
	"Version":   BuildVersionCommand,
	"Kickstart": BuildKickstartCommand,
	"Login":     BuildLoginCommand,
	"Logout":    BuildLogoutCommand,
	"Annotate":  BuildAnnotateCommand,
	"Export":    BuildExportCommand,
}

func loadFixture(t *testing.T) fixtureFile {
	t.Helper()
	data, err := os.ReadFile("testdata/commands.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f, err := decodeCommandFixture(data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

func decodeCommandFixture(data []byte) (fixtureFile, error) {
	var f fixtureFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		return fixtureFile{}, fmt.Errorf("decode command fixture with known fields: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fixtureFile{}, fmt.Errorf("command fixture must contain exactly one YAML document")
		}
		return fixtureFile{}, fmt.Errorf("decode trailing command fixture document: %w", err)
	}
	return f, nil
}

// ---- Tests ----

func TestFixture_StrictDecoder(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/commands.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	unknownField := append([]byte("unexpected_fixture_field: true\n"), data...)
	if _, err := decodeCommandFixture(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpected_fixture_field not found") {
		t.Fatalf("unknown field error = %v, want strict field rejection", err)
	}
	trailingDocument := append(append([]byte{}, data...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeCommandFixture(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error = %v, want single-document rejection", err)
	}
}

// TestFixture_AllBuildersHaveFixtures ensures every builder in the static
// registry has a corresponding fixture entry, and vice versa.
func TestFixture_AllBuildersHaveFixtures(t *testing.T) {
	t.Parallel()
	fixture := loadFixture(t)

	fixtureNames := make(map[string]bool, len(fixture.Commands))
	for _, cmd := range fixture.Commands {
		fixtureNames[cmd.Name] = true
	}

	for name := range builderByName {
		if !fixtureNames[name] {
			t.Errorf("builder %q has no fixture entry in commands.yaml", name)
		}
	}
	for _, cmd := range fixture.Commands {
		if _, ok := builderByName[cmd.Name]; !ok {
			t.Errorf("fixture %q has no matching builder function", cmd.Name)
		}
	}
}

// TestFixture_CommandStructure validates each command's Use field, aliases,
// flags, and subcommand tree against the YAML fixture.
func TestFixture_CommandStructure(t *testing.T) {
	t.Parallel()
	fixture := loadFixture(t)

	for _, fc := range fixture.Commands {
		fc := fc
		t.Run(fc.Name, func(t *testing.T) {
			t.Parallel()
			builder, ok := builderByName[fc.Name]
			if !ok {
				t.Fatalf("no builder for %q", fc.Name)
			}
			cmd := builder()

			// Verify Use field.
			if cmd.Name() != fc.Use {
				t.Errorf("Use: want %q, got %q", fc.Use, cmd.Name())
			}

			// Verify aliases.
			if len(fc.Aliases) > 0 {
				for _, alias := range fc.Aliases {
					if !containsString(cmd.Aliases, alias) {
						t.Errorf("missing alias %q; have %v", alias, cmd.Aliases)
					}
				}
			}

			// Verify flags on the command itself.
			verifyFlags(t, cmd, fc.Flags)

			// Verify subcommand tree.
			verifySubcommands(t, cmd, fc.Subcommands)
		})
	}
}

// TestFixture_ExitBehavior runs each exit_case from the fixture and validates
// the expected error/output behavior. Both top-level command exit_cases and
// nested subcommand exit_cases are exercised.
func TestFixture_ExitBehavior(t *testing.T) {
	t.Parallel()
	fixture := loadFixture(t)

	for _, fc := range fixture.Commands {
		fc := fc
		t.Run(fc.Name, func(t *testing.T) {
			t.Parallel()
			builder, ok := builderByName[fc.Name]
			if !ok {
				t.Fatalf("no builder for %q", fc.Name)
			}

			// Run top-level exit cases.
			runExitCases(t, fc.Use, builder, fc.ExitCases)

			// Run subcommand exit cases (one level deep, covering prune and similar).
			// The exit_case args for a subcommand start with the subcommand name,
			// e.g. args: ["prune"] → root.SetArgs(["annotate", "prune"]).
			for _, sc := range fc.Subcommands {
				sc := sc
				if len(sc.ExitCases) > 0 {
					t.Run(sc.Use, func(t *testing.T) {
						t.Parallel()
						runExitCases(t, fc.Use, builder, sc.ExitCases)
					})
				}
			}
		})
	}
}

// runExitCases executes the given exit cases against the built command.
// topUse is the name of the top-level subcommand (e.g. "annotate"), and
// ec.Args are appended to it: root.SetArgs([topUse] + ec.Args).
func runExitCases(t *testing.T, topUse string, builder func() *cobra.Command, cases []exitFixture) {
	t.Helper()
	for i, ec := range cases {
		ec := ec
		t.Run(strings.Join(ec.Args, "_"), func(t *testing.T) {
			t.Parallel()
			// Isolate config/data via per-invocation --data-dir/--config-dir
			// flags (parallel-safe; no t.Setenv of process-global XDG env).
			dir := t.TempDir()

			// Build a root command tree mimicking main().
			root := &cobra.Command{Use: "peasant"}
			root.PersistentFlags().String("config", "", "")
			root.PersistentFlags().String("data-dir", "", "")
			root.PersistentFlags().String("config-dir", "", "")
			root.AddCommand(builder())

			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, topUse}, ec.Args...))

			err := root.Execute()
			output := buf.String()

			if ec.ExpectError && err == nil {
				t.Errorf("exit_case[%d] %v: expected error, got nil; output: %s", i, ec.Args, output)
			}
			if !ec.ExpectError && err != nil {
				t.Errorf("exit_case[%d] %v: unexpected error: %v; output: %s", i, ec.Args, err, output)
			}

			for _, substr := range ec.OutputContains {
				if !strings.Contains(output, substr) {
					t.Errorf("exit_case[%d] %v: output missing %q; got: %s", i, ec.Args, substr, output)
				}
			}

			if err != nil {
				for _, substr := range ec.ErrorContains {
					if !strings.Contains(err.Error(), substr) {
						t.Errorf("exit_case[%d] %v: error missing %q; got: %v", i, ec.Args, substr, err)
					}
				}
			}
		})
	}
}

// ---- helpers ----

func verifyFlags(t *testing.T, cmd *cobra.Command, expected []flagFixture) {
	t.Helper()
	for _, ef := range expected {
		f := cmd.Flags().Lookup(ef.Name)
		if f == nil {
			t.Errorf("flag --%s not registered on %q", ef.Name, cmd.Name())
			continue
		}
		if f.Value.Type() != ef.Type {
			t.Errorf("flag --%s on %q: type want %q, got %q", ef.Name, cmd.Name(), ef.Type, f.Value.Type())
		}
		if f.DefValue != ef.Default {
			t.Errorf("flag --%s on %q: default want %q, got %q", ef.Name, cmd.Name(), ef.Default, f.DefValue)
		}
		if f.Hidden != ef.Hidden {
			t.Errorf("flag --%s on %q: hidden want %v, got %v", ef.Name, cmd.Name(), ef.Hidden, f.Hidden)
		}
	}
}

func verifySubcommands(t *testing.T, parent *cobra.Command, expected []subCmdFixture) {
	t.Helper()
	for _, es := range expected {
		var found *cobra.Command
		for _, c := range parent.Commands() {
			if c.Name() == es.Use {
				found = c
				break
			}
		}
		if found == nil {
			names := make([]string, 0, len(parent.Commands()))
			for _, c := range parent.Commands() {
				names = append(names, c.Name())
			}
			t.Errorf("subcommand %q not found under %q; have %v", es.Use, parent.Name(), names)
			continue
		}

		// Verify flags on subcommand.
		verifyFlags(t, found, es.Flags)

		// Recurse into nested subcommands.
		if len(es.Subcommands) > 0 {
			verifySubcommands(t, found, es.Subcommands)
		}
	}
}
