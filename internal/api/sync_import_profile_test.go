package api

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/web_import_profile.yaml
var webImportProfileYAML []byte

// TestWebImportsStoreAProfile runs each import entry point of the local web
// app over one synthetic Claude session and reads the run's stage rows back
// from the store: every web entry point keeps the profiler on like the
// commands.
func TestWebImportsStoreAProfile(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name  string `yaml:"name"`
			Entry string `yaml:"entry"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(webImportProfileYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("web_import_profile.yaml", "cases", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			var preflight bool
			switch c.Entry {
			case "import-route":
			case "kickstart":
				preflight = true
			default:
				t.Fatalf("web_import_profile.yaml: case %q names entry %q; add it to the runner", c.Name, c.Entry)
			}
			dataHome := webImportProfileHome(t)
			h := syncResidueHandler(t, dataHome.data, dataHome.cfg)
			start, launch, err := h.reserveIngest(preflight)
			if err != nil || start.AlreadyRunning || launch == nil {
				t.Fatalf("claim the %s import: start=%+v err=%v", c.Entry, start, err)
			}
			finished := make(chan schema.LocalKickstartIngest, 1)
			launch(func(state schema.LocalKickstartIngest) { finished <- state })
			select {
			case state := <-finished:
				if state.State != schema.LocalKickstartIngestIdle || state.New != 1 {
					t.Fatalf("the %s import ended as %+v, want idle with one new session", c.Entry, state)
				}
			case <-time.After(2 * time.Minute):
				t.Fatalf("the %s import did not finish", c.Entry)
			}
			stages := storedWebImportStages(t, dataHome.data)
			for _, required := range []ingest.Stage{ingest.StageDiscover, ingest.StageFilter, ingest.StageReport} {
				if !stages[required] {
					t.Fatalf("the %s import stored no %s stage row; stored %v", c.Entry, required, stages)
				}
			}
		})
	}
}

type webImportProfileFixture struct {
	data string
	cfg  *config.Config
}

// webImportProfileHome writes one synthetic Claude session and a configuration
// that imports it into a data home of its own.
func webImportProfileHome(t *testing.T) webImportProfileFixture {
	t.Helper()
	root := t.TempDir()
	const sessionID = "97100000-0000-4000-8000-0000000000e2"
	source := filepath.Join(root, "native", "-fixtures-synthetic-project", sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"sessionId":"` + sessionID + `","type":"user","cwd":"/fixtures/synthetic-project","message":{"role":"user","content":"synthetic prompt"}}` + "\n"
	if err := os.WriteFile(source, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.BaseConfig()
	cfg.Selection.Mode = config.SelectionModeAll
	cfg.Sources = config.SourcesConfig{ClaudeCode: config.SourceProviderConfig{Enabled: true, Paths: []string{filepath.Join(root, "native")}}}
	cfg.Output.BasePath = filepath.Join(root, "managed")
	cfg.Output.StalenessThresholdSec = 1
	dataHome := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Dir(string(defaults.ResolveDBFilePathWith(dataHome))), 0o700); err != nil {
		t.Fatal(err)
	}
	return webImportProfileFixture{data: dataHome, cfg: cfg}
}

// storedWebImportStages reads the stage names of the newest run.
func storedWebImportStages(t *testing.T, dataHome string) map[ingest.Stage]bool {
	t.Helper()
	conn, err := sqlite.OpenConn(string(defaults.ResolveDBFilePathWith(dataHome)), sqlite.OpenReadOnly)
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
