package main

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/cli/usage_and_exit.yaml
var usageAndExitYAML []byte

//go:embed testdata/cli/usage_and_exit.manifest.yaml
var usageAndExitManifestYAML []byte

// usageBlockMarker is the heading Cobra's usage template opens with.
const usageBlockMarker = "Usage:"

// printedUsageOnError reports whether a failed run printed the usage block.
// Cobra writes that block with Println, which goes to the command's out writer
// when one is set (stderr only when none is), so the check reads the combined
// output. A successful run that prints help is not a usage dump.
func printedUsageOnError(output string, err error) bool {
	return err != nil && strings.Contains(output, usageBlockMarker)
}

type usageAndExitCase struct {
	Name           string            `yaml:"name"`
	Args           []string          `yaml:"args"`
	WantExit       defaults.ExitCode `yaml:"want_exit"`
	WantUsage      bool              `yaml:"want_usage"`
	ErrorContains  []string          `yaml:"error_contains"`
	OutputContains []string          `yaml:"output_contains"`
	Mutation       string            `yaml:"mutation"`
}

type usageAndExitFixture struct {
	Cases []usageAndExitCase `yaml:"cases"`
}

func decodeUsageAndExitYAML(label string, data []byte, into any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		return errors.New("decode " + label + ": " + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New(label + " must contain exactly one YAML document")
	}
	return nil
}

func loadUsageAndExitFixture(t *testing.T) usageAndExitFixture {
	t.Helper()
	var fixture usageAndExitFixture
	if err := decodeUsageAndExitYAML("usage and exit fixture", usageAndExitYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := decodeUsageAndExitYAML("usage and exit manifest", usageAndExitManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if present[fixtureCase.Name] {
			t.Fatalf("usage and exit case %q is declared twice", fixtureCase.Name)
		}
		if len(fixtureCase.Args) == 0 || fixtureCase.Mutation == "" {
			t.Fatalf("usage and exit case %q needs args and a mutation", fixtureCase.Name)
		}
		present[fixtureCase.Name] = true
	}
	for _, required := range manifest.RequiredNames {
		if !present[required] {
			t.Errorf("required usage and exit case %q is missing from the fixture", required)
		}
	}
	return fixture
}

// TestUsageAndExitFixtureIsStrict rejects an unknown field and a trailing YAML
// document, so a misspelled case cannot silently stop running.
func TestUsageAndExitFixtureIsStrict(t *testing.T) {
	t.Parallel()
	var fixture usageAndExitFixture
	unknownField := append([]byte("unexpected_fixture_field: true\n"), usageAndExitYAML...)
	if err := decodeUsageAndExitYAML("fixture", unknownField, &fixture); err == nil || !strings.Contains(err.Error(), "field unexpected_fixture_field not found") {
		t.Fatalf("unknown field error = %v, want strict field rejection", err)
	}
	trailing := append(append([]byte{}, usageAndExitYAML...), []byte("\n---\nunexpected: document\n")...)
	if err := decodeUsageAndExitYAML("fixture", trailing, &fixture); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error = %v, want single-document rejection", err)
	}
}

// runRootIsolated executes the production root with the XDG overrides pointed
// at a fresh temporary directory and returns its combined output.
func runRootIsolated(t *testing.T, root *cobra.Command, args []string) (output string, err error) {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--config-dir", dir, "--data-dir", dir, "--state-dir", dir}, args...))
	err = root.Execute()
	return out.String(), err
}

// TestUsageAndExit drives every fixture case through buildRootCommand and
// checks the exit status, whether the usage block was printed, and the error.
func TestUsageAndExit(t *testing.T) {
	t.Parallel()
	for _, fixtureCase := range loadUsageAndExitFixture(t).Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			output, err := runRootIsolated(t, buildRootCommand(), fixtureCase.Args)

			gotExit := defaults.ExitOK
			if err != nil {
				gotExit = exitCodeFor(err)
			}
			if gotExit != fixtureCase.WantExit {
				t.Errorf("exit = %v, want %v (err: %v; red under: %s)", gotExit, fixtureCase.WantExit, err, fixtureCase.Mutation)
			}
			if gotUsage := printedUsageOnError(output, err); gotUsage != fixtureCase.WantUsage {
				t.Errorf("usage printed = %v, want %v (red under: %s)\noutput:\n%s", gotUsage, fixtureCase.WantUsage, fixtureCase.Mutation, output)
			}
			if len(fixtureCase.ErrorContains) == 0 && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			for _, substr := range fixtureCase.ErrorContains {
				if err == nil || !strings.Contains(err.Error(), substr) {
					t.Errorf("error = %v, want it to contain %q (red under: %s)", err, substr, fixtureCase.Mutation)
				}
			}
			for _, substr := range fixtureCase.OutputContains {
				if !strings.Contains(output, substr) {
					t.Errorf("output missing %q (red under: %s)\noutput:\n%s", substr, fixtureCase.Mutation, output)
				}
			}
		})
	}
}

// TestRequiredFlagErrorKeepsUsage pins the one invocation check Cobra runs
// after the root pre-run silences usage: a missing required flag is still an
// invocation error and must print the usage block. No production command marks
// a flag required today, so the case attaches a probe command to the
// production root.
func TestRequiredFlagErrorKeepsUsage(t *testing.T) {
	t.Parallel()
	root := buildRootCommand()
	probe := &cobra.Command{
		Use:  "required-flag-probe",
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	probe.Flags().String("needed", "", "a required flag")
	if err := probe.MarkFlagRequired("needed"); err != nil {
		t.Fatal(err)
	}
	root.AddCommand(probe)

	output, err := runRootIsolated(t, root, []string{"required-flag-probe"})
	if err == nil || !strings.Contains(err.Error(), `required flag(s) "needed" not set`) {
		t.Fatalf("err = %v, want the required-flag refusal", err)
	}
	if !printedUsageOnError(output, err) {
		t.Errorf("no usage block for a missing required flag:\n%s", output)
	}
}
