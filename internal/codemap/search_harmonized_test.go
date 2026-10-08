package codemap_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The consolidated search refuses while the rebuild flag is set and serves
// again after the gated rebuild, through the same service path users run.
func TestSearch_RefusesWhileRebuildFlagged(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	before, err := svc.Search(ctx, "caching", 20)
	if err != nil {
		t.Fatalf("Search before flagging: %v", err)
	}
	if len(before.Results) != 1 {
		t.Fatalf("results before flagging = %d, want 1", len(before.Results))
	}

	conn, err := s.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	if err := store.SearchStateSetNeedsRebuild(ctx, conn); err != nil {
		s.Pool().Put(conn)
		t.Fatalf("set the rebuild flag: %v", err)
	}
	s.Pool().Put(conn)

	if _, err := svc.Search(ctx, "caching", 20); err == nil {
		t.Fatal("Search succeeds while the rebuild flag is set")
	} else if !strings.Contains(err.Error(), "harvest verify --content") {
		t.Fatalf("refusal names %q, want the verify fix", err)
	}

	if _, err := s.EnsureSearchIndexHealthy(ctx); err != nil {
		t.Fatalf("run the index-health gate: %v", err)
	}
	after, err := svc.Search(ctx, "caching", 20)
	if err != nil {
		t.Fatalf("Search after the gated rebuild: %v", err)
	}
	if len(after.Results) != 1 {
		t.Fatalf("results after the rebuild = %d, want 1", len(after.Results))
	}
}

// The unified index is the same production query the store proves: a mirror
// term stays visible through the merged service path while both indexes
// exist.
func TestSearch_MergedPathKeepsMirrorRecall(t *testing.T) {
	t.Parallel()
	svc, s := newFixtureService(t, fxStubRepo())
	ctx := context.Background()

	conn, err := s.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	unified := false
	_ = sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'session_search_fts'`, &sqlitex.ExecOptions{
		ResultFunc: func(*sqlite.Stmt) error { unified = true; return nil },
	})
	s.Pool().Put(conn)
	if !unified {
		t.Skip("consolidated index absent; the merged path needs both indexes")
	}

	got, err := svc.Search(ctx, "pipeline", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results for 'pipeline' = %d, want the two seeded mirror hits", len(got.Results))
	}
}
