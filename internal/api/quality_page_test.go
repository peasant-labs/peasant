package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type qualityPageFixture struct {
	SeedSessions         int                      `yaml:"seed_sessions"`
	DefaultPageSize      int                      `yaml:"default_page_size"`
	RequiredCaseNames    []string                 `yaml:"required_case_names"`
	Cases                []qualityPageCase        `yaml:"cases"`
	RequiredInvalidNames []string                 `yaml:"required_invalid_names"`
	InvalidCases         []qualityPageInvalidCase `yaml:"invalid_cases"`
}

type qualityPageCase struct {
	Name    string `yaml:"name"`
	Query   string `yaml:"query"`
	Page    int    `yaml:"page"`
	PerPage int    `yaml:"perPage"`
	Items   int    `yaml:"items"`
	Next    int    `yaml:"next"`
	FirstID string `yaml:"firstID"`
	LastID  string `yaml:"lastID"`
}

type qualityPageInvalidCase struct {
	Name  string `yaml:"name"`
	Query string `yaml:"query"`
	Code  string `yaml:"code"`
}

func loadQualityPageFixture(t *testing.T) qualityPageFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/quality_page.yaml")
	if err != nil {
		t.Fatalf("read testdata/quality_page.yaml: %v", err)
	}
	var fixture qualityPageFixture
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode testdata/quality_page.yaml: %v", err)
	}
	if fixture.SeedSessions <= 0 || len(fixture.Cases) == 0 || len(fixture.InvalidCases) == 0 {
		t.Fatal("testdata/quality_page.yaml carries no seed count, valid cases, or invalid cases")
	}
	assertQualityCaseNames(t, "case", fixture.Cases, fixture.RequiredCaseNames)
	assertQualityCaseNames(t, "invalid case", fixture.InvalidCases, fixture.RequiredInvalidNames)
	return fixture
}

func assertQualityCaseNames[T interface{ name() string }](t *testing.T, label string, cases []T, required []string) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, item := range cases {
		seen[item.name()] = struct{}{}
	}
	if len(seen) != len(required) {
		t.Fatalf("%s fixture declares %d names but %d required", label, len(seen), len(required))
	}
	for _, name := range required {
		if _, ok := seen[name]; !ok {
			t.Fatalf("%s fixture is missing the required %s %q", label, label, name)
		}
	}
}

func (c qualityPageCase) name() string        { return c.Name }
func (c qualityPageInvalidCase) name() string { return c.Name }

func TestQualityPageContract(t *testing.T) {
	t.Parallel()
	fixture := loadQualityPageFixture(t)
	db := storetest.Open(t)
	seedHomeSessions(t, db, rootIDs(fixture.SeedSessions))
	server := qualityPageTestServer(t, db)

	if fixture.DefaultPageSize != int(qualityDefaultPageSize) {
		t.Fatalf("fixture default page size %d, server default %d", fixture.DefaultPageSize, qualityDefaultPageSize)
	}

	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			page := qualityPageRequest(t, server, testCase.Query, http.StatusOK)
			if page.Page != testCase.Page || page.PerPage != schema.PageSize(testCase.PerPage) {
				t.Fatalf("page = %d/%d, want %d/%d", page.Page, page.PerPage, testCase.Page, testCase.PerPage)
			}
			if page.Total != fixture.SeedSessions {
				t.Fatalf("total = %d, want the %d stored sessions", page.Total, fixture.SeedSessions)
			}
			if len(page.Items) != testCase.Items {
				t.Fatalf("items = %d, want %d", len(page.Items), testCase.Items)
			}
			if len(page.NextItems) != testCase.Next {
				t.Fatalf("nextItems = %d, want %d", len(page.NextItems), testCase.Next)
			}
			if got := page.Items[0].ID; got != testCase.FirstID {
				t.Fatalf("first item = %q, want %q", got, testCase.FirstID)
			}
			if got := page.Items[len(page.Items)-1].ID; got != testCase.LastID {
				t.Fatalf("last item = %q, want %q", got, testCase.LastID)
			}
			if page.Revision == "" {
				t.Fatal("page carries no revision")
			}
			assertQualitySummary(t, page.Summary, fixture.SeedSessions, 1)
			if err := page.Validate(); err != nil {
				t.Fatalf("page failed the schema contract: %v", err)
			}
		})
	}

	for _, testCase := range fixture.InvalidCases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.handleQualityPage(response, httptest.NewRequest(http.MethodGet, defaults.RouteQuality.String()+"?"+testCase.Query, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body.String())
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			if body.Code != testCase.Code {
				t.Fatalf("refusal code = %q, want %q", body.Code, testCase.Code)
			}
		})
	}
}

func TestQualityPageEmptyStoreReturnsPageOne(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	server := qualityPageTestServer(t, db)
	page := qualityPageRequest(t, server, "page=7&maxItems=25", http.StatusOK)
	if page.Page != 1 || page.PerPage != schema.PageSize25 || page.Total != 0 {
		t.Fatalf("empty quality page = page %d perPage %d total %d, want page 1 perPage 25 total 0", page.Page, page.PerPage, page.Total)
	}
	if len(page.Items) != 0 || len(page.NextItems) != 0 {
		t.Fatalf("empty quality page = items %d nextItems %d, want both empty", len(page.Items), len(page.NextItems))
	}
	if page.Revision == "" {
		t.Fatal("empty quality page carries no revision")
	}
	assertQualitySummary(t, page.Summary, 0, 0)
}

