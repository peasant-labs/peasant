package ingest_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestPipeline_ClaudeSpawnLinkingConsultsTheStoreAcrossRuns proves the
// pipeline's own discover() call site actually gives the Claude adapter the
// store's session-location lookup, not merely that the adapter behaves
// correctly when a test wires a fake in by hand. The pipeline builds its own
// adapters internally, so a later run can only find an earlier run's spawner
// through the store if the pipeline itself attaches the lookup at the same
// point it already attaches the evidence cache.
//
// Run 1 discovers and stores the parent from host-a alone. Run 2 is a FRESH
// pipeline whose scan root is host-b only — the parent's transcript is not
// even walked, so the only way the child can find it is through the store,
// which by then holds it purely as a side effect of run 1's own write stage.
func TestPipeline_ClaudeSpawnLinkingConsultsTheStoreAcrossRuns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database := openEvidenceStore(t, filepath.Join(root, "peasant.db"))
	outputDir := ingest.ResolvedPath(filepath.Join(root, "peasant-sync"))

	const parentSessionID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const childSessionID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	filesystem := testutil.NewMemFS()
	parentPath := "/claude/host-a/-workspace/" + parentSessionID + ".jsonl"
	parentLines := `{"sessionId":"` + parentSessionID + `","type":"user","toolUseResult":{"status":"teammate_spawned","team_name":"migration","name":"tests"},"message":{"role":"user","content":"spawn in an earlier run"}}` + "\n"
	if err := filesystem.WriteFile(parentPath, []byte(parentLines), 0o644); err != nil {
		t.Fatalf("write parent transcript: %v", err)
	}
	// Older than the staleness threshold below, so classifySession treats it
	// as a finished (not still-being-written) session and the pipeline
	// actually inserts it into the store instead of skipping it as active.
	filesystem.ModTimes[parentPath] = time.Now().Add(-2 * time.Hour)

	cfg1 := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {
				Paths:   []ingest.ResolvedPath{"/claude/host-a"},
				Enabled: true,
			},
		},
		OutputDir:          outputDir,
		StalenessThreshold: time.Minute,
	}
	pipeline1, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(),
		ingest.DefaultAdapterRegistry, cfg1, ingest.WithStore(database))
	if err != nil {
		t.Fatalf("NewPipeline (run 1): %v", err)
	}
	if _, err := pipeline1.Run(ctx); err != nil {
		t.Fatalf("Run (run 1): %v", err)
	}

	hostSlug, parentOfParent, err := database.LookupSessionLocation(ctx, ingest.SessionID(parentSessionID))
	if err != nil {
		t.Fatalf("LookupSessionLocation after run 1: %v", err)
	}
	if hostSlug == "" {
		t.Fatalf("parent session %q was not stored after run 1, so run 2 cannot prove anything", parentSessionID)
	}
	if parentOfParent != "" {
		t.Fatalf("parent session %q unexpectedly already has a parent after run 1: %q", parentSessionID, parentOfParent)
	}

	childPath := "/claude/host-b/-workspace/" + childSessionID + ".jsonl"
	childLines := `{"sessionId":"` + childSessionID + `","type":"user","teamName":"migration","agentName":"tests","message":{"role":"user","content":"write tests in a later run"}}` + "\n"
	if err := filesystem.WriteFile(childPath, []byte(childLines), 0o644); err != nil {
		t.Fatalf("write child transcript: %v", err)
	}
	filesystem.ModTimes[childPath] = time.Now().Add(-2 * time.Hour)

	cfg2 := cfg1
	cfg2.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessClaudeCode: {
			Paths:   []ingest.ResolvedPath{"/claude/host-b"},
			Enabled: true,
		},
	}
	pipeline2, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(),
		ingest.DefaultAdapterRegistry, cfg2, ingest.WithStore(database))
	if err != nil {
		t.Fatalf("NewPipeline (run 2): %v", err)
	}
	if _, err := pipeline2.Run(ctx); err != nil {
		t.Fatalf("Run (run 2): %v", err)
	}

	_, gotParentID, err := database.LookupSessionLocation(ctx, ingest.SessionID(childSessionID))
	if err != nil {
		t.Fatalf("LookupSessionLocation after run 2: %v", err)
	}
	if gotParentID != parentSessionID {
		t.Fatalf("child session %q parent = %q, want %q — the pipeline's discover() must attach the store's "+
			"session-location lookup to the Claude adapter, otherwise cross-run linking cannot see a spawner "+
			"discovered outside this run's own batch",
			childSessionID, gotParentID, parentSessionID)
	}
}
