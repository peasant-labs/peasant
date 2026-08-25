package ingest_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// enumerationFailingSource wraps a real source and fails only the legacy
// session enumeration, so the candidate probes as supported but discovery
// cannot read its sessions.
type enumerationFailingSource struct {
	ingest.OpenCodeSQLiteSource
}

func (enumerationFailingSource) LegacySessionIDs(context.Context, ingest.OpenCodeLegacySessionPageRequest) (ingest.OpenCodeLegacySessionPage, error) {
	return ingest.OpenCodeLegacySessionPage{}, errors.New("synthetic locked session table")
}

// TestPipelineSurfacesOpenCodeDiscoveryFailure proves that a supported OpenCode
// candidate whose enumeration fails leaves a visible diagnostic in the pipeline
// result, so a whole database that was skipped is not reported as success with
// no sessions.
func TestPipelineSurfacesOpenCodeDiscoveryFailure(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-message-part")
	databasePath := materialized.Path
	root, err := ingest.NewResolvedPath(filepath.Dir(databasePath))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}

	opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if openErr != nil {
			return nil, openErr
		}
		return enumerationFailingSource{OpenCodeSQLiteSource: source}, nil
	}
	factory := func(filesystem ingest.FileSystem, git ingest.GitResolver, installationSalt salt.Salt) ingest.SourceAdapter {
		candidateFS, ok := filesystem.(ingest.OpenCodeCandidateFileSystem)
		if !ok {
			t.Fatal("production filesystem lacks OpenCode candidate capability")
		}
		adapter, adapterErr := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, git, installationSalt, "latest", fixedCandidateEnvironment{}, candidateFS, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
		if adapterErr != nil {
			t.Fatalf("construct candidate-capable adapter: %v", adapterErr)
		}
		return adapter
	}

	output, err := ingest.NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatalf("resolve output: %v", err)
	}
	config := ingest.PipelineConfig{
		Sources:     map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{root}}},
		OutputDir:   output,
		Parallelism: 1,
		DryRun:      true,
	}
	pipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: factory}, config)
	if err != nil {
		t.Fatalf("construct pipeline: %v", err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("pipeline run over a candidate whose enumeration fails: %v", err)
	}

	found := false
	for _, diagnostic := range result.DiscoveryDiagnostics {
		if filepath.Clean(diagnostic.Location) == filepath.Clean(databasePath) && diagnostic.Provider == ingest.HarnessOpenCode {
			found = true
			if strings.TrimSpace(diagnostic.Summary) == "" {
				t.Fatalf("discovery diagnostic for %q has no summary", databasePath)
			}
		}
	}
	if !found {
		t.Fatalf("pipeline result did not surface the skipped database %q: diagnostics=%+v", databasePath, result.DiscoveryDiagnostics)
	}
}
