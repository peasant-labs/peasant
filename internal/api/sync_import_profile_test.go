package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestWebImportStoresAProfile runs the pipeline the web-triggered import
// builds over one synthetic Claude session and reads the run's stage rows back
// from the store: the web entry point keeps the profiler on like the commands.
func TestWebImportStoresAProfile(t *testing.T) {
	t.Parallel()
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
	h := syncResidueHandler(t, dataHome, cfg)
	built, err := h.newIngestPipeline(ingest.NewProgressState(), nil)
	if err != nil {
		t.Fatalf("build the web import pipeline: %v", err)
	}
	result, err := built.pipeline.Run(t.Context())
	built.release()
	if err != nil {
		t.Fatalf("web import: %v", err)
	}
	if result.Census == nil {
		t.Fatal("the web import reported no refresh census")
	}

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
	for _, required := range []ingest.Stage{ingest.StageDiscover, ingest.StageFilter, ingest.StageReport} {
		if !stages[required] {
			t.Fatalf("the web import stored no %s stage row; stored %v", required, stages)
		}
	}
}
