package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// claimFixture builds one discoverable session and returns a pipeline factory
// so a test can run the production path more than once.
func claimFixture(t *testing.T) (*testutil.MemFS, map[ingest.Harness]ingest.AdapterFactory, ingest.PipelineConfig, ingest.SessionID) {
	t.Helper()
	mfs := testutil.NewMemFS()
	const sessionID = "0a0a0a0a-1111-4111-8111-111111111111"
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, sessionID)
	body := `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2024-02-19T00:00:00Z"}` + "\n"
	if err := mfs.WriteFile(sourcePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture source: %v", err)
	}
	session := makeDiscoveredSession(t, sessionID, sourcePath, time.Now().Add(-time.Hour))
	metadata := map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: makeMinimalMeta(t, sessionID)}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, metadata),
	}
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return mfs, adapters, makePipelineConfig(testOutputDir), sid
}

func TestPipelineClaimsNewSessions(t *testing.T) {
	t.Parallel()
	mfs, adapters, cfg, sid := claimFixture(t)
	var claimed []string
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, ids []string) error {
		claimed = append(claimed, ids...)
		return nil
	})
	pipeline, err := newTestPipeline(mfs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithSessionClaimer(claimer))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Fatalf("new sessions = %d, want 1", result.Summary.New)
	}
	if len(claimed) != 1 || claimed[0] != string(sid) {
		t.Fatalf("claimed = %v, want [%s]", claimed, sid)
	}
}

func TestPipelineDryRunClaimsNothing(t *testing.T) {
	t.Parallel()
	mfs, adapters, cfg, _ := claimFixture(t)
	cfg.DryRun = true
	called := false
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, _ []string) error {
		called = true
		return nil
	})
	pipeline, err := newTestPipeline(mfs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithSessionClaimer(claimer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Fatal("a dry run claimed sessions")
	}
}

func TestPipelineClaimFailureDoesNotFailTheRun(t *testing.T) {
	t.Parallel()
	mfs, adapters, cfg, _ := claimFixture(t)
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, _ []string) error {
		return errors.New("claim store unavailable")
	})
	pipeline, err := newTestPipeline(mfs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithSessionClaimer(claimer))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("a claim failure failed the run: %v", err)
	}
	if result.Summary.New != 1 || result.Summary.Errors != 0 {
		t.Fatalf("summary = %+v, want one successful new session", result.Summary)
	}
	found := false
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.ErrorType == "local_ownership_claim_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want a local_ownership_claim_failed warning", result.Diagnostics)
	}
}
