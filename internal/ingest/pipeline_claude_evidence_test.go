package ingest_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestPipeline_ClaudeDiscoveryFillsEvidenceCache proves the ingest path also
// fills the discovery evidence cache. The pipeline builds its adapters itself,
// so the store must reach the Claude adapter for a later scan to profit from it.
func TestPipeline_ClaudeDiscoveryFillsEvidenceCache(t *testing.T) {
	ctx := context.Background()
	fixture := loadClaudeEvidenceCacheFixtures(t).Cases[0]

	filesystem := testutil.NewCountingFS(testutil.NewMemFS())
	writeClaudeEvidenceFiles(t, filesystem, fixture.Files)

	root := t.TempDir()
	database := openEvidenceStore(t, filepath.Join(root, "peasant.db"))

	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {
				Paths:   []ingest.ResolvedPath{claudeEvidenceCacheRoot},
				Enabled: true,
			},
		},
		OutputDir:          ingest.ResolvedPath(filepath.Join(root, "peasant-sync")),
		StalenessThreshold: time.Minute,
	}
	pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(),
		ingest.DefaultAdapterRegistry, cfg, ingest.WithStore(database))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if _, err := pipeline.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	records, err := database.LoadClaudeEvidence(ctx)
	if err != nil {
		t.Fatalf("LoadClaudeEvidence: %v", err)
	}
	if len(records) != len(fixture.Files) {
		t.Fatalf("ingest cached %d transcripts, want %d", len(records), len(fixture.Files))
	}
	for _, file := range fixture.Files {
		path := ingest.ResolvedPath(claudeEvidenceCacheRoot + "/" + file.Path)
		if _, ok := records[path]; !ok {
			t.Errorf("ingest did not cache the evidence of transcript %q", path)
		}
	}
}
