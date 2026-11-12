package harnesslayout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
)

type antigravityDeniedReadsFixture struct {
	Session string `yaml:"session"`
	Cases   []struct {
		Name  string `yaml:"name"`
		Reads []struct {
			Database string `yaml:"database"`
			SQL      string `yaml:"sql"`
		} `yaml:"reads"`
		Denial *struct {
			Database string `yaml:"database"`
			Table    string `yaml:"table"`
			Column   string `yaml:"column"`
		} `yaml:"denial"`
		Refused *struct {
			Table  string `yaml:"table"`
			Column string `yaml:"column"`
			Rule   string `yaml:"rule"`
		} `yaml:"refused"`
	} `yaml:"cases"`
}

// parseAntigravityDatabase is the closed set of database tokens the fixture
// may use.
func parseAntigravityDatabase(t *testing.T, raw string) antigravityDatabase {
	t.Helper()
	switch database := antigravityDatabase(raw); database {
	case antigravityBodyDatabase, antigravityIndexDatabase:
		return database
	}
	t.Fatalf("unknown database %q in antigravity_denied_reads.yaml; use body or index", raw)
	return ""
}

func parseAntigravityRule(t *testing.T, raw string) antigravityRule {
	t.Helper()
	switch raw {
	case "deny-list":
		return antigravityRuleList
	case "blob-column":
		return antigravityRuleBlob
	}
	t.Fatalf("unknown rule %q in antigravity_denied_reads.yaml; use deny-list or blob-column", raw)
	return ""
}

// TestAntigravityDeniedReads holds the read guard: every read that can return
// conversation text fails through antigravityOpen, names what it refused, and
// produces no row.
func TestAntigravityDeniedReads(t *testing.T) {
	var fixture antigravityDeniedReadsFixture
	loadAntigravityFixture(t, "antigravity_denied_reads", &fixture)
	store := loadAntigravityStore(t)

	dir := t.TempDir()
	rels := map[antigravityDatabase]string{
		antigravityBodyDatabase:  antigravityBody.pattern(fixture.Session),
		antigravityIndexDatabase: antigravityIndexDB.Pattern,
	}
	store.writeBody(t, filepath.Join(dir, filepath.FromSlash(rels[antigravityBodyDatabase])), true)
	store.writeIndex(t, filepath.Join(dir, rels[antigravityIndexDatabase]), []string{fixture.Session})
	src := Source{FS: os.DirFS(dir), Dir: dir}

	// The denials of the fixture equal the production list, in both directions.
	listed := map[antigravityDenial]bool{}
	for _, denial := range antigravityDeniedReads {
		listed[denial] = true
	}
	covered := map[antigravityDenial]bool{}
	for _, tc := range fixture.Cases {
		if tc.Denial == nil {
			continue
		}
		denial := antigravityDenial{Database: parseAntigravityDatabase(t, tc.Denial.Database), Table: tc.Denial.Table, Column: tc.Denial.Column}
		covered[denial] = true
		if !listed[denial] {
			t.Errorf("case %q covers %+v, which antigravityDeniedReads does not list; add it to the list or remove the case", tc.Name, denial)
		}
	}
	for denial := range listed {
		if !covered[denial] {
			t.Errorf("antigravityDeniedReads lists %+v and no case covers it; add a case to antigravity_denied_reads.yaml", denial)
		}
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if len(tc.Reads) == 0 {
				t.Fatal("case has no reads")
			}
			for _, read := range tc.Reads {
				database := parseAntigravityDatabase(t, read.Database)
				rows, err := runGuardedRead(t, src, rels[database], database, read.SQL)
				if tc.Refused == nil {
					if err != nil {
						t.Errorf("%s: %v, want an allowed read", read.SQL, err)
					}
					continue
				}
				if err == nil {
					t.Errorf("%s succeeded with %d rows, want the read guard to refuse it", read.SQL, rows)
					continue
				}
				if code := sqlite.ErrCode(err); code != sqlite.ResultAuth {
					t.Errorf("%s failed with %v, want the read guard's %v", read.SQL, code, sqlite.ResultAuth)
				}
				if rows != 0 {
					t.Errorf("%s produced %d rows before failing, want none", read.SQL, rows)
				}
				rule := parseAntigravityRule(t, tc.Refused.Rule)
				for _, want := range []string{
					fmt.Sprintf("column %s of table %s", tc.Refused.Column, tc.Refused.Table),
					string(rule),
					"antigravity_core.go",
					"conversation text",
					"No value was read",
					"Remove the column from the query",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: error %q does not contain %q", read.SQL, err, want)
					}
				}
			}
		})
	}
}

func runGuardedRead(t *testing.T, src Source, rel string, database antigravityDatabase, query string) (int, error) {
	t.Helper()
	db, err := antigravityOpen(context.Background(), src, rel, database)
	if err != nil {
		t.Fatalf("antigravityOpen(%s): %v", rel, err)
	}
	defer db.Close()
	rows := 0
	err = db.query(query, nil, func(*sqlite.Stmt) error {
		rows++
		return nil
	})
	return rows, err
}
