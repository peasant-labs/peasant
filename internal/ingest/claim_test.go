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
	writeClaimSource(t, root, sessionID, time.Now().Add(-72*time.Hour))
	var claimed []string
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, ids []string) error {
		claimed = append(claimed, ids...)
		return nil
	})
	result := runClaimPipelineWith(t, root, claimPipelineConfig(root), claimer)
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
	writeClaimSource(t, root, sessionID, time.Now().Add(-72*time.Hour))
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, _ []string) error {
		return errors.New("claim store unavailable")
	})
	result := runClaimPipelineWith(t, root, claimPipelineConfig(root), claimer)
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

// writeClaimSource writes one settled or active Claude Code transcript under root
// and returns its path.
func writeClaimSource(t *testing.T, root, sessionID string, modTime time.Time) string {
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

// pendingClaimMarkers returns every pending-claim marker file under root.
func pendingClaimMarkers(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == ".pending-claim" {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s for pending-claim markers: %v", root, err)
	}
	return found
}

// runClaimPipeline runs the production pipeline against root with a claimer that
// records every session id it receives.
func runClaimPipeline(t *testing.T, root string, cfg ingest.PipelineConfig) []string {
	t.Helper()
	var claimed []string
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, ids []string) error {
		claimed = append(claimed, ids...)
		return nil
	})
	runClaimPipelineWith(t, root, cfg, claimer)
	return claimed
}

func runClaimPipelineWith(t *testing.T, root string, cfg ingest.PipelineConfig, claimer ingest.SessionClaimer) *ingest.PipelineResult {
	t.Helper()
	dbPath := filepath.Join(root, "peasant.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return runClaimPipelineOnStore(t, db, cfg, claimer, db)
}

// runClaimPipelineOnStore runs the production pipeline against an open store.
// storeForPipeline may wrap the store to inject a failure while metricsStore
// stays the real store.
func runClaimPipelineOnStore(t *testing.T, metricsStore *store.Store, cfg ingest.PipelineConfig, claimer ingest.SessionClaimer, storeForPipeline ingest.SessionStore) *ingest.PipelineResult {
	t.Helper()
	fs := &ingest.OSFileSystem{}
	pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
		ingest.WithStore(storeForPipeline), ingest.WithMetricsStore(metricsStore),
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

func claimPipelineConfig(root string) ingest.PipelineConfig {
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
	writeClaimSource(t, root, sessionID, time.Now())
	cfg := claimPipelineConfig(root)
	cfg.IncludeActive = true

	first := runClaimPipeline(t, root, cfg)
	if len(first) != 1 || first[0] != sessionID {
		t.Fatalf("first active run claimed = %v, want [%s]", first, sessionID)
	}
	// The recording settles after that first run. It must not be claimed twice.
	writeClaimSource(t, root, sessionID, time.Now().Add(-72*time.Hour))
	second := runClaimPipeline(t, root, cfg)
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
	writeClaimSource(t, root, sessionID, time.Now().Add(-72*time.Hour))
	cfg := claimPipelineConfig(root)
	if first := runClaimPipeline(t, root, cfg); len(first) != 1 {
		t.Fatalf("first run claimed = %v, want the new session", first)
	}
	cfg.Force = true
	if forced := runClaimPipeline(t, root, cfg); len(forced) != 0 {
		t.Fatalf("forced re-process claimed = %v, want none", forced)
	}
}

// firstMirrorFailureStore fails the first artifact mirror and then delegates to
// the real store. The pair is installed before the mirror, so the first run
// leaves the saved files with no database row.
type firstMirrorFailureStore struct {
	*store.Store
	failed bool
}

func (s *firstMirrorFailureStore) MirrorArtifacts(ctx context.Context, requests []ingest.ArtifactMirrorRequest) []ingest.ArtifactMirrorResult {
	if !s.failed {
		s.failed = true
		results := make([]ingest.ArtifactMirrorResult, len(requests))
		for i, request := range requests {
			var sid ingest.SessionID
			if request.Artifact != nil {
				sid = request.Artifact.Metadata.SessionID
			}
			results[i] = ingest.ArtifactMirrorResult{SessionID: sid, Err: errors.New("injected first mirror failure")}
		}
		return results
	}
	return s.Store.MirrorArtifacts(ctx, requests)
}

// TestAutoClaimCarriesFirstStoredAcrossFirstMirrorFailure proves a saved pair
// whose first mirror write failed is still claimed on the next harvest. Without
// the pending-claim marker the next run sees a saved pair and treats the session
// as old, so the automatic claim never happens.
func TestAutoClaimCarriesFirstStoredAcrossFirstMirrorFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const sessionID = "11111111-1111-4111-8111-111111111204"
	writeClaimSource(t, root, sessionID, time.Now().Add(-72*time.Hour))
	cfg := claimPipelineConfig(root)
	dbPath := filepath.Join(root, "peasant.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	wrapped := &firstMirrorFailureStore{Store: db}
	var claimed []string
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, ids []string) error {
		claimed = append(claimed, ids...)
		return nil
	})

	first := runClaimPipelineOnStore(t, db, cfg, claimer, wrapped)
	if len(claimed) != 0 {
		t.Fatalf("first run claimed = %v, want none after the injected mirror failure", claimed)
	}
	if first.Summary.New != 1 {
		t.Fatalf("first run new sessions = %d, want the session counted as new", first.Summary.New)
	}
	if markers := pendingClaimMarkers(t, string(cfg.OutputDir)); len(markers) != 1 {
		t.Fatalf("pending-claim markers after the failed mirror = %v, want one", markers)
	}

	second := runClaimPipelineOnStore(t, db, cfg, claimer, wrapped)
	if len(claimed) != 1 || claimed[0] != sessionID {
		t.Fatalf("second run claimed = %v, want [%s] once the mirror succeeds", claimed, sessionID)
	}
	if second.Summary.Errors != 0 {
		t.Fatalf("second run errors = %d, want the session stored cleanly", second.Summary.Errors)
	}
	if markers := pendingClaimMarkers(t, string(cfg.OutputDir)); len(markers) != 0 {
		t.Fatalf("pending-claim markers after the successful mirror = %v, want none", markers)
	}
}

