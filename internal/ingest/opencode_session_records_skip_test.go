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

// emptyEnumRecordCountingSource reports no legacy sessions and counts every
// session-record read so a test can prove the read is skipped with no
// candidates.
type emptyEnumRecordCountingSource struct {
	ingest.OpenCodeSQLiteSource
	counter *int
	mu      *sync.Mutex
}

func (source emptyEnumRecordCountingSource) LegacySessionIDs(context.Context, ingest.OpenCodeLegacySessionPageRequest) (ingest.OpenCodeLegacySessionPage, error) {
	return ingest.OpenCodeLegacySessionPage{}, nil
}

func (source emptyEnumRecordCountingSource) SessionRecords(ctx context.Context, request ingest.OpenCodeSessionRecordPageRequest) (ingest.OpenCodeSessionRecordPage, error) {
	source.mu.Lock()
	*source.counter++
	source.mu.Unlock()
	return source.OpenCodeSQLiteSource.SessionRecords(ctx, request)
}

// TestOpenCodeSessionRecordsSkippedWithNoCandidates proves that discovery does
// not read the session table when a supported database enumerates no sessions.
func TestOpenCodeSessionRecordsSkippedWithNoCandidates(t *testing.T) {
	t.Parallel()
	materialized := testfixture.MaterializeByName(t, "legacy-message-part")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	var mu sync.Mutex
	recordReads := 0
	opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if openErr != nil {
			return nil, openErr
		}
		return emptyEnumRecordCountingSource{OpenCodeSQLiteSource: source, counter: &recordReads, mu: &mu}, nil
	}
	filesystem := &ingest.OSFileSystem{}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedCandidateEnvironment{}, filesystem, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	if _, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}}); err != nil {
		t.Fatalf("discover with no enumerated sessions: %v", err)
	}
	mu.Lock()
	reads := recordReads
	mu.Unlock()
	if reads != 0 {
		t.Fatalf("session table was read %d times with no candidates, want 0", reads)
	}
}
