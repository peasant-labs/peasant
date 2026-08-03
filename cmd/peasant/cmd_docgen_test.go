package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDocgenUsesPortableConfigDefault(t *testing.T) {
	docsDir := t.TempDir()
	runtimeConfigPath := filepath.Join(t.TempDir(), "config.yaml")
	root := &cobra.Command{Use: "peasant"}
	runtimeConfigUsage := "Path to config file (default: ~/.config/peasant/config.yaml)"
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