func TestQualityPageSummaryInvalidatesOnRevision(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	seedHomeSessions(t, db, rootIDs(1))
	server := qualityPageTestServer(t, db)

	first := qualityPageRequest(t, server, "page=1&maxItems=25", http.StatusOK)
	if first.Summary.TotalSessions != 1 || first.Summary.SessionStats.TurnCount.Count != 1 {
		t.Fatalf("first summary = %+v, want one stored session", first.Summary)
	}

	seedHomeSessions(t, db, []string{"sess_root001"})

	second := qualityPageRequest(t, server, "page=1&maxItems=25", http.StatusOK)
	if second.Revision == first.Revision {
		t.Fatalf("revision did not advance after a session write: %q", second.Revision)
	}
	if second.Summary.TotalSessions != 2 || second.Summary.SessionStats.TurnCount.Count != 2 {
		t.Fatalf("second summary = %+v, want two stored sessions", second.Summary)
	}
	if second.Total != 2 || len(second.Items) != 2 {
		t.Fatalf("second page = total %d items %d, want 2 and 2", second.Total, len(second.Items))
	}
}

func TestQualityPageRouteIsMounted(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	seedHomeSessions(t, db, rootIDs(2))
	base := startDiscoveryHTTPServer(t, ServerConfig{
		Store:      db,
		Config:     config.BaseConfig(),
		Provider:   NewStoreDataProvider(db, sessionvisibility.Browsing()),
		ConfigHome: t.TempDir(),
	})
	response, err := http.Get(base + defaults.RouteQuality.String() + "?page=1&maxItems=25")
	if err != nil {
		t.Fatalf("GET %s: %v", defaults.RouteQuality, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("mounted quality route status = %d, want 200", response.StatusCode)
	}
	var page schema.QualityPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("decode mounted quality page: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 || page.Summary.TotalSessions != 2 {
		t.Fatalf("mounted quality page = total %d items %d summary %d, want 2/2/2", page.Total, len(page.Items), page.Summary.TotalSessions)
	}
	if err := page.Validate(); err != nil {
		t.Fatalf("mounted quality page failed the schema contract: %v", err)
	}
}

func qualityPageTestServer(t *testing.T, db *store.Store) *Server {
	t.Helper()
	return NewServer(ServerConfig{
		Store:      db,
		Config:     config.BaseConfig(),
		Provider:   NewStoreDataProvider(db, sessionvisibility.Browsing()),
		ConfigHome: t.TempDir(),
	})
}

func qualityPageRequest(t *testing.T, server *Server, query string, status int) *schema.QualityPage {
	t.Helper()
	response := httptest.NewRecorder()
	server.handleQualityPage(response, httptest.NewRequest(http.MethodGet, defaults.RouteQuality.String()+"?"+query, nil))
	if response.Code != status {
		t.Fatalf("GET %s status = %d, want %d; body=%s", query, response.Code, status, response.Body.String())
	}
	if status != http.StatusOK {
		return nil
	}
	var page schema.QualityPage
	decoder := json.NewDecoder(strings.NewReader(response.Body.String()))
	if err := decoder.Decode(&page); err != nil {
		t.Fatalf("decode quality page: %v", err)
	}
	return &page
}

// assertQualitySummary checks the library-wide summary the fixture's stored
// sessions must produce. The fixture seeds one project and one contributor.
func assertQualitySummary(t *testing.T, summary schema.ProjectAnalytics, sessions, projects int) {
	t.Helper()
	if summary.TotalSessions != sessions {
		t.Fatalf("summary.totalSessions = %d, want %d", summary.TotalSessions, sessions)
	}
	if summary.TotalProjects != projects {
		t.Fatalf("summary.totalProjects = %d, want %d", summary.TotalProjects, projects)
	}
	if sessions == 0 {
		if summary.TotalContributors != 0 {
			t.Fatalf("empty summary.totalContributors = %d, want 0", summary.TotalContributors)
		}
	} else if summary.TotalContributors != 1 {
		t.Fatalf("summary.totalContributors = %d, want the one local contributor", summary.TotalContributors)
	}
	if summary.OutcomeDistribution.Total != sessions {
		t.Fatalf("summary outcome total = %d, want %d", summary.OutcomeDistribution.Total, sessions)
	}
	if summary.SessionStats.TurnCount.Count != sessions {
		t.Fatalf("summary turn-count sample = %d, want %d", summary.SessionStats.TurnCount.Count, sessions)
	}
	if summary.SessionStats.TurnCount.Count == 0 && (summary.SessionStats.TurnCount.Median != nil || summary.SessionStats.TurnCount.P90 != nil) {
		t.Fatal("an empty summary fabricated a median")
	}
}
