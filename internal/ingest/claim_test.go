package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
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
	root := t.TempDir()
	const sessionID = "0a0a0a0a-1111-4111-8111-111111111111"
	claimB1Source(t, root, sessionID, time.Now().Add(-72*time.Hour))
	var claimed []string
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, ids []string) error {
		claimed = append(claimed, ids...)
		return nil
	})
	result := claimB1RunWith(t, root, claimB1Config(root), claimer)
	if result.Summary.New != 1 {
		t.Fatalf("new sessions = %d, want 1", result.Summary.New)
	}
	if len(claimed) != 1 || claimed[0] != sessionID {
		t.Fatalf("claimed = %v, want [%s]", claimed, sessionID)
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
	root := t.TempDir()
	const sessionID = "0b0b0b0b-1111-4111-8111-111111111111"
	claimB1Source(t, root, sessionID, time.Now().Add(-72*time.Hour))
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, _ []string) error {
		return errors.New("claim store unavailable")
	})
	result := claimB1RunWith(t, root, claimB1Config(root), claimer)
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

// claimB1Source writes one settled or active Claude Code transcript under root
// and returns its path.
func claimB1Source(t *testing.T, root, sessionID string, modTime time.Time) string {
	t.Helper()
	source := filepath.Join(root, "recordings", "-workspace", sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"sessionId":"` + sessionID + `","type":"user","uuid":"entry-first","timestamp":"2023-11-14T22:13:20Z","cwd":"/workspace","message":{"role":"user","content":"first turn"}}` + "\n"
	if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return source
}

// claimB1Run runs the production pipeline against root with a claimer that
// records every session id it receives.
func claimB1Run(t *testing.T, root string, cfg ingest.PipelineConfig) []string {
	t.Helper()
	var claimed []string
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, ids []string) error {
		claimed = append(claimed, ids...)
		return nil
	})
	claimB1RunWith(t, root, cfg, claimer)
	return claimed
}

func claimB1RunWith(t *testing.T, root string, cfg ingest.PipelineConfig, claimer ingest.SessionClaimer) *ingest.PipelineResult {
	t.Helper()
	fs := &ingest.OSFileSystem{}
	dbPath := filepath.Join(root, "peasant.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
		ingest.WithStore(db), ingest.WithMetricsStore(db),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithSessionClaimer(claimer))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("run pipeline: %v", err)
	}
	return result
}

func claimB1Config(root string) ingest.PipelineConfig {
	return ingest.PipelineConfig{
		Sources:            map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join(root, "recordings"))}}},
		OutputDir:          ingest.ResolvedPath(filepath.Join(root, "managed")),
		StalenessThreshold: 5 * time.Minute,
		Parallelism:        1,
	}
}

// TestAutoClaimSessionFirstSeenActive proves a session first stored while it is
// still being written is claimed on that first run, and not again later.
func TestAutoClaimSessionFirstSeenActive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const sessionID = "11111111-1111-4111-8111-111111111201"
	claimB1Source(t, root, sessionID, time.Now())
	cfg := claimB1Config(root)
	cfg.IncludeActive = true

	first := claimB1Run(t, root, cfg)
	if len(first) != 1 || first[0] != sessionID {
		t.Fatalf("first active run claimed = %v, want [%s]", first, sessionID)
	}
	// The recording settles after that first run. It must not be claimed twice.
	claimB1Source(t, root, sessionID, time.Now().Add(-72*time.Hour))
	second := claimB1Run(t, root, cfg)
	if len(second) != 0 {
		t.Fatalf("second run claimed = %v, want none for the already stored session", second)
	}
}

// TestAutoClaimSkipsForcedReprocess proves Force re-processing a stored session
// claims nothing, whatever diff status the forced run reports.
func TestAutoClaimSkipsForcedReprocess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const sessionID = "11111111-1111-4111-8111-111111111202"
	claimB1Source(t, root, sessionID, time.Now().Add(-72*time.Hour))
	cfg := claimB1Config(root)
	if first := claimB1Run(t, root, cfg); len(first) != 1 {
		t.Fatalf("first run claimed = %v, want the new session", first)
	}
	cfg.Force = true
	if forced := claimB1Run(t, root, cfg); len(forced) != 0 {
		t.Fatalf("forced re-process claimed = %v, want none", forced)
	}
}

// TestAutoClaimSkipsRebuildFromSavedPairs proves deleting the database and
// rebuilding from saved copies (and the full index rebuild) claims nothing.
func TestAutoClaimSkipsRebuildFromSavedPairs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const sessionID = "11111111-1111-4111-8111-111111111203"
	claimB1Source(t, root, sessionID, time.Now().Add(-72*time.Hour))
	cfg := claimB1Config(root)
	claimB1Run(t, root, cfg)

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(root, "peasant.db") + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if rebuilt := claimB1Run(t, root, cfg); len(rebuilt) != 0 {
		t.Fatalf("rebuild from saved pairs claimed = %v, want none", rebuilt)
	}
	cfg.RebuildAll = true
	if indexed := claimB1Run(t, root, cfg); len(indexed) != 0 {
		t.Fatalf("harvest index --all claimed = %v, want none", indexed)
	}
}
