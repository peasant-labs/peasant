package ingest_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// openCounter records how many times each cleaned database path is opened.
type openCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (counter *openCounter) opener() ingest.OpenCodeSQLiteSourceOpener {
	return func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		counter.mu.Lock()
		counter.counts[filepath.Clean(path.String())]++
		counter.mu.Unlock()
		return ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
	}
}

// TestOpenCodeDiscoverOpensEachDatabaseOnceForDiscovery proves that a single
// Discover opens one supported database exactly twice: once for the schema
// probe and once for discovery, records, and freshness combined. Before the
// open-once change discovery, records, and freshness each opened the database,
// so the count was higher.
func TestOpenCodeDiscoverOpensEachDatabaseOnceForDiscovery(t *testing.T) {
	t.Parallel()
	materialized := testfixture.MaterializeByName(t, "current-session-message")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	counter := &openCounter{counts: make(map[string]int)}
	filesystem := &ingest.OSFileSystem{}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedCandidateEnvironment{}, filesystem, counter.opener(), ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("run OpenCode discovery: %v", err)
	}
	if len(discovered) != 1 {
		t.Fatalf("discovery returned %d sessions, want the one synthetic current session", len(discovered))
	}
	counter.mu.Lock()
	opens := counter.counts[filepath.Clean(materialized.Path)]
	counter.mu.Unlock()
	if opens != 2 {
		t.Fatalf("database %q was opened %d times in one Discover, want 2 (one schema probe and one shared discovery open)", materialized.Path, opens)
	}
}
