package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestOwnerClaimReadsUseTheirIndexes refuses a temporary B-tree for the page
// read (with and without the older cursor) and for the strict-reuse lookup.
// The design record's indexes must serve all three.
func TestOwnerClaimReadsUseTheirIndexes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := Open(filepath.Join(t.TempDir(), "owner-claim-plan.db"), WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.pool.Put(conn)

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{name: "page-first", sql: ownerClaimsPageSQL(false), args: []any{"lo_plan", OwnerClaimsPageSize + 1}},
		{name: "page-older", sql: ownerClaimsPageSQL(true), args: []any{"lo_plan", int64(1000), int64(1), OwnerClaimsPageSize + 1}},
		{name: "strict-reuse", sql: newestOwnerClaimSQL, args: []any{"lo_plan"}},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			var details []string
			if err := sqlitex.ExecuteTransient(conn, "EXPLAIN QUERY PLAN "+testCase.sql, &sqlitex.ExecOptions{
				Args: testCase.args,
				ResultFunc: func(stmt *sqlite.Stmt) error {
					details = append(details, stmt.ColumnText(3))
					return nil
				},
			}); err != nil {
				t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
			}
			plan := strings.Join(details, "\n")
			if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
				t.Fatalf("plan uses a temporary sort:\n%s", plan)
			}
			if !strings.Contains(plan, "idx_owner_claims_owner_order") {
				t.Fatalf("plan does not use idx_owner_claims_owner_order:\n%s", plan)
			}
		})
	}
}
