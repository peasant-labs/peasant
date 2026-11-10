package main

import (
	"bytes"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

func TestHarvestHelpRegistry(t *testing.T) {
	var expected []string
	for harness := range ingest.DefaultAdapterRegistry {
		expected = append(expected, string(harness))
	}
	sort.Strings(expected)

	for _, subcommand := range []string{"", "logs", "index"} {
		name := subcommand
		if name == "" {
			name = "harvest"
		}
		t.Run(name, func(t *testing.T) {
			cmd := BuildHarvestCommand()
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			args := []string{"--help"}
			if subcommand != "" {
				args = append([]string{subcommand}, args...)
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(output.String(), "\n") {
				if !strings.Contains(line, "--source-harness string") {
					continue
				}
				_, list, ok := strings.Cut(line, "(")
				if !ok {
					t.Fatalf("missing harness list: %s", line)
				}
				list, _, _ = strings.Cut(list, ")")
				list, _, _ = strings.Cut(list, ";")
				if got := strings.Split(list, ", "); !slices.Equal(got, expected) {
					t.Fatalf("help harnesses = %q, want %q", got, expected)
				}
				return
			}
			t.Fatalf("source-harness flag missing from help:\n%s", output.String())
		})
	}
}
