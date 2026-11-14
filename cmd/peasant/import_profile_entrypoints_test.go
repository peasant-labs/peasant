package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/import_profile_entrypoints.yaml
var importProfileEntrypointsYAML []byte

type importProfileEntrypointCase struct {
	Name        string   `yaml:"name"`
	Entry       string   `yaml:"entry"`
	Args        []string `yaml:"args"`
	WantPrinted bool     `yaml:"wantPrinted"`
}

// storedProfileStages reads, from the store an entry point wrote, the stage
// names of its newest run.
func storedProfileStages(t *testing.T, dataDir string) map[ingest.Stage]bool {
	t.Helper()
	conn, err := sqlite.OpenConn(string(defaults.ResolveDBFilePathWith(dataDir)), sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stages := make(map[ingest.Stage]bool)
	if err := sqlitex.ExecuteTransient(conn, `SELECT stage FROM ingest_run_stages WHERE run_id = (SELECT max(id) FROM ingest_log)`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			stage, err := ingest.NewStage(stmt.ColumnText(0))
			if err != nil {
				return err
			}
			stages[stage] = true
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return stages
}

func TestImportEntryPointsStoreAProfile(t *testing.T) {
	var fixture struct {
		RequiredNames        []string                      `yaml:"requiredNames"`
		Session              selectionRunnerSourceSession  `yaml:"session"`
		PrintedProfileMarker string                        `yaml:"printedProfileMarker"`
		Cases                []importProfileEntrypointCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(importProfileEntrypointsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, row := range fixture.Cases {
		present[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("import_profile_entrypoints.yaml", "cases", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	if fixture.PrintedProfileMarker == "" {
		t.Fatal("import_profile_entrypoints.yaml names no printed-profile marker")
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "native")
			writeSelectionRunnerSources(t, source, []selectionRunnerSourceSession{fixture.Session})
			var output string
			switch row.Entry {
			case "harvest":
				configPath := filepath.Join(root, "harvest-config.yaml")
				body := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n", source, filepath.Join(root, "out"))
				if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				output, err = executeHarvestCmd(t, root, append([]string{"--config=" + configPath}, row.Args...))
				if err != nil {
					t.Fatalf("harvest failed: %v\n%s", err, output)
				}
			case "kickstart":
				cfg := config.BaseConfig()
				cfg.Selection.Mode = config.SelectionModeAll
				cfg.Sources = config.SourcesConfig{ClaudeCode: config.SourceProviderConfig{Enabled: true, Paths: []string{source}}}
				cfg.Output.BasePath = filepath.Join(root, "managed")
				configPath := filepath.Join(root, "config.yaml")
				if err := config.SaveAtomic(configPath, cfg); err != nil {
					t.Fatal(err)
				}
				cmd := &cobra.Command{Use: "kickstart"}
				cmd.Flags().String("data-dir", root, "")
				run, _ := kickstartLocalIngest(cmd, configPath, selectionRunnerListings([]selectionRunnerSourceSession{fixture.Session}))
				result, err := run(t.Context())
				if err != nil || result == nil || result.New != 1 {
					t.Fatalf("kickstart import: %+v %v", result, err)
				}
			default:
				t.Fatalf("import_profile_entrypoints.yaml: case %q names entry %q; add it to the runner", row.Name, row.Entry)
			}
			stages := storedProfileStages(t, root)
			for _, required := range []ingest.Stage{ingest.StageDiscover, ingest.StageExtract, ingest.StageReport} {
				if !stages[required] {
					t.Fatalf("the %s entry point stored no %s stage row; stored %v", row.Entry, required, stages)
				}
			}
			if row.Entry == "harvest" && strings.Contains(output, fixture.PrintedProfileMarker) != row.WantPrinted {
				t.Fatalf("printed profile present = %v, want %v:\n%s", !row.WantPrinted, row.WantPrinted, output)
			}
		})
	}
}
