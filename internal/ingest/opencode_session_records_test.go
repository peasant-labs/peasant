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
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestOpenCodeSessionRecordsDegradePerRow proves that one undecodable session
// row is dropped with a diagnostic while every other session keeps its parent
// link and clock. Before the per-row degrade the whole session-record read
// aborted on the first bad row and every session lost its parent link.
func TestOpenCodeSessionRecordsDegradePerRow(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "session-records-degrade")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	const child = "ses_3cd91f52effeXd3QAJ54jOyzB1"
	const parent = "ses_3cd91f52effeXd3QAJ54jOyzB2"
	// Link the child to the parent with a good row, then corrupt the parent's
	// own session row by storing a non-integer time_updated. The parent row must
	// be dropped without losing the child's parent link.
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		if err := sqlitex.Execute(connection, `UPDATE session SET parent_id = ?2 WHERE id = ?1`, &sqlitex.ExecOptions{Args: []any{child, parent}}); err != nil {
			return err
		}
		return sqlitex.Execute(connection, `UPDATE session SET time_updated = 'not-an-integer' WHERE id = ?1`, &sqlitex.ExecOptions{Args: []any{parent}})
	})

	adapter := parentClockAdapter(t)
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover with a corrupt session row: %v", err)
	}
	var childSession *ingest.DiscoveredSession
	for index := range discovered {
		if string(discovered[index].SessionID) == child {
			childSession = &discovered[index]
		}
	}
	if childSession == nil {
		t.Fatalf("degrade discovery = %+v, want the child session discovered", discovered)
	}
	if childSession.ParentUUID == nil || string(*childSession.ParentUUID) != parent {
		t.Fatalf("child parent link = %v, want %q kept while the corrupt parent row is dropped", childSession.ParentUUID, parent)
	}
	if diagnostic := discoveryFailedDiagnostic(adapter.CandidateEvidence(), materialized.Path); diagnostic == nil {
		t.Fatalf("the dropped corrupt session row recorded no diagnostic: evidence=%+v", adapter.CandidateEvidence())
	}
}

// TestSessionRecordsReportOneDropPerBadRowAcrossPages proves that a single
// undecodable session row is reported once, not once per page. The bad row is
// the last row of the first page, so the earlier cursor design re-fetched it on
// the next page and counted the same drop twice.
func TestSessionRecordsReportOneDropPerBadRowAcrossPages(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "session-records-degrade")
	// The second session row by identifier order carries a non-integer clock, so
	// its identifier stays valid but the row is dropped. With page size one it is
	// the sentinel row of the first page.
	const corrupt = "ses_3cd91f52effeXd3QAJ54jOyzB2"
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, `UPDATE session SET time_updated = 'not-an-integer' WHERE id = ?1`, &sqlitex.ExecOptions{Args: []any{corrupt}})
	})

	path, err := ingest.NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate synthetic source path: %v", err)
	}
	source, err := ingest.OpenOpenCodeSQLiteSource(t.Context(), path, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("open synthetic source: %v", err)
	}
	defer func() { _ = source.Close(t.Context()) }()

	pageSize, err := ingest.NewOpenCodeCurrentPageSize(1)
	if err != nil {
		t.Fatalf("build bounded page size: %v", err)
	}

	drops := 0
	var cursor *ingest.OpenCodeSessionRecordCursor
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("session-record pagination did not terminate")
		}
		page, readErr := source.SessionRecords(t.Context(), ingest.OpenCodeSessionRecordPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			t.Fatalf("read session-record page %d: %v", pages, readErr)
		}
		drops += len(page.Skipped)
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}

	if drops != 1 {
		t.Fatalf("one corrupt session row reported %d drops across pages, want exactly one", drops)
	}
}

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
