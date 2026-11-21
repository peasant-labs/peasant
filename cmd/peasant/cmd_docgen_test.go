package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDocgenUsesPortableConfigDefault(t *testing.T) {
	t.Parallel()
	docsDir := t.TempDir()
	runtimeConfigPath := filepath.Join(t.TempDir(), "config.yaml")
	root := &cobra.Command{Use: "peasant"}
	runtimeConfigUsage := buildRootCommand().PersistentFlags().Lookup("config").Usage
	root.PersistentFlags().String("config", runtimeConfigPath, runtimeConfigUsage)
	root.AddCommand(BuildDocgenCommand())
	root.SetArgs([]string{"docgen", docsDir})

	if err := root.Execute(); err != nil {
		t.Fatalf("generate CLI docs: %v", err)
	}

	generated, err := os.ReadFile(filepath.Join(docsDir, "peasant.md"))
	if err != nil {
		t.Fatalf("read generated root doc: %v", err)
	}
	contents := string(generated)
	if strings.Contains(contents, runtimeConfigPath) {
		t.Errorf("generated docs contain runtime config path %q", runtimeConfigPath)
	}
	if !strings.Contains(contents, "~/.config/peasant/config.yaml") {
		t.Error("generated docs do not contain the portable config default")
	}
	if !strings.Contains(contents, "](../cli-contract.md)") {
		t.Error("the generated root page does not link the CLI contract")
	}

	configFlag := root.PersistentFlags().Lookup("config")
	if got := configFlag.DefValue; got != runtimeConfigPath {
		t.Errorf("config flag default after doc generation = %q, want %q", got, runtimeConfigPath)
	}
	if got := configFlag.Value.String(); got != runtimeConfigPath {
		t.Errorf("config flag runtime value after doc generation = %q, want %q", got, runtimeConfigPath)
	}
	if got := configFlag.Usage; got != runtimeConfigUsage {
		t.Errorf("config flag usage after doc generation = %q, want %q", got, runtimeConfigUsage)
	}
}

// TestRootConfigHelpShowsOneDefault pins the --config line of the production
// root help: Cobra prints the flag's default itself, so the usage text must not
// carry a second one.
func TestRootConfigHelpShowsOneDefault(t *testing.T) {
	t.Parallel()
	root := buildRootCommand()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root --help: %v", err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, "--config string") {
			continue
		}
		if got := strings.Count(line, "default"); got != 1 {
			t.Errorf("--config help line names %d defaults, want 1: %q", got, line)
		}
		return
	}
	t.Fatalf("root help has no --config line:\n%s", out.String())
}
