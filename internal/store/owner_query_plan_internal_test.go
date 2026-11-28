package store

import (
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/owner_claim_queries.yaml
var ownerClaimQueriesYAML []byte

type ownerClaimQueriesFixture struct {
	RequiredNames []string                 `yaml:"requiredNames"`
	Cases         []ownerClaimQueryFixture `yaml:"cases"`
}

type ownerClaimQueryFixture struct {
	Name string `yaml:"name"`
}

// TestOwnerClaimReadsUseNoTemporarySort refuses a temporary B-tree for the page
// read (with and without the older cursor) and for the strict-reuse lookup.
// Write order (rowid) is the only order key, so each read is a reverse row-order
// scan that stops at its LIMIT and needs no claim-log index.
func TestOwnerClaimReadsUseNoTemporarySort(t *testing.T) {
	t.Parallel()
	var fixture ownerClaimQueriesFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(ownerClaimQueriesYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode owner_claim_queries.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if strings.Join(names, ",") != strings.Join(fixture.RequiredNames, ",") {
		t.Fatalf("owner_claim_queries.yaml cases = %v, want %v", names, fixture.RequiredNames)
	}
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

	queries := map[string]struct {
		sql  string
		args []any
	}{
		"page-first":   {sql: ownerClaimsPageSQL(false), args: []any{"lo_plan", OwnerClaimsPageSize + 1}},
		"page-older":   {sql: ownerClaimsPageSQL(true), args: []any{"lo_plan", int64(1), OwnerClaimsPageSize + 1}},
		"strict-reuse": {sql: newestOwnerClaimSQL, args: []any{"lo_plan"}},
	}
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			query, ok := queries[testCase.Name]
			if !ok {
				t.Fatalf("owner_claim_queries.yaml case %q has no query", testCase.Name)
			}
			var details []string
			if err := sqlitex.ExecuteTransient(conn, "EXPLAIN QUERY PLAN "+query.sql, &sqlitex.ExecOptions{
				Args: query.args,
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
			if strings.Contains(plan, "idx_owner_claims") {
				t.Fatalf("plan uses a removed claim-log index:\n%s", plan)
			}
		})
	}
}

// TestOwnerClaimsHasOnlyItsIdentityIndex pins the claim log's index set: the
// only allowed index is the one SQLite builds for the id primary key. A new
// named index (whatever its name) would make the reverse row-order reads cost
// more and is not needed.
func TestOwnerClaimsHasOnlyItsIdentityIndex(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "owner-claim-index.db"), WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.pool.Put(conn)

	type claimIndex struct {
		name   string
		unique int64
		origin string
	}
	var indexes []claimIndex
	if err := sqlitex.ExecuteTransient(conn, `SELECT name, "unique", origin FROM pragma_index_list('owner_claims') ORDER BY seq`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			indexes = append(indexes, claimIndex{name: stmt.ColumnText(0), unique: stmt.ColumnInt64(1), origin: stmt.ColumnText(2)})
			return nil
		},
	}); err != nil {
		t.Fatalf("read owner_claims indexes: %v", err)
	}
	if len(indexes) == 0 {
		t.Fatal("owner_claims has no index for its id primary key")
	}
	for _, index := range indexes {
		if index.unique != 1 {
			t.Fatalf("owner_claims index %q is not unique; only the id primary key may be indexed", index.name)
		}
		var columns []string
		if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, &sqlitex.ExecOptions{
			Args: []any{index.name},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				columns = append(columns, stmt.ColumnText(0))
				return nil
			},
		}); err != nil {
			t.Fatalf("read columns of owner_claims index %q: %v", index.name, err)
		}
		if strings.Join(columns, ",") != "id" {
			t.Fatalf("owner_claims index %q covers %v, want only the id primary key", index.name, columns)
		}
		if index.origin != "pk" && index.origin != "u" {
			t.Fatalf("owner_claims index %q has origin %q; only a primary-key or unique-id index is allowed", index.name, index.origin)
		}
	}
}
