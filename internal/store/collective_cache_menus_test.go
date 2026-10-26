package store_test

import (
	_ "embed"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"reflect"
	"regexp"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/collective_cache_menus.yaml
var cacheMenusYAML []byte

func menuStrings[T ~string](values []T) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, string(value))
	}
	sort.Strings(result)
	return result
}

func TestCollectiveCacheSQLMenusMatchCanonicalSets(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name   string `yaml:"name"`
			Table  string `yaml:"table"`
			Column string `yaml:"column"`
			Menu   string `yaml:"menu"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(cacheMenusYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if names[c.Name] {
			t.Fatal("duplicate SQL menu fixture")
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("missing required SQL menu fixture %s", name)
		}
	}
	s := storetest.Open(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var expected []string
			switch c.Menu {
			case "observation-state":
				expected = menuStrings(schema.AllCollectiveObservationStates)
			case "observation-reason":
				expected = menuStrings(schema.AllCollectiveObservationReasons)
			case "acceptance":
				expected = menuStrings(schema.AllVillageGroupAcceptanceModes)
			case "access":
				expected = menuStrings(schema.AllVillageGroupDataAccessPolicies)
			case "receipt-operation":
				expected = menuStrings([]schema.CollectiveOperation{schema.CollectiveOperationPublish, schema.CollectiveOperationShare})
			case "receipt-outcome":
				expected = menuStrings(schema.AllLocalConfirmedContributionOutcomes)
			case "operation":
				expected = menuStrings(schema.AllCollectiveOperations)
			case "diagnostic-outcome":
				expected = menuStrings(schema.AllLocalOperationDiagnosticOutcomes)
			case "issue":
				expected = menuStrings(schema.AllOperationIssueCodes)
			case "coverage":
				expected = menuStrings(schema.AllCollectiveEvidenceCoverages)
			case "source":
				expected = menuStrings(schema.AllCollectiveObservationSources)
			case "domain":
				expected = menuStrings(schema.AllCollectiveFetchDomains)
			default:
				t.Fatal("unknown canonical menu")
			}
			conn, err := s.Pool().Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var sql string
			err = sqlitex.Execute(conn, `SELECT sql FROM sqlite_schema WHERE type='table' AND name=?`, &sqlitex.ExecOptions{Args: []any{c.Table}, ResultFunc: func(stmt *sqlite.Stmt) error { sql = stmt.ColumnText(0); return nil }})
			s.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			pattern := regexp.MustCompile(`(?is)` + regexp.QuoteMeta(c.Column) + `\s+IN\s*\(([^)]*)\)`)
			match := pattern.FindStringSubmatch(sql)
			if match == nil {
				t.Fatalf("no closed SQL menu for %s.%s", c.Table, c.Column)
			}
			actual := []string{}
			for _, literal := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(match[1], -1) {
				actual = append(actual, literal[1])
			}
			sort.Strings(actual)
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("SQL menu=%v canonical=%v", actual, expected)
			}
		})
	}
}
