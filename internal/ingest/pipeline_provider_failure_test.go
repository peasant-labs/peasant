package ingest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// failingDiscoveryAdapter fails the whole Discover call for one harness while
// another harness succeeds.
type failingDiscoveryAdapter struct{}

func (failingDiscoveryAdapter) Harness() ingest.Harness { return ingest.HarnessCodex }

func (failingDiscoveryAdapter) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	return nil, errors.New("synthetic codex discovery failure")
}

func (failingDiscoveryAdapter) ExtractMetadata(context.Context, ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	return nil, errors.New("unused: discovery failed before extraction")
}

// TestPipelineProviderDiscoveryFailureIsSurfaced proves a run with one
// healthy harness and one failed harness reports the failure on its result.
// Without the diagnostic the run loses every session of the failed harness and
// still exits 0.
func TestPipelineProviderDiscoveryFailureIsSurfaced(t *testing.T) {
	mfs := testutil.NewMemFS()
	transcript := `{"sessionId":"11111111-1111-4111-8111-111111111111","type":"user","cwd":"/workspace","message":{"role":"user","content":"hello"}}` + "\n"
	if err := mfs.WriteFile("/claude/-workspace/11111111-1111-4111-8111-111111111111.jsonl", []byte(transcript), 0o644); err != nil {
		t.Fatalf("write the healthy transcript: %v", err)
	}

	output, err := ingest.NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatalf("resolve output directory: %v", err)
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewClaudeAdapter(fs, git, s)
		},
		ingest.HarnessCodex: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
			return failingDiscoveryAdapter{}
		},
	}
	config := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{"/claude"}, Enabled: true},
			ingest.HarnessCodex:      {Paths: []ingest.ResolvedPath{"/codex/sessions"}, Enabled: true},
		},
		OutputDir:   output,
		Parallelism: 1,
		DryRun:      true,
	}
	pipeline, err := newTestPipeline(mfs, testutil.NoGitResolver(), adapters, config)
	if err != nil {
		t.Fatalf("construct pipeline: %v", err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("a run with one healthy harness failed: %v", err)
	}

	// The failed harness is reported AND the healthy harness's work survives: a
	// diagnostic that came with an empty run would not be a partial run.
	found := false
	for _, diagnostic := range result.DiscoveryDiagnostics {
		if diagnostic.Code != ingest.DiscoveryCodeHarnessFailed || diagnostic.Provider != ingest.HarnessCodex {
			continue
		}
		if strings.TrimSpace(diagnostic.Summary) == "" {
			t.Errorf("the partial-failure diagnostic has no summary: %+v", diagnostic)
		}
		if !strings.Contains(diagnostic.Detail, "synthetic codex discovery failure") {
			t.Errorf("the partial-failure diagnostic does not name the cause: %q", diagnostic.Detail)
		}
		found = true
	}
	if !found {
		t.Fatalf("the run lost the Codex harness with no diagnostic: %+v", result.DiscoveryDiagnostics)
	}
	const healthySessionID = "11111111-1111-4111-8111-111111111111"
	for _, session := range result.Sessions {
		if session.SessionID.String() == healthySessionID && session.Harness == ingest.HarnessClaudeCode {
			return
		}
	}
	t.Fatalf("the healthy Claude session %s was lost from the partial run: %+v", healthySessionID, result.Sessions)
}
