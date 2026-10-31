package main

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
)

//go:embed testdata/port_flags.yaml
var portFlagsYAML []byte

func TestPortFlagsValidateBeforeCommandsRun(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name  string `yaml:"name"`
			Raw   string `yaml:"raw"`
			Valid bool   `yaml:"valid"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(portFlagsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	builders := map[string]func() *cobra.Command{
		"web start": func() *cobra.Command { c, _, _ := BuildWebCommand().Find([]string{"start"}); return c },
		"web stop":  func() *cobra.Command { c, _, _ := BuildWebCommand().Find([]string{"stop"}); return c },
		"open":      BuildOpenCommand,
	}
	for name, build := range builders {
		for _, tc := range fixture.Cases {
			t.Run(name+"/"+tc.Name, func(t *testing.T) {
				t.Parallel()
				cmd := build()
				err := cmd.ParseFlags([]string{"--port", tc.Raw})
				if tc.Valid {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "1-65535") {
					t.Fatalf("invalid port parse error = %v", err)
				}
			})
		}
	}
}
