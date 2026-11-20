package store

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_rebuild_budget.yaml
var searchRebuildBudgetYAML []byte

// searchRebuildEntry names the production path a budget case rebuilds
// through. The set is closed.
type searchRebuildEntry string

const (
	searchRebuildEntryRepairStep  searchRebuildEntry = "repair-step"
	searchRebuildEntrySearchIndex searchRebuildEntry = "search-index-rebuild"
)

// searchRebuildOutcome names how a budget case ends. The set is closed.
type searchRebuildOutcome string

const (
	searchRebuildOutcomeCommit searchRebuildOutcome = "commit"
	searchRebuildOutcomeFail   searchRebuildOutcome = "fail"
	searchRebuildOutcomeCancel searchRebuildOutcome = "cancel"
)

type searchRebuildBudgetFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Filler        struct {
		Rows       int `yaml:"rows"`
		RowBytes   int `yaml:"rowBytes"`
		Vocabulary int `yaml:"vocabulary"`
	} `yaml:"filler"`
	Queries []string `yaml:"queries"`
	Cases   []struct {
		Name    string               `yaml:"name"`
		Entry   searchRebuildEntry   `yaml:"entry"`
		Outcome searchRebuildOutcome `yaml:"outcome"`
	} `yaml:"cases"`
}

func loadSearchRebuildBudgetFixture(t *testing.T) searchRebuildBudgetFixture {
	t.Helper()
	var fixture searchRebuildBudgetFixture
	if err := yaml.Unmarshal(searchRebuildBudgetYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		switch c.Entry {
		case searchRebuildEntryRepairStep, searchRebuildEntrySearchIndex:
		default:
			t.Fatalf("case %s names entry %q, outside {%s, %s}", c.Name, c.Entry, searchRebuildEntryRepairStep, searchRebuildEntrySearchIndex)
		}
		switch c.Outcome {
		case searchRebuildOutcomeCommit, searchRebuildOutcomeFail, searchRebuildOutcomeCancel:
		default:
			t.Fatalf("case %s names outcome %q, outside {%s, %s, %s}", c.Name, c.Outcome, searchRebuildOutcomeCommit, searchRebuildOutcomeFail, searchRebuildOutcomeCancel)
		}
		if c.Entry == searchRebuildEntrySearchIndex && c.Outcome != searchRebuildOutcomeCommit {
			t.Fatalf("case %s: the search index rebuild has no failure seam after its rebuild; give it outcome %s", c.Name, searchRebuildOutcomeCommit)
		}
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredNames, names, "search rebuild budget"); err != nil {
		t.Fatal(err)
	}
	fillerSized := fixture.Filler.Rows > 0 && fixture.Filler.RowBytes > 0 && fixture.Filler.Vocabulary > 0
	if !fillerSized || len(fixture.Queries) == 0 {
		t.Fatal("search rebuild budget fixture has no filler or no queries")
	}
	return fixture
}

