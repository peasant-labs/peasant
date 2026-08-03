package main

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/config_paths.yaml
var configPathFixtureYAML []byte

type configPathFixtures struct {
	DeclaredRows int                 `yaml:"declared_rows"`
	Cases        []configPathFixture `yaml:"cases"`
}

type configPathFixture struct {
	Name      string `yaml:"name"`
	ConfigDir string `yaml:"config_dir"`
	Config    string `yaml:"config"`
	Expected  string `yaml:"expected"`
}

func loadConfigPathFixtures(t *testing.T) configPathFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(configPathFixtureYAML))
	decoder.KnownFields(true)
	var fixtures configPathFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode config path fixture with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("config path fixture must contain one YAML document: %v", err)
	}
	if fixtures.DeclaredRows != len(fixtures.Cases) || fixtures.DeclaredRows < 2 {
		t.Fatalf("config path fixture row guard failed: declared=%d actual=%d", fixtures.DeclaredRows, len(fixtures.Cases))
	}
	observed := make([]configPathInput, 0, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		testutil.RequireFixtureFields(t, "config path", fixture.Name, []testutil.FixtureField{
			{Key: "name", Value: fixture.Name},
			{Key: "config_dir", Value: fixture.ConfigDir},
			{Key: "expected", Value: fixture.Expected},
		})
		observed = append(observed, fixture.resolutionInput())
	}
	// The two rows encode DIFFERENT rules — deriving the file from --config-dir,
	// and --config winning over it — so losing either loses a rule, not just a
	// data point. The floor above catches the count dropping; this catches the
	// rule disappearing, and it is derived from the row rather than declared.
	testutil.RequireClosedSetCoverage(t, "config path", "resolution input", allConfigPathInputs, observed)
	return fixtures
}

// configPathInput is the closed set of inputs config-path resolution has to
// honour, computed from the row's own flags.
type configPathInput string

const (
	configPathFromConfigDir configPathInput = "derived-from-config-dir"
	configPathFromExplicit  configPathInput = "explicit-config-wins"
)

var allConfigPathInputs = []configPathInput{configPathFromConfigDir, configPathFromExplicit}

func (f configPathFixture) resolutionInput() configPathInput {
	if f.Config != "" {
		return configPathFromExplicit
	}
	return configPathFromConfigDir
}

func TestBuildKickstartCommand_ResolvesMountedConfigPath(t *testing.T) {
	for _, fixture := range loadConfigPathFixtures(t).Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			root := &cobra.Command{Use: "peasant"}
			root.PersistentFlags().String("config", "default.yaml", "")
			root.PersistentFlags().String("config-dir", "", "")
			kickstart := BuildKickstartCommand()
			root.AddCommand(kickstart)
			if err := root.PersistentFlags().Set("config-dir", fixture.ConfigDir); err != nil {
				t.Fatalf("set --config-dir: %v", err)
			}
			if fixture.Config != "" {
				if err := root.PersistentFlags().Set("config", fixture.Config); err != nil {
					t.Fatalf("set --config: %v", err)
				}
			}
			if got := resolveConfigPath(kickstart); got != fixture.Expected {
				t.Fatalf("mounted kickstart config path = %q, want %q", got, fixture.Expected)
			}
		})
	}
}
