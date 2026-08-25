package ingest_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestPipeline_FillsInStoredOriginsAndReportsTheReMine proves the pipeline's own
// call sites, not merely that the pieces behave when a test wires them by hand.
// The pipeline builds its adapters internally, so a resolver nobody constructs
// and a statistics interface nobody asserts against would both pass silently.
//
// Run 1 ingests two roots into an empty store, so its resolve pass has nothing
// to judge, and its discovery has to mine every transcript. Run 2 is a FRESH
// pipeline over the same files and the same store: its resolve pass must find
// the rows run 1 stored and finalise them, and its discovery must reuse the warm
// cache and report no re-mine at all.
func TestPipeline_FillsInStoredOriginsAndReportsTheReMine(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database := openEvidenceStore(t, filepath.Join(root, "peasant.db"))
	outputDir := ingest.ResolvedPath(filepath.Join(root, "peasant-sync"))

	const agentSessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	const personSessionID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"

	filesystem := testutil.NewMemFS()
	transcripts := map[string]string{
		"/claude/-workspace/" + agentSessionID + ".jsonl": `{"sessionId":"` + agentSessionID +
			`","type":"user","teamName":"migration","agentName":"tests","message":{"role":"user","content":"take the failing slice"}}` + "\n",
		"/claude/-workspace/" + personSessionID + ".jsonl": `{"sessionId":"` + personSessionID +
			`","type":"user","message":{"role":"user","content":"<command-name>/share</command-name>"}}` + "\n",
	}
	for path, body := range transcripts {
		if err := filesystem.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write transcript %q: %v", path, err)
		}
		// Older than the staleness threshold, so the run treats the session as
		// finished and actually stores it.
		filesystem.ModTimes[path] = time.Now().Add(-2 * time.Hour)
	}

	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {
				Paths:   []ingest.ResolvedPath{"/claude"},
				Enabled: true,
			},
		},
		OutputDir:          outputDir,
		StalenessThreshold: time.Minute,
	}

	run := func(label string) *ingest.PipelineResult {
		t.Helper()
		pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(),
			ingest.DefaultAdapterRegistry, cfg, ingest.WithStore(database))
		if err != nil {
			t.Fatalf("NewPipeline (%s): %v", label, err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("Run (%s): %v", label, err)
		}
		if result.Summary.OriginResolveError != nil {
			t.Fatalf("resolve pass (%s): %v", label, result.Summary.OriginResolveError)
		}
		return result
	}

	first := run("run 1")
	if first.Summary.ReminedEvidenceRecords == 0 {
		t.Fatal("run 1 reported no re-mined evidence records over a cold cache; either discovery reused something it " +
			"could not have, or the pipeline never type-asserts the adapter for its discovery statistics")
	}
	if first.Summary.OriginResolve.Examined != 0 {
		t.Errorf("run 1 examined %d stored rows, want none: the pass runs BEFORE this run's own writes, so on an empty "+
			"store it has nothing to judge", first.Summary.OriginResolve.Examined)
	}

	stale, err := database.ListStaleOriginSessions(ctx, ingest.OriginRuleVersion)
	if err != nil {
		t.Fatalf("list rows below the version line after run 1: %v", err)
	}
	if len(stale) != len(transcripts) {
		t.Fatalf("run 1 left %d of %d stored rows below the version line; the rows it writes itself are judged by the "+
			"NEXT run's pass, so both must be waiting here", len(stale), len(transcripts))
	}

	second := run("run 2")
	if second.Summary.ReminedEvidenceRecords != 0 {
		t.Errorf("run 2 re-mined %d evidence records over a warm cache, want none: the count is scoped to the most "+
			"recent discovery, so a non-zero value here means the cache was not reused",
			second.Summary.ReminedEvidenceRecords)
	}
	if second.Summary.OriginResolve.Examined != len(stale) {
		t.Fatalf("run 2 examined %d stored rows, want the %d run 1 left below the line",
			second.Summary.OriginResolve.Examined, len(stale))
	}
	if second.Summary.OriginResolve.Degraded != 0 {
		t.Errorf("run 2 left %d rows retryable, want none: every transcript is still on disk and the cache is warm, so "+
			"every row resolved from full evidence", second.Summary.OriginResolve.Degraded)
	}

	remaining, err := database.ListStaleOriginSessions(ctx, ingest.OriginRuleVersion)
	if err != nil {
		t.Fatalf("list rows below the version line after run 2: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d rows are still below the version line after run 2, want none", len(remaining))
	}

	// The verdicts themselves, so a pass that finalised everything at the wrong
	// value would not pass on the watermark alone.
	judged, err := database.ListStaleOriginSessions(ctx, ingest.OriginRuleVersion+1)
	if err != nil {
		t.Fatalf("read back the judged rows: %v", err)
	}
	want := map[string]string{
		agentSessionID:  sessionorigin.Agent.String(),
		personSessionID: sessionorigin.User.String(),
	}
	for _, row := range judged {
		expected, known := want[string(row.SessionID)]
		if !known {
			continue
		}
		if row.StoredOrigin != expected {
			t.Errorf("session %q carries origin %q, want %q", row.SessionID, row.StoredOrigin, expected)
		}
		delete(want, string(row.SessionID))
	}
	for id := range want {
		t.Errorf("session %q was never read back, so its verdict was not checked", id)
	}
}

// TestClaudeReminedCountDescribesOnlyTheMostRecentDiscovery pins the one part of
// the DiscoveryStatistics contract the interface cannot enforce: the count
// belongs to the LAST Discover, not to everything the adapter has ever done.
//
// One adapter, two discoveries over the same unchanged files. The first mines
// both transcripts; the second reuses the cache the first wrote. An adapter that
// accumulated instead of replacing would still report the first run's work here,
// and the second-run gate in every consumer of this count would be unfalsifiable.
func TestClaudeReminedCountDescribesOnlyTheMostRecentDiscovery(t *testing.T) {
	ctx := context.Background()
	database := openEvidenceStore(t, filepath.Join(t.TempDir(), "peasant.db"))

	filesystem := testutil.NewMemFS()
	transcripts := []string{
		"/claude/-workspace/eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee.jsonl",
		"/claude/-workspace/ffffffff-ffff-4fff-8fff-ffffffffffff.jsonl",
	}
	for _, path := range transcripts {
		body := `{"type":"user","message":{"role":"user","content":"a plain prompt"}}` + "\n"
		if err := filesystem.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write transcript %q: %v", path, err)
		}
	}

	adapter := ingest.NewClaudeAdapter(filesystem, testutil.DefaultGitResolver(), salt.Salt{})
	ingest.AttachClaudeEvidenceCache(adapter, database)
	cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{"/claude"}, Enabled: true}

	if _, err := adapter.Discover(ctx, cfg); err != nil {
		t.Fatalf("first discovery: %v", err)
	}
	first := adapter.ReminedCount()
	if first != len(transcripts) {
		t.Fatalf("the first discovery re-mined %d of %d transcripts over a cold cache; the measurement below only means "+
			"something if the first run really did the work", first, len(transcripts))
	}

	if _, err := adapter.Discover(ctx, cfg); err != nil {
		t.Fatalf("second discovery: %v", err)
	}
	if second := adapter.ReminedCount(); second != 0 {
		t.Errorf("the second discovery on the SAME adapter reports %d re-mined records, want 0: the count describes the "+
			"most recent Discover, so it must be replaced by each call rather than accumulated across them", second)
	}
}
