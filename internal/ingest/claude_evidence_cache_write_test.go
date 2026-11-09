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

// failingClaudeEvidenceCache is an evidence cache whose write always fails. The
// transcript evidence is still readable, so discovery is correct; only caching
// it fails, which is the condition a run must report instead of hiding.
type failingClaudeEvidenceCache struct {
	writeErr error
	loads    int
	saves    int
}

func (c *failingClaudeEvidenceCache) LoadClaudeEvidence(context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	c.loads++
	return map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence{}, nil
}

func (c *failingClaudeEvidenceCache) LoadClaudeEvidenceFor(context.Context, []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	c.loads++
	return map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence{}, nil
}

func (c *failingClaudeEvidenceCache) SaveClaudeEvidence(context.Context, []ingest.ClaudeTranscriptEvidence, []ingest.ResolvedPath) error {
	c.saves++
	return c.writeErr
}

// TestClaudeEvidenceCacheWriteIsReported pins the two reporting rules for a
// failed evidence-cache write: the full walk records a diagnostic and simply
// mines the transcripts again next time, and the located build returns the
// failure from its deferred write so the run can stop before any session write.
// Restoring the discarded error in saveMinedEvidence makes the diagnostic half
// red, and swallowing the deferred write makes the located half red.
func TestClaudeEvidenceCacheWriteIsReported(t *testing.T) {
	t.Parallel()

	const rootPath = "/claude"
	const sessionID = ingest.SessionID("11111111-1111-4111-8111-111111111111")
	writeErr := errors.New("evidence cache write refused by the test double")

	newAdapter := func() (*ingest.ClaudeAdapter, *failingClaudeEvidenceCache) {
		mfs := testutil.NewMemFS()
		body := `{"sessionId":"` + string(sessionID) + `","type":"user","cwd":"/work/project","message":{"role":"user","content":"hello"}}` + "\n"
		if err := mfs.WriteFile(rootPath+"/project-a/"+string(sessionID)+".jsonl", []byte(body), 0o644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		cache := &failingClaudeEvidenceCache{writeErr: writeErr}
		adapter := ingest.NewClaudeAdapter(mfs, testutil.NoGitResolver(), salt.Salt{})
		ingest.AttachClaudeEvidenceCache(adapter, cache)
		ingest.AttachClaudeDerivedLookup(adapter, &claudeLocatorDerivedLookup{})
		return adapter, cache
	}
	cfg := ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(rootPath)}}

	t.Run("full-walk-records-a-diagnostic-and-retries", func(t *testing.T) {
		adapter, cache := newAdapter()
		for run := 1; run <= 2; run++ {
			sessions, err := adapter.Discover(context.Background(), cfg)
			if err != nil {
				t.Fatalf("run %d: Discover returned an error for a failed cache write: %v", run, err)
			}
			if len(sessions) != 1 {
				t.Fatalf("run %d: discovered %d sessions, want 1; a cache failure must not change discovery", run, len(sessions))
			}
			diagnostics := adapter.DiscoveryDiagnostics()
			found := false
			for _, diagnostic := range diagnostics {
				if diagnostic.Code == ingest.DiscoveryCodeClaudeEvidenceCacheWrite {
					found = true
				}
			}
			if !found {
				t.Fatalf("run %d: a failed evidence-cache write was not reported; diagnostics = %+v", run, diagnostics)
			}
			if adapter.ReminedCount() != 1 {
				t.Fatalf("run %d: ReminedCount() = %d, want 1; an unwritten cache means the next run mines the transcript again", run, adapter.ReminedCount())
			}
		}
		if cache.saves != 2 {
			t.Fatalf("the full walk attempted %d cache write(s), want 2 (one per run)", cache.saves)
		}
	})

	t.Run("located-build-returns-the-write-failure", func(t *testing.T) {
		adapter, _ := newAdapter()
		at := ingest.LocatedSource{
			ID:   sessionID,
			Path: ingest.ResolvedPath(rootPath + "/project-a/" + string(sessionID) + ".jsonl"),
		}
		build, err := adapter.DiscoverSessionAt(context.Background(), cfg, at)
		if err != nil {
			t.Fatalf("DiscoverSessionAt returned an error before the deferred write was applied: %v", err)
		}
		if len(build.Members) != 1 {
			t.Fatalf("DiscoverSessionAt returned %d members, want 1", len(build.Members))
		}
		if build.Deferred == nil {
			t.Fatal("DiscoverSessionAt returned no deferred writes")
		}
		if err := build.Deferred.Apply(context.Background()); err == nil {
			t.Fatal("the deferred evidence write swallowed the cache's failure; the run would continue to a session write")
		} else if !errors.Is(err, writeErr) {
			t.Fatalf("deferred write error = %v, want it to wrap the cache's error", err)
		} else if !strings.Contains(err.Error(), "evidence") {
			t.Fatalf("deferred write error %q does not name the evidence write", err)
		}
	})
}
