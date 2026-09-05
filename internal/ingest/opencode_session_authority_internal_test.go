package ingest

import (
	"context"
	_ "embed"
	"errors"
	"path/filepath"
	"slices"
	"strings"
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
	RequiredColumns    []string `yaml:"required_columns"`
	CatalogColumns     int      `yaml:"catalog_columns"`
	CatalogOverflow    bool     `yaml:"catalog_overflow"`
	V2Layout           string   `yaml:"v2_layout"`
	DiagnosticContains string   `yaml:"diagnostic_contains"`
	FailRead           bool     `yaml:"fail_read"`
	Name               string   `yaml:"name"`
	Fixture            string   `yaml:"fixture"`
	Setup              string   `yaml:"setup"`
	AfterRead          string   `yaml:"after_read"`
	Table              string   `yaml:"table"`
	RecordIDs          []string `yaml:"record_ids"`
	PresentIDs         []string `yaml:"present_ids"`
	DiscoveredIDs      []string `yaml:"discovered_ids"`
	Title              string   `yaml:"title"`
	Skipped            bool     `yaml:"skipped"`
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
			if c.CatalogOverflow {
				_, err := source.Catalog(t.Context())
				var overflow *OpenCodeCatalogOverflowError
				if !errors.As(err, &overflow) || overflow.Scope != OpenCodeCatalogColumns || overflow.Table != "session_v2" || overflow.Limit != 64 {
					t.Fatalf("metadata column overflow = %v, want session_v2 retained limit 64", err)
				}
				testfixture.AssertUnchanged(t, materialized, before)
				return
			}
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
			filesystem := &OSFileSystem{}
			opener := func(ctx context.Context, path OpenCodeSQLiteSourcePath, options OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error) {
				opened, openErr := OpenOpenCodeSQLiteSource(ctx, path, options)
				if openErr != nil || !c.FailRead {
					return opened, openErr
				}
				return metadataReadFailingSource{OpenCodeSQLiteSource: opened}, nil
			}
			adapter, err := NewOpenCodeAdapterWithCandidateProbe(filesystem, nil, salt.Salt{}, "latest", canonicalTieEnvironment{}, filesystem, opener, DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatal(err)
			}
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
			assertSessionAuthorityEvidence(t, adapter, materialized.Path, c)
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

type metadataReadFailingSource struct{ OpenCodeSQLiteSource }

func (s metadataReadFailingSource) SessionRecords(ctx context.Context, request OpenCodeSessionRecordPageRequest) (OpenCodeSessionRecordPage, error) {
	page, err := s.OpenCodeSQLiteSource.SessionRecords(ctx, request)
	if err != nil {
		return page, err
	}
	return OpenCodeSessionRecordPage{Table: page.Table}, errors.New("synthetic authoritative metadata read failed")
}

func assertSessionAuthorityEvidence(t *testing.T, adapter *OpenCodeAdapter, path string, c sessionAuthorityCase) {
	t.Helper()
	for _, result := range adapter.CandidateEvidence() {
		if result.Candidate.Path != path {
			continue
		}
		if string(result.SessionTable) != c.Table || string(result.V2Layout) != c.V2Layout {
			t.Fatalf("selected table/layout = %q/%q, want %q/%q", result.SessionTable, result.V2Layout, c.Table, c.V2Layout)
		}
		if c.V2Layout != "absent" && len(result.Evidence.SessionV2Columns) == 0 {
			t.Fatal("v2 catalog evidence missing")
		}
		if c.CatalogColumns != 0 && len(result.Evidence.SessionV2Columns) != c.CatalogColumns {
			t.Fatalf("metadata catalog retained %d columns, want boundary %d", len(result.Evidence.SessionV2Columns), c.CatalogColumns)
		}
		if len(c.RequiredColumns) > 0 {
			columns := map[string]bool{}
			for _, column := range result.Evidence.SessionV2Columns {
				columns[column.Name] = true
			}
			assertSessionAuthorityIDs(t, "native metadata columns", columns, c.RequiredColumns)
		}
		if c.Table == "session" && len(result.Evidence.SessionColumns) == 0 {
			t.Fatal("legacy catalog evidence missing")
		}
		found := c.DiagnosticContains == ""
		for _, diagnostic := range result.Diagnostics {
			if strings.Contains(diagnostic.What, c.DiagnosticContains) {
				found = true
			}
			if diagnostic.Meaning == "" || diagnostic.Remediation == "" {
				t.Fatalf("diagnostic lacks actionable outcome: %+v", diagnostic)
			}
			if strings.Contains(diagnostic.Meaning, "sessions from this candidate were skipped") {
				t.Fatalf("metadata diagnostic misrepresents retention: %+v", diagnostic)
			}
			if strings.Contains(diagnostic.What+diagnostic.Why, "native user question") {
				t.Fatal("diagnostic leaked transcript payload")
			}
		}
		if !found {
			t.Fatalf("missing diagnostic containing %q: %+v", c.DiagnosticContains, result.Diagnostics)
		}
		return
	}
	t.Fatal("source catalog evidence missing")
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
