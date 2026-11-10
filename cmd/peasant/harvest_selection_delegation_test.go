package main

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
)

//go:embed testdata/harvest_selection_delegation.yaml
var harvestSelectionDelegationYAML []byte

type harvestDelegationFixture struct {
	RequiredNames []string                `yaml:"requiredNames"`
	IDs           harvestDelegationIDs    `yaml:"ids"`
	Cases         []harvestDelegationCase `yaml:"cases"`
}

type harvestDelegationIDs struct {
	Selected string `yaml:"selected"`
	Other    string `yaml:"other"`
}

type harvestDelegationCase struct {
	Name                   string   `yaml:"name"`
	All                    bool     `yaml:"all"`
	IndexOnly              bool     `yaml:"indexOnly"`
	PreludeAll             bool     `yaml:"preludeAll"`
	Session                string   `yaml:"session"`
	ExpectFilter           bool     `yaml:"expectFilter"`
	ExpectNoWithholdNotice bool     `yaml:"expectNoWithholdNotice"`
	ExpectImported         []string `yaml:"expectImported"`
	ExpectAbsent           []string `yaml:"expectAbsent"`
}

func loadHarvestDelegationFixture(t *testing.T) harvestDelegationFixture {
	t.Helper()
	var fixture harvestDelegationFixture
	if err := testutil.DecodeFixtureYAML(harvestSelectionDelegationYAML, &fixture); err != nil {
		t.Fatalf("decode harvest selection delegation fixture: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, item := range fixture.Cases {
		if item.Name == "" || present[item.Name] {
			t.Fatalf("harvest delegation fixture has an empty or duplicate case name %q", item.Name)
		}
		present[item.Name] = true
	}
	if err := testutil.RequireFixtureNames("cmd/peasant/testdata/harvest_selection_delegation.yaml", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestHarvestSelectionDelegation(t *testing.T) {
	t.Parallel()
	fixture := loadHarvestDelegationFixture(t)
	for _, item := range fixture.Cases {
		item := item
		t.Run(item.Name, func(t *testing.T) {
			t.Parallel()
			runHarvestDelegationCase(t, fixture.IDs, item)
		})
	}
}

func runHarvestDelegationCase(t *testing.T, ids harvestDelegationIDs, item harvestDelegationCase) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "recordings")
	output := filepath.Join(dir, "managed")
	project := filepath.Join(source, "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDelegationSession(t, filepath.Join(project, ids.Selected+".jsonl"), ids.Selected)
	writeDelegationSession(t, filepath.Join(project, ids.Other+".jsonl"), ids.Other)

	cfg := config.BaseConfig()
	cfg.Output.BasePath = output
	for _, harness := range defaults.AllHarnesses {
		provider, ok := cfg.Sources.Provider(harness)
		if ok {
			provider.Enabled = false
			provider.Paths = nil
		}
	}
	cfg.Selection = config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessClaudeCode.String(): {Sessions: []string{ids.Selected}},
		},
	}
	configPath := filepath.Join(dir, string(defaults.Config.FileName))
	if err := config.SaveAtomic(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	sessionArg := ""
	if item.Session == "other" {
		sessionArg = ids.Other
	}
	flags := &harvestFlags{all: item.All}
	if sessionArg != "" {
		flags.sessionIDs = []string{sessionArg}
	}
	mode := harvestAll
	if item.IndexOnly {
		mode = harvestIndexOnly
	}
	if got := harvestAppliesSavedSelection(mode, flags, cfg); got != item.ExpectFilter {
		t.Fatalf("saved selection applied = %v, want %v", got, item.ExpectFilter)
	}

	if item.PreludeAll {
		out, err := runDelegationHarvest(t, dir, configPath, []string{"--all", "--include-active", "--output", output, "--source-harness", "claude-code", "--source-path", source})
		if err != nil {
			t.Fatalf("prelude harvest --all: %v\n%s", err, out)
		}
	}
	args := []string{"--output", output}
	if !item.IndexOnly {
		args = append(args, "--include-active", "--source-harness", "claude-code", "--source-path", source)
	}
	if item.All {
		args = append(args, "--all")
	}
	if sessionArg != "" {
		args = append(args, "--session", sessionArg)
	}
	if item.IndexOnly {
		args = append([]string{"index"}, args...)
	}
	out, err := runDelegationHarvest(t, dir, configPath, args)
	if err != nil {
		t.Fatalf("harvest: %v\n%s", err, out)
	}
	if item.ExpectNoWithholdNotice && strings.Contains(out, "was withheld during ingest") {
		t.Fatalf("index-only harvest printed a selection withhold notice:\n%s", out)
	}
	for _, id := range item.ExpectImported {
		if !outputContainsSession(t, output, id) {
			t.Errorf("session %s was not imported; output:\n%s", id, out)
		}
	}
	for _, id := range item.ExpectAbsent {
		if outputContainsSession(t, output, id) {
			t.Errorf("session %s was imported; output:\n%s", id, out)
		}
	}
}

func runDelegationHarvest(t *testing.T, dir, configPath string, args []string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.AddCommand(BuildHarvestCommand())
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"harvest", "--data-dir", dir, "--config-dir", dir, "--config", configPath}, args...))
	err := root.Execute()
	return buf.String(), err
}

func writeDelegationSession(t *testing.T, path, id string) {
	t.Helper()
	body := `{"sessionId":"` + id + `","type":"user","uuid":"` + id + `-user","timestamp":"2026-01-01T00:00:00Z","cwd":"/workspace","gitBranch":"main","message":{"role":"user","content":"hello from the selection test"}}
{"sessionId":"` + id + `","type":"assistant","uuid":"` + id + `-assistant","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"ok"}]}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}

func outputContainsSession(t *testing.T, output, id string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(output, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.Contains(path, id) {
			found = true
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return found
}
