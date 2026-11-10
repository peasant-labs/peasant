package ingest_test

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestPipelineReindexAttachesTheCodexRolloutCatalog runs the production reindex
// path end to end: `harvest index` (not `--all`) builds its own discovery
// adapter for the canonical source facts, attaches the store as its
// rollout-catalog writer, and the Codex walk catalogues the rollout it
// enumerated. Dropping the attach leaves the table empty, so this case fails:
// the reindex path would walk Codex and write nothing, which is exactly the
// drift the full-walk attach does not cover.
func TestPipelineReindexAttachesTheCodexRolloutCatalog(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	output := filepath.Join(root, "managed")
	codexRoot := filepath.Join(root, "sessions")
	sessionID := ingest.SessionID("11111111-1111-4111-8111-111111111111")
	path := filepath.Join(codexRoot, "2026", "11", "09", "rollout-2026-11-09T10-00-00-"+sessionID.String()+".jsonl")
	body := `{"timestamp":"2026-11-09T10:00:00Z","type":"session_meta","payload":{"id":"` + sessionID.String() + `","cwd":"/work/project","cli_version":"0.133.0"}}` + "\n" +
		`{"timestamp":"2026-11-09T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the Codex source tree: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the rollout: %v", err)
	}

	db := storetest.Open(t)
	fs := &ingest.OSFileSystem{}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessCodex: func(filesystem ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewCodexAdapter(filesystem, git, s)
		},
	}
	cfg := makePipelineConfig(output, func(c *ingest.PipelineConfig) {
		c.Reindex = true
		c.Sources = map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessCodex: {
				Enabled: true,
				Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(codexRoot)},
			},
		}
	})
	pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithHarvesterVersions(maps.Clone(ingest.HarvesterVersionRegistry)),
	)
	if err != nil {
		t.Fatalf("build the pipeline: %v", err)
	}
	if _, err := pipeline.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rows, err := db.LoadCodexRolloutCatalog(ctx)
	if err != nil {
		t.Fatalf("load the rollout catalog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("catalog holds %d rows, want the one rollout the reindex walk enumerated; a plain `harvest index` must attach the store as the Codex rollout-catalog writer", len(rows))
	}
	if rows[0].Path.String() != path || rows[0].SessionID != sessionID {
		t.Fatalf("catalog row = %+v, want %s=%s", rows[0], path, sessionID)
	}
}
