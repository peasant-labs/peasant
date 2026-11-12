package ingest_test

import (
	"context"
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/discovery_pipeline_progress.yaml
var discoveryPipelineYAML []byte

//go:embed testdata/discovery_pipeline_progress.manifest.yaml
var discoveryPipelineManifest []byte

func TestPipelineDiscoveryProgress(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name  string                  `yaml:"name"`
			Root  string                  `yaml:"root"`
			Files []discoveryProgressFile `yaml:"files"`
			Block string                  `yaml:"block"`
			Done  int                     `yaml:"done"`
			Total int                     `yaml:"total"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(discoveryPipelineYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(discoveryPipelineManifest, "pipeline discovery progress")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "pipeline discovery progress"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			fs := testutil.NewGatedFS(testutil.NewMemFS())
			for _, file := range c.Files {
				if err := fs.WriteFile(c.Root+"/"+file.Path, []byte(file.Body+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fs.Arm(fsdecorator.Gate{Op: fsdecorator.OpStat, Path: c.Root + "/" + c.Block})
			defer fs.Release()
			progress := ingest.NewProgressState()
			cfg := ingest.PipelineConfig{DryRun: true, OutputDir: "/output", Progress: progress,
				Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(c.Root)}}}}
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
				return ingest.NewClaudeAdapter(fs, git, s)
			}}
			pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), adapters, cfg)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := pipeline.Run(context.Background()); done <- err }()
			ctx := testwait.Context(t)
			select {
			case <-fs.Reached():
			case <-ctx.Done():
				t.Fatal("discovery did not reach the gated source")
			}
			state := progress.Snapshot()[ingest.StageDiscover]
			if !state.Started || state.Ended {
				t.Fatalf("discovery must be active while its source is blocked: %+v", state)
			}
			if state.Done != c.Done || state.Total != c.Total {
				t.Fatalf("active discovery = %+v, want %d/%d before end", state, c.Done, c.Total)
			}
			fs.Release()
			if err := testwait.Receive(t, done, "pipeline completion"); err != nil {
				t.Fatal(err)
			}
			state = progress.Snapshot()[ingest.StageDiscover]
			if !state.Ended || state.Done != c.Total || state.Total != c.Total {
				t.Fatalf("completed discovery = %+v, want %d/%d", state, c.Total, c.Total)
			}
		})
	}
}