// TestSearchRebuildBudget proves that both whole-index rebuild paths use the
// widened postings budget (one segment where FTS5's default writes several),
// return exactly the rows and ranks a default-budget rebuild returns, and
// leave no stored budget behind, also when the search trigger repair fails or
// is cancelled inside its transaction.
func TestSearchRebuildBudget(t *testing.T) {
	t.Parallel()
	fixture := loadSearchRebuildBudgetFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "budget.db")
			conn := openMigrationV76Store(t, path, "delete", false)
			removeBodySentinel(t, conn)
			seedSearchRebuildFiller(t, conn, fixture.Filler.Rows, fixture.Filler.RowBytes, fixture.Filler.Vocabulary)
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			if c.Entry == searchRebuildEntrySearchIndex {
				if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
					t.Fatalf("upgrade the store before the search index rebuild: %v", err)
				}
			}

			// The reference: FTS5's default budget, on a copy.
			reference := filepath.Join(dir, "reference.db")
			copySearchRebuildStore(t, path, reference)
			refConn, err := sqlite.OpenConn(reference, sqlite.OpenReadWrite)
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlitex.ExecuteTransient(refConn, `INSERT INTO session_search_fts(session_search_fts) VALUES('rebuild')`, nil); err != nil {
				t.Fatal(err)
			}
			wantDigest := searchRebuildDigest(t, refConn, fixture.Queries)
			if segments := searchIndexSegments(t, refConn); segments < 2 {
				t.Fatalf("a default-budget rebuild wrote %d segment(s); the filler must need several flushes for the case to prove the wider budget", segments)
			}
			configBefore := searchConfigRows(t, refConn)
			if err := refConn.Close(); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("synthetic failure after the repair's rebuild")
			var runErr error
			switch c.Entry {
			case searchRebuildEntryRepairStep:
				opts := UpgradeOptions{}
				switch c.Outcome {
				case searchRebuildOutcomeFail:
					opts.afterDataFill = func(version int) error {
						if version == searchRepairSchemaVersion {
							return failure
						}
						return nil
					}
				case searchRebuildOutcomeCancel:
					opts.afterDataFill = func(version int) error {
						if version == searchRepairSchemaVersion {
							cancel()
						}
						return nil
					}
				}
				var report UpgradeReport
				report, runErr = Upgrade(ctx, path, opts)
				if runErr == nil {
					repaired := false
					for _, step := range report.Steps {
						repaired = repaired || (step.Step.Version == searchRepairSchemaVersion && step.Step.Cost.Long())
					}
					if !repaired {
						t.Fatal("the upgrade did not plan the long search trigger repair; the store carries no residue")
					}
				}
			case searchRebuildEntrySearchIndex:
				s, err := Open(path, WithPoolSize(1))
				if err != nil {
					t.Fatal(err)
				}
				runErr = s.RebuildSearchIndex(ctx, nil)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}

			if c.Outcome != searchRebuildOutcomeCommit {
				if runErr == nil {
					t.Fatal("the upgrade succeeded; the case's failure did not reach the repair step")
				}
				if c.Outcome == searchRebuildOutcomeFail && !errors.Is(runErr, failure) {
					t.Fatalf("upgrade error = %v, want the synthetic failure", runErr)
				}
				stopped, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
				if err != nil {
					t.Fatal(err)
				}
				version, err := readPragmaInt(stopped, "PRAGMA user_version")
				if err != nil {
					t.Fatal(err)
				}
				if int(version) != searchRepairSchemaVersion-1 {
					t.Errorf("the store is at schema %d after the stopped repair, want %d: the step must roll back", version, searchRepairSchemaVersion-1)
				}
				if err := ftsContentIntegrityCheck(stopped); err == nil {
					t.Error("the stale results passed the integrity check after the stopped repair; the repair did not roll back")
				}
				// A failed step removes the budget at once. A cancelled one may
				// leave it on the behind store, for the next upgrade to remove.
				if got := searchConfigRows(t, stopped); c.Outcome == searchRebuildOutcomeFail && !equalStrings(got, configBefore) {
					t.Errorf("search index configuration after the failed repair = %q, want %q as before", got, configBefore)
				}
				if err := stopped.Close(); err != nil {
					t.Fatal(err)
				}
				if c.Outcome == searchRebuildOutcomeFail {
					return
				}
				if _, runErr = Upgrade(t.Context(), path, UpgradeOptions{}); runErr != nil {
					t.Fatalf("the upgrade after the cancelled repair: %v", runErr)
				}
			}
			if runErr != nil {
				t.Fatal(runErr)
			}

			conn, err = sqlite.OpenConn(path, sqlite.OpenReadWrite)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if got := searchConfigRows(t, conn); !equalStrings(got, configBefore) {
				t.Errorf("search index configuration after the rebuild = %q, want %q as before: the widened budget must not stay stored", got, configBefore)
			}
			if got := searchRebuildDigest(t, conn, fixture.Queries); got != wantDigest {
				t.Error("the rebuild returned different rows or ranks than a rebuild at FTS5's default budget")
			}
			if segments := searchIndexSegments(t, conn); segments != 1 {
				t.Errorf("the rebuild wrote %d segments, want 1: the widened budget holds every posting of this store", segments)
			}
		})
	}
}

// seedSearchRebuildFiller adds body rows of filler text; the body trigger
// indexes them into the union index.
func seedSearchRebuildFiller(t *testing.T, conn *sqlite.Conn, rows, rowBytes, vocabulary int) {
	t.Helper()
	words := rand.New(rand.NewPCG(76, 64))
	end := sqlitex.Transaction(conn)
	var err error
	defer end(&err)
	for row := 0; row < rows && err == nil; row++ {
		var text strings.Builder
		for text.Len() < rowBytes {
			fmt.Fprintf(&text, "w%d ", words.IntN(vocabulary))
		}
		err = sqlitex.ExecuteTransient(conn, `INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth)
VALUES((SELECT coalesce(max(body_id), ?) + 1 FROM session_entry_bodies), ?, printf('%064x', ?), ?, 'claude-code', 'text', 'assistant', ?, 0, 0, 0, 0)`,
			&sqlitex.ExecOptions{Args: []any{BodyRowIDBase - 1, v76BodiesSession, row + 1000, row + 100, text.String()}})
	}
	if err != nil {
		t.Fatalf("seed the filler bodies: %v", err)
	}
}

// searchRebuildDigest hashes every query's rows and bm25 ranks in rank order.
// A query that matches nothing fails the test: it would prove nothing.
func searchRebuildDigest(t *testing.T, conn *sqlite.Conn, queries []string) string {
	t.Helper()
	hash := sha256.New()
	for _, query := range queries {
		rows := 0
		if err := sqlitex.ExecuteTransient(conn, `SELECT rowid, rank FROM session_search_fts WHERE session_search_fts MATCH ? ORDER BY rank, rowid`, &sqlitex.ExecOptions{
			Args: []any{query},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows++
				_, _ = fmt.Fprintf(hash, "%s %d %.12g\n", query, stmt.ColumnInt64(0), stmt.ColumnFloat(1))
				return nil
			},
		}); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if rows == 0 {
			t.Fatalf("query %q matched nothing; it proves nothing about the rebuild", query)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// searchIndexSegments counts the union index's segments.
func searchIndexSegments(t *testing.T, conn *sqlite.Conn) int {
	t.Helper()
	segments := 0
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(DISTINCT segid) FROM session_search_fts_idx`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { segments = stmt.ColumnInt(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	return segments
}

// searchConfigRows lists the union index's stored configuration as k=v.
func searchConfigRows(t *testing.T, conn *sqlite.Conn) []string {
	t.Helper()
	var rows []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT k, v FROM session_search_fts_config`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, stmt.ColumnText(0)+"="+stmt.ColumnText(1))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(rows)
	return rows
}

// copySearchRebuildStore copies a closed store file with no sidecar.
func copySearchRebuildStore(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
