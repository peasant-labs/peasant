package main

import (
	"github.com/peasant-labs/peasant/internal/ingest"
)

// newTestPipeline constructs a pipeline for the composition-level tests that
// build one directly.
//
// It deliberately does NOT inject an arena size, unlike internal/ingest's test
// helper of the same name: cmd/peasant's TestMain bounds
// PEASANT_INGEST_ARENA_BYTES for the whole test binary already
// (main_test.go). Keeping the two helpers distinct makes the difference
// explicit — internal/ingest injects the option because its tests no longer
// set the environment, while this package relies on the process-wide default.
func newTestPipeline(fs ingest.FileSystem, git ingest.GitResolver, adapters map[ingest.Harness]ingest.AdapterFactory, cfg ingest.PipelineConfig, opts ...ingest.PipelineOption) (*ingest.Pipeline, error) {
	return ingest.NewPipeline(fs, git, adapters, cfg, opts...)
}
