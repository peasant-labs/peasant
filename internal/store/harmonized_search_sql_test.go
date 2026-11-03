package store

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/unified_search_zero_filter.sql
var unifiedSearchZeroGolden string

//go:embed testdata/legacy_mirror_zero_filter.sql
var legacySearchZeroGolden string

//go:embed testdata/search_filters.yaml
var searchFiltersYAML []byte

//go:embed testdata/search_filters.manifest.yaml
var searchFiltersManifestYAML []byte

type searchFilterSpec struct {
	Project *string  `yaml:"project"`
	Harness *string  `yaml:"harness"`
	Since   *int64   `yaml:"since"`
	Until   *int64   `yaml:"until"`
	Tag     *string  `yaml:"tag"`
	IDs     []string `yaml:"ids"`
}

func (f searchFilterSpec) filter() SearchFilters {
	return SearchFilters{ProjectName: f.Project, Harness: f.Harness, StartFromMs: f.Since,
		StartBeforeMs: f.Until, Tag: f.Tag, SessionIDs: f.IDs}
}

type searchFilterCase struct {
	Name      string           `yaml:"name"`
	Filters   searchFilterSpec `yaml:"filters"`
	IDSetSize int              `yaml:"idSetSize"`
	Query     string           `yaml:"query"`
	Limit     int              `yaml:"limit"`
	Offset    int              `yaml:"offset"`
	Want      []string         `yaml:"want"`
}

type searchFilterFixtures struct {
	Projects []struct {
		Hash, Cwd, Remote string
	} `yaml:"projects"`
	Sessions []struct {
		ID, Project, Harness string
		Start                int64
		Tags                 []string
		Body                 bool
		Entries              []string
	} `yaml:"sessions"`
	Cases []searchFilterCase `yaml:"cases"`
}

func loadSearchFilterFixtures(t *testing.T) searchFilterFixtures {
	t.Helper()
	var family searchFilterFixtures
	if err := yaml.Unmarshal(searchFiltersYAML, &family); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Required []string `yaml:"required"`
	}
	if err := yaml.Unmarshal(searchFiltersManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for i := range family.Cases {
		c := &family.Cases[i]
		if c.Name == "" || names[c.Name] {
			t.Fatalf("empty or duplicate search filter case %q", c.Name)
		}
		names[c.Name] = true
		for len(c.Filters.IDs) < c.IDSetSize {
			c.Filters.IDs = append(c.Filters.IDs, fmt.Sprintf("99999999-9999-4999-8999-%012d", len(c.Filters.IDs)))
		}
		if c.Query == "" {
			c.Query = "cache"
		}
		if c.Limit == 0 {
			c.Limit = 50
		}
	}
	for _, name := range manifest.Required {
		if !names[name] {
			t.Fatalf("required search filter case %q is missing", name)
		}
	}
	return family
}

func TestSearchFilterSQL(t *testing.T) {
	t.Parallel()
	for _, c := range loadSearchFilterFixtures(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			f := c.Filters.filter()
			conditions, args := f.where()
			if f.IsZero() != (len(conditions) == 0 && len(args) == 0) {
				t.Fatal("zero filter and predicates disagree")
			}
			if len(f.SessionIDs) > 0 && f.ProjectName == nil && f.Harness == nil && f.Tag == nil && f.StartFromMs == nil && f.StartBeforeMs == nil {
				if len(args) != 1 {
					t.Fatalf("session set uses %d binds, want one JSON array", len(args))
				}
				var got []string
				if err := json.Unmarshal([]byte(args[0].(string)), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, f.SessionIDs) {
					t.Fatalf("session set = %v, want %v", got, f.SessionIDs)
				}
			}
			assertSearchSQL(t, unifiedSearchZeroGolden, unifiedSearchSQL, unifiedSearchExistsSQL, f)
			assertSearchSQL(t, legacySearchZeroGolden, legacySearchSQL, legacySearchExistsSQL, f)
		})
	}
}

func assertSearchSQL(t *testing.T, golden string, ranked, exists func(SearchFilters) (string, []any), f SearchFilters) {
	t.Helper()
	conditions, args := f.where()
	predicate := ""
	if len(conditions) > 0 {
		predicate = "\n  AND (" + strings.Join(conditions, " AND ") + ")"
	}
	const tail = "\nORDER BY rank, f.session_id, f.entry_index\nLIMIT ? OFFSET ?"
	want := strings.TrimSuffix(golden, tail) + predicate + tail
	got, gotArgs := ranked(f)
	if got != want {
		t.Fatalf("ranked SQL differs beyond wrapped predicate:\n%s\nwant:\n%s", got, want)
	}
	if !reflect.DeepEqual(gotArgs, args) {
		t.Fatalf("ranked args = %#v, want %#v", gotArgs, args)
	}
	from := strings.Index(want, "\nFROM ")
	wantExists := "SELECT 1" + strings.TrimSuffix(want[from:], tail) + "\nLIMIT 1"
	probe, probeArgs := exists(f)
	if probe != wantExists {
		t.Fatalf("existence SQL = %s, want %s", probe, wantExists)
	}
	if !reflect.DeepEqual(probeArgs, args) {
		t.Fatalf("existence args = %#v, want %#v", probeArgs, args)
	}
	lower := strings.ToLower(probe)
	if strings.Contains(lower, "bm25(") || strings.Contains(lower, "snippet(") || strings.Contains(lower, "order by") || strings.Contains(lower, "union") {
		t.Fatalf("existence probe ranks, shapes, orders or merges: %s", probe)
	}
}
