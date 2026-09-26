package ingest_test

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
)

// testIngestArenaBytes is the bounded staging arena every test pipeline uses.
// Production keeps the environment default: the PEASANT_INGEST_ARENA_BYTES
// override when set, else the 2 GiB slab. Tests inject this size through
// WithArenaSizeBytes so no test has to mutate the process environment for the
// whole binary.
const testIngestArenaBytes = 64 << 20 // 64 MiB

// newTestPipeline is ingest.NewPipeline with the injected test arena size.
func newTestPipeline(fs ingest.FileSystem, git ingest.GitResolver, adapters map[ingest.Harness]ingest.AdapterFactory, cfg ingest.PipelineConfig, opts ...ingest.PipelineOption) (*ingest.Pipeline, error) {
	return ingest.NewPipeline(fs, git, adapters, cfg, append([]ingest.PipelineOption{ingest.WithArenaSizeBytes(testIngestArenaBytes)}, opts...)...)
}

// newTestOpenCodeAdapter is the production OpenCode adapter constructor with a
// deterministic, empty environment, so candidate discovery never inherits a
// developer's OPENCODE_DB, OPENCODE_DISABLE_CHANNEL_DB, or OPENCODE_CHANNEL
// overrides. The production constructor keeps SystemOpenCodeEnvironment as its
// default.
func newTestOpenCodeAdapter(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) *ingest.OpenCodeAdapter {
	return ingest.NewOpenCodeAdapter(fs, git, s, ingest.WithOpenCodeEnvironment(testOpenCodeEnvironment{}))
}

// testOpenCodeEnvironment reports every variable absent.
type testOpenCodeEnvironment struct{}

func (testOpenCodeEnvironment) LookupEnv(string) (string, bool) { return "", false }