// TestAutoClaimSkipsMarkerAfterRebuildMirrorFailure proves a mirror failure
// during a rebuild does not turn a previously stored session into first-stored
// evidence. The saved copy existed before the run, so no marker is written and
// the next harvest still claims nothing.
func TestAutoClaimSkipsMarkerAfterRebuildMirrorFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const sessionID = "11111111-1111-4111-8111-111111111299"
	writeClaimSource(t, root, sessionID, time.Now().Add(-72*time.Hour))
	cfg := claimPipelineConfig(root)
	if first := runClaimPipeline(t, root, cfg); len(first) != 1 || first[0] != sessionID {
		t.Fatalf("first run claimed = %v, want [%s]", first, sessionID)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(root, "peasant.db") + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(root, "peasant.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	wrapped := &firstMirrorFailureStore{Store: db}
	var claimed []string
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, ids []string) error {
		claimed = append(claimed, ids...)
		return nil
	})

	runClaimPipelineOnStore(t, db, cfg, claimer, wrapped)
	if len(claimed) != 0 {
		t.Fatalf("rebuild with a failed mirror claimed = %v, want none", claimed)
	}
	if markers := pendingClaimMarkers(t, string(cfg.OutputDir)); len(markers) != 0 {
		t.Fatalf("rebuild mirror failure wrote a marker beside a previously stored session: %v", markers)
	}

	next := runClaimPipelineOnStore(t, db, cfg, claimer, wrapped)
	if len(claimed) != 0 {
		t.Fatalf("a session stored before the run was claimed automatically: %v", claimed)
	}
	if next.Summary.Errors != 0 {
		t.Fatalf("next run errors = %d, want the previously stored session recorded cleanly", next.Summary.Errors)
	}
}

// TestAutoClaimSkipsStoredRowWithoutSavedPair proves the "no database row" half
// of first stored: a row that already exists is not claimed again even when its
// saved pair was deleted and a changed source makes the session mirror again.
func TestAutoClaimSkipsStoredRowWithoutSavedPair(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const sessionID = "11111111-1111-4111-8111-111111111205"
	source := writeClaimSource(t, root, sessionID, time.Now().Add(-72*time.Hour))
	cfg := claimPipelineConfig(root)
	var claimed []string
	claimer := ingest.SessionClaimerFunc(func(_ context.Context, ids []string) error {
		claimed = append(claimed, ids...)
		return nil
	})
	runClaimPipelineWith(t, root, cfg, claimer)
	if len(claimed) != 1 {
		t.Fatalf("first run claimed = %v, want the new session", claimed)
	}
	claimed = nil
	if err := os.RemoveAll(filepath.Join(root, "managed")); err != nil {
		t.Fatal(err)
	}
	// Append a second turn so the stored row is re-processed from a changed
	// source with its saved pair gone. Restoring the settled mtime keeps the
	// session out of the active-only path.
	file, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	secondTurn := `{"sessionId":"` + sessionID + `","type":"user","uuid":"entry-second","timestamp":"2023-11-14T22:14:20Z","cwd":"/workspace","message":{"role":"user","content":"second turn"}}` + "\n"
	if _, err := file.WriteString(secondTurn); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	settled := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(source, settled, settled); err != nil {
		t.Fatal(err)
	}
	second := runClaimPipelineWith(t, root, cfg, claimer)
	if len(claimed) != 0 {
		t.Fatalf("second run claimed = %v, want none for a stored row without a saved pair", claimed)
	}
	if second.Summary.Updated != 1 {
		t.Fatalf("second run summary = %+v, want the stored session updated", second.Summary)
	}
}

// TestAutoClaimSkipsRebuildFromSavedPairs proves deleting the database and
// rebuilding from saved copies (and the full index rebuild) claims nothing.
func TestAutoClaimSkipsRebuildFromSavedPairs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const sessionID = "11111111-1111-4111-8111-111111111203"
	writeClaimSource(t, root, sessionID, time.Now().Add(-72*time.Hour))
	cfg := claimPipelineConfig(root)
	runClaimPipeline(t, root, cfg)

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(root, "peasant.db") + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if rebuilt := runClaimPipeline(t, root, cfg); len(rebuilt) != 0 {
		t.Fatalf("rebuild from saved pairs claimed = %v, want none", rebuilt)
	}
	cfg.RebuildAll = true
	if indexed := runClaimPipeline(t, root, cfg); len(indexed) != 0 {
		t.Fatalf("harvest index --all claimed = %v, want none", indexed)
	}
}
