package ingest

import (
	_ "embed"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/opencode_session_authority.yaml
var sessionAuthorityYAML []byte

type sessionAuthorityFixture struct {
	Required []string               `yaml:"required_cases"`
	Cases    []sessionAuthorityCase `yaml:"cases"`
}

type sessionAuthorityCase struct {
	Name          string   `yaml:"name"`
	Fixture       string   `yaml:"fixture"`
	Setup         string   `yaml:"setup"`
	AfterRead     string   `yaml:"after_read"`
	Table         string   `yaml:"table"`
	RecordIDs     []string `yaml:"record_ids"`
	PresentIDs    []string `yaml:"present_ids"`
	DiscoveredIDs []string `yaml:"discovered_ids"`
	Title         string   `yaml:"title"`
	Skipped       bool     `yaml:"skipped"`
}

func loadSessionAuthorityFixtures(t *testing.T) []sessionAuthorityCase {
	t.Helper()
	var fixture sessionAuthorityFixture
	if err := yaml.Unmarshal(sessionAuthorityYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("empty or duplicate authority case %q", c.Name)
		}
		names[c.Name] = true
	}
	if len(fixture.Required) == 0 {
		t.Fatal("missing required authority case names")
	}
	for _, name := range fixture.Required {
		if !names[name] {
			t.Fatalf("missing required authority case %q", name)
		}
	}
	return fixture.Cases
}

// Only the materializer's test-owned source can be passed to setup writes.
func applySessionAuthoritySetup(t *testing.T, source testfixture.MaterializedSource, script string) {
	t.Helper()
	if script == "" {
		return
	}
	conn, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeSessionAuthority(t *testing.T) {
	for _, c := range loadSessionAuthorityFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, c.Fixture)
			applySessionAuthoritySetup(t, materialized, c.Setup)
			before := testfixture.SnapshotSource(t, materialized)
			path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
			if err != nil {
				t.Fatal(err)
			}
			source, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close(t.Context()) }()
			pageSize, err := NewOpenCodeCurrentPageSize(1)
			if err != nil {
				t.Fatal(err)
			}
			recordIDs, presentIDs := map[string]bool{}, map[string]bool{}
			var cursor *OpenCodeSessionRecordCursor
			skipped := false
			for pages := 0; ; pages++ {
				if pages > len(c.PresentIDs)+1 {
					t.Fatal("authority pagination did not terminate")
				}
				page, err := source.SessionRecords(t.Context(), OpenCodeSessionRecordPageRequest{PageSize: pageSize, After: cursor})
				if err != nil {
					t.Fatal(err)
				}
				if !page.Supported || string(page.Table) != c.Table {
					t.Fatalf("selected source = %+v, want %s", page, c.Table)
				}
				skipped = skipped || len(page.Skipped) > 0
				for _, row := range page.Records {
					recordIDs[row.SessionID.String()] = true
				}
				for _, id := range page.PresentSessionIDs {
					presentIDs[id.String()] = true
				}
				if page.Next == nil {
					break
				}
				cursor = page.Next
			}
			assertSessionAuthorityIDs(t, "records", recordIDs, c.RecordIDs)
			assertSessionAuthorityIDs(t, "presence", presentIDs, c.PresentIDs)
			if skipped != c.Skipped {
				t.Fatalf("skipped records = %t, want %t", skipped, c.Skipped)
			}
			testfixture.AssertUnchanged(t, materialized, before)

			// Discovery uses the same authority for metadata and deletion.
			root, err := NewResolvedPath(filepath.Dir(materialized.Path))
			if err != nil {
				t.Fatal(err)
			}
			adapter := NewOpenCodeAdapter(&OSFileSystem{}, nil, salt.Salt{})
			discovered, err := adapter.Discover(t.Context(), SourceConfig{Enabled: true, Paths: []ResolvedPath{root}})
			if err != nil {
				t.Fatal(err)
			}
			discoveredIDs := map[string]bool{}
			for _, session := range discovered {
				discoveredIDs[string(session.SessionID)] = true
				if session.Title != c.Title {
					t.Fatalf("title = %q, want authoritative title %q", session.Title, c.Title)
				}
			}
			assertSessionAuthorityIDs(t, "discovery", discoveredIDs, c.DiscoveredIDs)
			testfixture.AssertUnchanged(t, materialized, before)
			if c.AfterRead != "" {
				applySessionAuthoritySetup(t, materialized, c.AfterRead)
				afterSetup := testfixture.SnapshotSource(t, materialized)
				_, err := source.SessionRecords(t.Context(), OpenCodeSessionRecordPageRequest{PageSize: pageSize})
				if err == nil {
					t.Fatal("failed selected table silently fell back to empty legacy records")
				}
				testfixture.AssertUnchanged(t, materialized, afterSetup)
			}
		})
	}
}

func assertSessionAuthorityIDs(t *testing.T, kind string, actual map[string]bool, expected []string) {
	t.Helper()
	got := make([]string, 0, len(actual))
	for id := range actual {
		got = append(got, id)
	}
	want := slices.Clone(expected)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s IDs = %v, want %v", kind, got, want)
	}
}
