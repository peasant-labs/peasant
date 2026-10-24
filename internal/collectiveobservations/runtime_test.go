package collectiveobservations

import (
	_ "embed"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/runtime.yaml
var runtimeYAML []byte

func TestRuntimeFetchTimeoutConfiguration(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name    string `yaml:"name"`
			Unset   bool   `yaml:"unset"`
			Value   string `yaml:"value"`
			Want    string `yaml:"want"`
			Invalid bool   `yaml:"invalid"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(runtimeYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Setenv(EnvFetchTimeout, c.Value)
			if c.Unset {
				if err := os.Unsetenv(EnvFetchTimeout); err != nil {
					t.Fatal(err)
				}
			}
			got, err := RuntimeFetchTimeout()
			if c.Invalid {
				if err == nil || !strings.Contains(err.Error(), EnvFetchTimeout) || got != 0 {
					t.Fatalf("invalid runtime override accepted: %v %v", got, err)
				}
				return
			}
			want, parseErr := time.ParseDuration(c.Want)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if err != nil || got != want {
				t.Fatalf("runtime timeout=%v err=%v want=%v", got, err, want)
			}
		})
	}
}
