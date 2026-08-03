package api_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/project_resolution.yaml
var projectResolutionFixtureYAML []byte

//go:embed testdata/project_summaries_errors.yaml
var projectSummariesErrorFixtureYAML []byte

//go:embed testdata/map_error_responses.yaml
var mapErrorResponsesYAML []byte

const projectSummariesErrorCaseCount = 2

type mapErrorResponseFixture struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	RequiredNames     []string `yaml:"requiredNames"`
	Cases             []struct {
		Name               string   `yaml:"name"`
		ErrorKind          string   `yaml:"errorKind"`
		Endpoint           string   `yaml:"endpoint"`
		Status             int      `yaml:"status"`
		Code               string   `yaml:"code"`
		RequiredFragments  []string `yaml:"requiredFragments"`
		ForbiddenFragments []string `yaml:"forbiddenFragments"`
	} `yaml:"cases"`
}

func decodeMapErrorResponses(source []byte) (mapErrorResponseFixture, error) {
	var fixture mapErrorResponseFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode map error response fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fixture, fmt.Errorf("map error response fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.ExpectedCaseCount != 13 || len(fixture.RequiredNames) != fixture.ExpectedCaseCount || len(fixture.Cases) != fixture.ExpectedCaseCount {
		return fixture, fmt.Errorf("map error response fixture count mismatch: declared=%d names=%d cases=%d, want 13", fixture.ExpectedCaseCount, len(fixture.RequiredNames), len(fixture.Cases))
	}
	required := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if name == "" {
			return fixture, fmt.Errorf("map error response fixture has an empty required name")
		}
		if _, duplicate := required[name]; duplicate {
			return fixture, fmt.Errorf("map error response fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if _, duplicate := seen[testCase.Name]; duplicate {
			return fixture, fmt.Errorf("map error response fixture repeats case name %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		if _, ok := required[testCase.Name]; !ok {
			return fixture, fmt.Errorf("map error response fixture has unknown case %q", testCase.Name)
		}
		if testCase.ErrorKind == "" || testCase.Endpoint == "" || testCase.Status == 0 || testCase.Code == "" || len(testCase.RequiredFragments) == 0 || len(testCase.ForbiddenFragments) == 0 {
			return fixture, fmt.Errorf("map error response fixture case %q is incomplete", testCase.Name)
		}
		for _, fragment := range append(append([]string{}, testCase.RequiredFragments...), testCase.ForbiddenFragments...) {
			if fragment == "" {
				return fixture, fmt.Errorf("map error response fixture case %q has an empty message fragment", testCase.Name)
			}
		}
	}
	for name := range required {
		if _, ok := seen[name]; !ok {
			return fixture, fmt.Errorf("map error response fixture is missing required case %q", name)
		}
	}
	return fixture, nil
}

var mapHandlerProjectHash = schema.ProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
var mapHandlerHiddenHash = schema.ProjectHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

var requiredProjectSummariesErrorCaseNames = map[string]struct{}{
	"persisted selection failure is typed":     {},
	"unrelated provider failure stays untyped": {},
}

type projectSummariesErrorFixture struct {
	ExpectedCaseCount int `yaml:"expectedCaseCount"`
	Cases             []struct {
		Name      string  `yaml:"name"`
		ErrorKind string  `yaml:"errorKind"`
		WantCode  *string `yaml:"wantCode"`
	} `yaml:"cases"`
}

func decodeProjectSummariesErrorFixture(source []byte) (projectSummariesErrorFixture, error) {
	var fixture projectSummariesErrorFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode project summaries error fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fixture, fmt.Errorf("project summaries error fixture contains more than one YAML document")
		}
		return fixture, fmt.Errorf("decode trailing project summaries error fixture content: %w", err)
	}
	if fixture.ExpectedCaseCount != projectSummariesErrorCaseCount {
		return fixture, fmt.Errorf("project summaries error fixture expectedCaseCount = %d, want independently defined %d", fixture.ExpectedCaseCount, projectSummariesErrorCaseCount)
	}
	if len(fixture.Cases) != projectSummariesErrorCaseCount {
		return fixture, fmt.Errorf("project summaries error fixture has %d cases, want %d", len(fixture.Cases), projectSummariesErrorCaseCount)
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		if _, required := requiredProjectSummariesErrorCaseNames[testCase.Name]; !required {
			return fixture, fmt.Errorf("project summaries error fixture cases[%d] has unknown or missing semantic name %q", index, testCase.Name)
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return fixture, fmt.Errorf("project summaries error fixture duplicates semantic name %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		if testCase.WantCode == nil {
			return fixture, fmt.Errorf("project summaries error fixture cases[%d] %q is missing required wantCode", index, testCase.Name)
		}
		if testCase.ErrorKind != "selection" && testCase.ErrorKind != "generic" {
			return fixture, fmt.Errorf("project summaries error fixture cases[%d] %q has unsupported errorKind %q", index, testCase.Name, testCase.ErrorKind)
		}
	}
	if len(seen) != len(requiredProjectSummariesErrorCaseNames) {
		return fixture, fmt.Errorf("project summaries error fixture does not cover every required semantic case")
	}
	return fixture, nil
}

func loadProjectSummariesErrorFixture(t *testing.T) projectSummariesErrorFixture {
	t.Helper()
	fixture, err := decodeProjectSummariesErrorFixture(projectSummariesErrorFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

type projectResolutionFixture struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	RequiredNames     []string `yaml:"requiredNames"`
	Cases             []struct {
		Name            string `yaml:"name"`
		Project         string `yaml:"project"`
		ProviderError   string `yaml:"providerError"`
		ExpectedStatus  int    `yaml:"expectedStatus"`
		ExpectedProject string `yaml:"expectedProject"`
		ExpectedHash    string `yaml:"expectedHash"`
		ExpectedError   string `yaml:"expectedError"`
	} `yaml:"cases"`
}

var requiredProjectResolutionNames = map[string]struct{}{
	"explicit hidden project resolves without enumerating discovery rows": {},
	"unknown explicit project returns not found":                          {},
	"missing exact project name is actionable":                            {},
}

func decodeProjectResolutionFixture(source []byte) (projectResolutionFixture, error) {
	var fixture projectResolutionFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode project resolution fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fixture, fmt.Errorf("project resolution fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.ExpectedCaseCount != len(requiredProjectResolutionNames) || len(fixture.RequiredNames) != fixture.ExpectedCaseCount || len(fixture.Cases) != fixture.ExpectedCaseCount {
		return fixture, fmt.Errorf("project resolution fixture cardinality mismatch")
	}
	required := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if name == "" {
			return fixture, fmt.Errorf("project resolution fixture has an empty required name")
		}
		if _, ok := requiredProjectResolutionNames[name]; !ok {
			return fixture, fmt.Errorf("project resolution fixture has unknown required name %q", name)
		}
		if _, duplicate := required[name]; duplicate {
			return fixture, fmt.Errorf("project resolution fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" {
			return fixture, fmt.Errorf("project resolution fixture has an empty case name")
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return fixture, fmt.Errorf("project resolution fixture repeats case %q", testCase.Name)
		}
		if _, ok := required[testCase.Name]; !ok {
			return fixture, fmt.Errorf("project resolution fixture has unknown case %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
	}
	return fixture, nil
}

func loadProjectResolutionFixture(t *testing.T) projectResolutionFixture {
	t.Helper()
	fixture, err := decodeProjectResolutionFixture(projectResolutionFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// mapStubProvider embeds the no-op batchTestProvider and overrides the
// Map/Review/summary methods with canned payloads, recorded arguments, and
// injectable errors. The system under test is the Server's handlers; the
// provider remains a dependency mock.
type mapStubProvider struct {
	batchTestProvider

	// recorded arguments of the last call
	gotProjectHash schema.ProjectHash
	gotCommit      string
	gotPath        string
	gotFile        string
	gotBranch      string
	gotQuery       string
	gotLimit       int

	// recorded calls without arguments
	summariesCalled bool

	// optional canned FrictionCluster set attached to ChangeDetail payloads; nil
	// leaves the never-nil empty slice (default). Used by the route-level
	// FrictionCluster-shape verification.
	frictions []schema.FrictionCluster

	// injectable errors (per method)
	graphErr      error
	nodeErr       error
	tasksErr      error
	reviewErr     error
	changeErr     error
	diffErr       error
	summariesErr  error
	resolutionErr error
	searchErr     error
}

func (p *mapStubProvider) ResolveProject(_ context.Context, project string) (*schema.ProjectResolutionPayload, error) {
	p.gotQuery = project
	if p.resolutionErr != nil {
		return nil, p.resolutionErr
	}
	return &schema.ProjectResolutionPayload{Project: project, ProjectHash: mapHandlerHiddenHash}, nil
}

func (p *mapStubProvider) ProjectSummaries(_ context.Context) (*codemap.ProjectSummariesResult, error) {
	p.summariesCalled = true
	if p.summariesErr != nil {
		return nil, p.summariesErr
	}
	result := &codemap.ProjectSummariesResult{Projects: []schema.ProjectSummary{}}
	result.Projects = append(result.Projects, schema.ProjectSummary{
		ProjectHash: mapHandlerProjectHash,
		Project:     "/repo/fortuna",
		Sessions:    3,
		OpenChanges: 1,
	})
	return result, nil
}

func (p *mapStubProvider) MapGraph(_ context.Context, projectHash schema.ProjectHash, commit string) (*schema.MapGraphPayload, error) {
	p.gotProjectHash, p.gotCommit = projectHash, commit
	if p.graphErr != nil {
		return nil, p.graphErr
	}
	payload := schema.NewMapGraphPayload(projectHash)
	payload.RepoFound = true
	payload.AtCommit = commit
	return payload, nil
}

func (p *mapStubProvider) MapNodeDetail(_ context.Context, projectHash schema.ProjectHash, path string) (*schema.MapNodeDetailPayload, error) {
	p.gotProjectHash, p.gotPath = projectHash, path
	if p.nodeErr != nil {
		return nil, p.nodeErr
	}
	return schema.NewMapNodeDetailPayload(path), nil
}

func (p *mapStubProvider) ProjectTasks(_ context.Context, projectHash schema.ProjectHash, file string) (*schema.ProjectTasksPayload, error) {
	p.gotProjectHash, p.gotFile = projectHash, file
	if p.tasksErr != nil {
		return nil, p.tasksErr
	}
	payload := schema.NewProjectTasksPayload(projectHash)
	payload.FileFilter = file
	return payload, nil
}

func (p *mapStubProvider) ReviewChanges(_ context.Context, projectHash schema.ProjectHash) (*schema.ReviewListPayload, error) {
	p.gotProjectHash = projectHash
	if p.reviewErr != nil {
		return nil, p.reviewErr
	}
	return schema.NewReviewListPayload(projectHash), nil
}

func (p *mapStubProvider) ChangeDetail(_ context.Context, projectHash schema.ProjectHash, branch string) (*schema.ChangeDetailPayload, error) {
	p.gotProjectHash, p.gotBranch = projectHash, branch
	if p.changeErr != nil {
		return nil, p.changeErr
	}
	payload := schema.NewChangeDetailPayload(branch)
	if p.frictions != nil {
		payload.Frictions = p.frictions
	}
	return payload, nil
}

func (p *mapStubProvider) ChangeDiff(_ context.Context, projectHash schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error) {
	p.gotProjectHash, p.gotBranch, p.gotFile = projectHash, branch, file
	if p.diffErr != nil {
		return nil, p.diffErr
	}
	return schema.NewChangeDiffPayload(branch, file), nil
}

func (p *mapStubProvider) Search(_ context.Context, query string, limit int) (*schema.SearchPayload, error) {
	p.gotQuery, p.gotLimit = query, limit
	if p.searchErr != nil {
		return nil, p.searchErr
	}
	payload := schema.NewSearchPayload(query)
	payload.Results = append(payload.Results, schema.SearchResult{
		SessionID:  "sess-1",
		Project:    "/repo/fortuna",
		EntryIndex: 4,
		Role:       string(schema.RoleUser),
		Snippet:    "matched [" + query + "] here",
		Score:      1.5,
	})
	return payload, nil
}

var _ api.DataProvider = (*mapStubProvider)(nil)

// startMapTestServer boots a real Server on a dynamic port with the given
// provider and returns its base URL. Shutdown via t.Cleanup.
func startMapTestServer(t *testing.T, provider api.DataProvider) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := api.NewServer(api.ServerConfig{Port: 0, Provider: provider})
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	return "http://" + srv.Addr().String()
}

// getJSON GETs the URL and returns status code, Content-Type header, and raw
// body.
func getJSON(t *testing.T, url string) (int, string, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get(defaults.HeaderContentType), body
}

func TestProjectResolutionHandler_Fixture(t *testing.T) {
	fixture := loadProjectResolutionFixture(t)
	stub := &mapStubProvider{}
	base := startMapTestServer(t, stub)

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			stub.gotQuery = ""
			stub.summariesCalled = false
			stub.resolutionErr = nil
			if testCase.ProviderError == "not_found" {
				stub.resolutionErr = codemap.ErrProjectNotFound
			}
			endpoint := base + "/api/v1/projects/resolve"
			if testCase.Project != "" {
				endpoint += "?name=" + url.QueryEscape(testCase.Project)
			}
			status, _, body := getJSON(t, endpoint)
			if status != testCase.ExpectedStatus {
				t.Fatalf("status = %d, want %d; body=%s", status, testCase.ExpectedStatus, body)
			}
			if testCase.ExpectedError != "" {
				var payload map[string]string
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("decode error payload: %v", err)
				}
				if !strings.Contains(payload["error"], testCase.ExpectedError) {
					t.Fatalf("error = %q, want substring %q", payload["error"], testCase.ExpectedError)
				}
				return
			}
			var payload schema.ProjectResolutionPayload
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode resolution payload: %v", err)
			}
			if payload.Project != testCase.ExpectedProject || payload.ProjectHash.String() != testCase.ExpectedHash {
				t.Fatalf("payload = %+v, want project=%q hash=%q", payload, testCase.ExpectedProject, testCase.ExpectedHash)
			}
			if stub.gotQuery != testCase.Project {
				t.Fatalf("provider project = %q, want %q", stub.gotQuery, testCase.Project)
			}
			if stub.summariesCalled {
				t.Fatal("direct resolution enumerated selection-filtered project summaries")
			}
		})
	}
}

func TestProjectResolutionFixtureRejectsStructuralAndSemanticMutations(t *testing.T) {
	unknown := bytes.Replace(projectResolutionFixtureYAML, []byte("expectedCaseCount:"), []byte("unknown: true\nexpectedCaseCount:"), 1)
	if _, err := decodeProjectResolutionFixture(unknown); err == nil {
		t.Fatal("unknown-field mutation unexpectedly validated")
	}
	trailing := append(append([]byte{}, projectResolutionFixtureYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeProjectResolutionFixture(trailing); err == nil {
		t.Fatal("trailing-document mutation unexpectedly validated")
	}
	duplicate := bytes.Replace(projectResolutionFixtureYAML, []byte("  - unknown explicit project returns not found\n"), []byte("  - explicit hidden project resolves without enumerating discovery rows\n"), 1)
	if _, err := decodeProjectResolutionFixture(duplicate); err == nil {
		t.Fatal("duplicate-name mutation unexpectedly validated")
	}
	renamed := bytes.Replace(projectResolutionFixtureYAML, []byte("name: unknown explicit project returns not found"), []byte("name: renamed resolution behavior"), 1)
	if _, err := decodeProjectResolutionFixture(renamed); err == nil {
		t.Fatal("count-preserving semantic rename unexpectedly validated")
	}
	deleted := bytes.Replace(projectResolutionFixtureYAML, []byte("  - name: missing exact project name is actionable\n    expectedStatus: 400\n    expectedError: required query field \"name\" is missing or empty\n"), nil, 1)
	if _, err := decodeProjectResolutionFixture(deleted); err == nil {
		t.Fatal("semantic row deletion unexpectedly validated")
	}
}

func TestMapHandlers_HappyPaths(t *testing.T) {
	stub := &mapStubProvider{}
	base := startMapTestServer(t, stub)

	cases := []struct {
		name      string
		url       string
		checkStub func(t *testing.T)
		checkBody func(t *testing.T, body map[string]any)
	}{
		{
			name: "map graph with commit",
			url:  base + "/api/v1/map/" + mapHandlerProjectHash.String() + "?commit=deadbeef",
			checkStub: func(t *testing.T) {
				if stub.gotProjectHash != mapHandlerProjectHash || stub.gotCommit != "deadbeef" {
					t.Errorf("provider got (%q, %q), want (%q, deadbeef)", stub.gotProjectHash, stub.gotCommit, mapHandlerProjectHash)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				if body["projectHash"] != mapHandlerProjectHash.String() {
					t.Errorf("projectHash = %v, want %s", body["projectHash"], mapHandlerProjectHash)
				}
				// Arrays are never null.
				for _, key := range []string{"nodes", "structureEdges", "activityEdges", "violations", "parsedLanguages"} {
					arr, ok := body[key].([]any)
					if !ok || arr == nil {
						t.Errorf("%s = %v (%T), want JSON array", key, body[key], body[key])
					}
				}
			},
		},
		{
			name: "node detail",
			url:  base + "/api/v1/map/" + mapHandlerProjectHash.String() + "/node?path=internal/api",
			checkStub: func(t *testing.T) {
				if stub.gotPath != "internal/api" {
					t.Errorf("provider got path %q, want internal/api", stub.gotPath)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				if body["path"] != "internal/api" {
					t.Errorf("path = %v, want internal/api", body["path"])
				}
			},
		},
		{
			name: "tasks with file filter",
			url:  base + "/api/v1/map/" + mapHandlerProjectHash.String() + "/tasks?file=internal/api/server.go",
			checkStub: func(t *testing.T) {
				if stub.gotFile != "internal/api/server.go" {
					t.Errorf("provider got file %q, want internal/api/server.go", stub.gotFile)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				if body["fileFilter"] != "internal/api/server.go" {
					t.Errorf("fileFilter = %v", body["fileFilter"])
				}
			},
		},
		{
			name: "tasks without file filter",
			url:  base + "/api/v1/map/" + mapHandlerProjectHash.String() + "/tasks",
			checkStub: func(t *testing.T) {
				if stub.gotFile != "" {
					t.Errorf("provider got file %q, want empty", stub.gotFile)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				if arr, ok := body["tasks"].([]any); !ok || arr == nil {
					t.Errorf("tasks = %v, want JSON array", body["tasks"])
				}
			},
		},
		{
			name: "project summaries",
			url:  base + "/api/v1/projects/summary",
			checkStub: func(t *testing.T) {
				if !stub.summariesCalled {
					t.Error("provider ProjectSummaries was not called")
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				rows, ok := body["projects"].([]any)
				if !ok || rows == nil {
					t.Fatalf("projects = %v (%T), want JSON array", body["projects"], body["projects"])
				}
				if len(rows) != 1 {
					t.Fatalf("projects rows = %d, want 1", len(rows))
				}
				row, ok := rows[0].(map[string]any)
				if !ok {
					t.Fatalf("projects[0] = %v (%T), want object", rows[0], rows[0])
				}
				if row["projectHash"] != mapHandlerProjectHash.String() || row["project"] != "/repo/fortuna" {
					t.Errorf("projects[0] = %v, want the stub row", row)
				}
			},
		},
		{
			name: "review list",
			url:  base + "/api/v1/review/" + mapHandlerProjectHash.String(),
			checkStub: func(t *testing.T) {
				if stub.gotProjectHash != mapHandlerProjectHash {
					t.Errorf("provider got hash %q, want %q", stub.gotProjectHash, mapHandlerProjectHash)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				for _, key := range []string{"changes", "recentCommits"} {
					if arr, ok := body[key].([]any); !ok || arr == nil {
						t.Errorf("%s = %v, want JSON array", key, body[key])
					}
				}
			},
		},
		{
			name: "change detail with slashed branch",
			url:  base + "/api/v1/review/" + mapHandlerProjectHash.String() + "/change?branch=" + "feat%2Fqueue-retry",
			checkStub: func(t *testing.T) {
				if stub.gotBranch != "feat/queue-retry" {
					t.Errorf("provider got branch %q, want feat/queue-retry", stub.gotBranch)
				}
			},
			checkBody: func(t *testing.T, body map[string]any) {
				if body["branch"] != "feat/queue-retry" {
					t.Errorf("branch = %v, want feat/queue-retry", body["branch"])
				}
				// Never-null arrays through the HTTP boundary (incl. 5.1 frictions).
				for _, key := range []string{"files", "work", "unusual", "frictions"} {
					if arr, ok := body[key].([]any); !ok || arr == nil {
						t.Errorf("%s = %v (%T), want JSON array", key, body[key], body[key])
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, contentType, raw := getJSON(t, tc.url)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", status, raw)
			}
			if contentType != defaults.ContentJSON.String() {
				t.Errorf("Content-Type = %q, want %q", contentType, defaults.ContentJSON)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("unmarshal: %v (body: %s)", err, raw)
			}
			tc.checkStub(t)
			tc.checkBody(t, body)
		})
	}
}

func TestMapHandlers_ErrorMapping(t *testing.T) {
	stub := &mapStubProvider{}
	base := startMapTestServer(t, stub)

	// Errors arrive wrapped (the store adapter wraps codemap sentinels).
	wrap := func(err error) error { return fmt.Errorf("store adapter: %w", err) }

	cases := []struct {
		name       string
		setup      func()
		url        string
		wantStatus int
	}{
		{
			name:       "unknown project on map graph is 404",
			setup:      func() { stub.graphErr = wrap(codemap.ErrProjectNotFound) },
			url:        base + "/api/v1/map/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown node is 404",
			setup:      func() { stub.nodeErr = wrap(codemap.ErrNodeNotFound) },
			url:        base + "/api/v1/map/" + mapHandlerProjectHash.String() + "/node?path=nope",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing node path is 400",
			setup:      func() {},
			url:        base + "/api/v1/map/" + mapHandlerProjectHash.String() + "/node",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown project on review is 404",
			setup:      func() { stub.reviewErr = wrap(codemap.ErrProjectNotFound) },
			url:        base + "/api/v1/review/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing branch is 400",
			setup:      func() {},
			url:        base + "/api/v1/review/" + mapHandlerProjectHash.String() + "/change",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "repo-less project on change detail is 404",
			setup:      func() { stub.changeErr = wrap(codemap.ErrRepoNotFound) },
			url:        base + "/api/v1/review/" + mapHandlerProjectHash.String() + "/change?branch=feat%2Fx",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown branch on change detail is 404",
			setup:      func() { stub.changeErr = wrap(codemap.ErrBranchNotFound) },
			url:        base + "/api/v1/review/" + mapHandlerProjectHash.String() + "/change?branch=nope",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing branch on diff is 400",
			setup:      func() {},
			url:        base + "/api/v1/review/" + mapHandlerProjectHash.String() + "/diff?file=a.go",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing file on diff is 400",
			setup:      func() {},
			url:        base + "/api/v1/review/" + mapHandlerProjectHash.String() + "/diff?branch=feat%2Fx",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown branch on diff is 404",
			setup:      func() { stub.diffErr = wrap(codemap.ErrBranchNotFound) },
			url:        base + "/api/v1/review/" + mapHandlerProjectHash.String() + "/diff?branch=nope&file=a.go",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "generic provider failure is 500",
			setup:      func() { stub.graphErr = fmt.Errorf("boom") },
			url:        base + "/api/v1/map/" + mapHandlerProjectHash.String(),
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "project summaries failure is 500",
			setup:      func() { stub.summariesErr = fmt.Errorf("boom") },
			url:        base + "/api/v1/projects/summary",
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "empty search query is 400",
			setup:      func() {},
			url:        base + "/api/v1/search",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "short search query is 400",
			setup:      func() {},
			url:        base + "/api/v1/search?q=a",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "generic search failure is 500",
			setup:      func() { stub.searchErr = fmt.Errorf("boom") },
			url:        base + "/api/v1/search?q=pipeline",
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub.graphErr, stub.nodeErr, stub.tasksErr, stub.reviewErr, stub.changeErr, stub.diffErr, stub.summariesErr, stub.searchErr = nil, nil, nil, nil, nil, nil, nil, nil
			tc.setup()

			status, contentType, raw := getJSON(t, tc.url)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", status, tc.wantStatus, raw)
			}
			// Error bodies are JSON and must say so (http.Error would stamp
			// text/plain).
			if contentType != defaults.ContentJSON.String() {
				t.Errorf("Content-Type = %q, want %q", contentType, defaults.ContentJSON)
			}
			if !strings.Contains(string(raw), `"error"`) {
				t.Errorf("body %q does not carry an inline error JSON", raw)
			}
		})
	}
}

func TestMapErrorResponses_AreStableActionableAndSanitized(t *testing.T) {
	fixture, err := decodeMapErrorResponses(mapErrorResponsesYAML)
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(mapErrorResponsesYAML, []byte("expectedCaseCount:"), []byte("unexpected: true\nexpectedCaseCount:"), 1)
	if _, err := decodeMapErrorResponses(unknown); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}
	trailing := append(append([]byte{}, mapErrorResponsesYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeMapErrorResponses(trailing); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}
	renamed := bytes.Replace(mapErrorResponsesYAML, []byte("name: missing project is stable"), []byte("name: renamed project behavior"), 1)
	if _, err := decodeMapErrorResponses(renamed); err == nil {
		t.Fatal("count-preserving semantic rename unexpectedly validated")
	}
	deleted := bytes.Replace(mapErrorResponsesYAML, []byte("  - name: provider details are sanitized\n"), []byte("  - name: removed provider behavior\n"), 1)
	if _, err := decodeMapErrorResponses(deleted); err == nil {
		t.Fatal("semantic row deletion unexpectedly validated")
	}

	_, selectionErr := sessionvisibility.New(config.SelectionConfig{Mode: "not-a-selection-mode"})
	if selectionErr == nil {
		t.Fatal("invalid selection config unexpectedly succeeded")
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			if strings.HasPrefix(testCase.ErrorKind, "guard-") {
				return // exercised directly by map_request_guard_internal_test.go
			}
			wrapped := func(cause error) error { return fmt.Errorf("store-secret: %w", cause) }
			stub := &mapStubProvider{}
			switch testCase.ErrorKind {
			case "selection":
				stub.graphErr = selectionErr
			case "project":
				stub.graphErr = wrapped(codemap.ErrProjectNotFound)
			case "ambiguous":
				stub.resolutionErr = wrapped(codemap.ErrProjectAmbiguous)
			case "identity":
				stub.resolutionErr = wrapped(codemap.ErrProjectIdentityInvalid)
			case "node":
				stub.nodeErr = wrapped(codemap.ErrNodeNotFound)
			case "branch":
				stub.changeErr = wrapped(codemap.ErrBranchNotFound)
			case "repository":
				stub.changeErr = wrapped(codemap.ErrRepoNotFound)
			case "provider":
				stub.graphErr = fmt.Errorf("secret-database-password")
			case "search-selection":
				stub.searchErr = selectionErr
			case "search-provider":
				stub.searchErr = fmt.Errorf("secret-search-provider")
			default:
				t.Fatalf("unsupported fixture errorKind %q", testCase.ErrorKind)
			}
			base := startMapTestServer(t, stub)
			var path string
			switch testCase.Endpoint {
			case "map":
				path = "/api/v1/map/" + mapHandlerProjectHash.String()
			case "resolve":
				path = "/api/v1/projects/resolve?name=legacy"
			case "node":
				path = "/api/v1/map/" + mapHandlerProjectHash.String() + "/node?path=missing"
			case "change":
				path = "/api/v1/review/" + mapHandlerProjectHash.String() + "/change?branch=missing"
			case "search":
				path = "/api/v1/search?q=timeline"
			default:
				t.Fatalf("unsupported fixture endpoint %q", testCase.Endpoint)
			}
			status, _, raw := getJSON(t, base+path)
			if status != testCase.Status {
				t.Fatalf("status = %d, want %d; body=%s", status, testCase.Status, raw)
			}
			var body struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if body.Code != testCase.Code || body.Error == "" {
				t.Fatalf("error envelope = %+v, want code %q and actionable message", body, testCase.Code)
			}
			for _, fragment := range testCase.RequiredFragments {
				if !strings.Contains(body.Error, fragment) {
					t.Fatalf("error %q is missing required actionable fragment %q", body.Error, fragment)
				}
			}
			for _, fragment := range testCase.ForbiddenFragments {
				if strings.Contains(body.Error, fragment) {
					t.Fatalf("error %q leaked forbidden provider detail %q", body.Error, fragment)
				}
			}
		})
	}
}

func TestProjectSummariesHandler_PreservesDiscoveryErrorClass(t *testing.T) {
	_, selectionErr := sessionvisibility.New(config.SelectionConfig{Mode: "not-a-selection-mode"})
	if selectionErr == nil {
		t.Fatal("invalid selection config unexpectedly succeeded")
	}

	for _, testCase := range loadProjectSummariesErrorFixture(t).Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			providerErr := fmt.Errorf("database unavailable")
			if testCase.ErrorKind == "selection" {
				providerErr = selectionErr
			}
			stub := &mapStubProvider{summariesErr: providerErr}
			base := startMapTestServer(t, stub)
			status, _, raw := getJSON(t, base+"/api/v1/projects/summary")
			if status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%s", status, raw)
			}
			var body struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if body.Code != *testCase.WantCode {
				t.Fatalf("code = %q, want %q; body=%s", body.Code, *testCase.WantCode, raw)
			}
			if !strings.Contains(body.Error, providerErr.Error()) {
				t.Fatalf("error = %q, want provider cause %q", body.Error, providerErr)
			}
		})
	}
}

func TestProjectSummariesErrorFixtureRejectsSchemaAndCoverageDrift(t *testing.T) {
	unknownField := bytes.Replace(projectSummariesErrorFixtureYAML, []byte("errorKind: selection"), []byte("errorKind: selection\n    unexpected: true"), 1)
	if _, err := decodeProjectSummariesErrorFixture(unknownField); err == nil {
		t.Fatal("fixture loader accepted an unknown case field")
	}

	missingRequiredField := bytes.Replace(projectSummariesErrorFixtureYAML, []byte("    wantCode: selection_visibility\n"), nil, 1)
	if _, err := decodeProjectSummariesErrorFixture(missingRequiredField); err == nil {
		t.Fatal("fixture loader accepted a case missing wantCode")
	}

	renamedSemanticCase := bytes.Replace(projectSummariesErrorFixtureYAML, []byte("persisted selection failure is typed"), []byte("renamed selection behavior"), 1)
	if _, err := decodeProjectSummariesErrorFixture(renamedSemanticCase); err == nil {
		t.Fatal("fixture loader accepted a renamed required semantic case")
	}

	countDrift := bytes.Replace(projectSummariesErrorFixtureYAML, []byte("expectedCaseCount: 2"), []byte("expectedCaseCount: 3"), 1)
	if _, err := decodeProjectSummariesErrorFixture(countDrift); err == nil {
		t.Fatal("fixture loader accepted expectedCaseCount drift")
	}

	trailingDocument := append(append([]byte{}, projectSummariesErrorFixtureYAML...), []byte("\n---\nextra: document\n")...)
	if _, err := decodeProjectSummariesErrorFixture(trailingDocument); err == nil {
		t.Fatal("fixture loader accepted a trailing YAML document")
	}
}

func TestMapHandlers_NilProvider_Returns503(t *testing.T) {
	base := startMapTestServer(t, nil)

	urls := []string{
		base + "/api/v1/map/" + mapHandlerProjectHash.String(),
		base + "/api/v1/map/" + mapHandlerProjectHash.String() + "/node?path=x",
		base + "/api/v1/map/" + mapHandlerProjectHash.String() + "/tasks",
		base + "/api/v1/projects/summary",
		base + "/api/v1/review/" + mapHandlerProjectHash.String(),
		base + "/api/v1/review/" + mapHandlerProjectHash.String() + "/change?branch=x",
		base + "/api/v1/search?q=pipeline",
	}
	for _, url := range urls {
		status, contentType, raw := getJSON(t, url)
		if status != http.StatusServiceUnavailable {
			t.Errorf("GET %s: status = %d, want 503 (body: %s)", url, status, raw)
		}
		if contentType != defaults.ContentJSON.String() {
			t.Errorf("GET %s: Content-Type = %q, want %q", url, contentType, defaults.ContentJSON)
		}
	}
}

func TestSearchHandler_HappyPath(t *testing.T) {
	stub := &mapStubProvider{}
	base := startMapTestServer(t, stub)

	// limit is parsed and passed through; the query is forwarded verbatim.
	status, contentType, raw := getJSON(t, base+"/api/v1/search?q=ingest%20pipeline&limit=5")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, raw)
	}
	if contentType != defaults.ContentJSON.String() {
		t.Errorf("Content-Type = %q, want %q", contentType, defaults.ContentJSON)
	}
	if stub.gotQuery != "ingest pipeline" {
		t.Errorf("provider got query %q, want %q", stub.gotQuery, "ingest pipeline")
	}
	if stub.gotLimit != 5 {
		t.Errorf("provider got limit %d, want 5", stub.gotLimit)
	}

	var body struct {
		Query   string `json:"query"`
		Results []struct {
			SessionID  string `json:"sessionId"`
			EntryIndex int    `json:"entryIndex"`
			Snippet    string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, raw)
	}
	if body.Query != "ingest pipeline" {
		t.Errorf("query = %q, want %q", body.Query, "ingest pipeline")
	}
	if len(body.Results) != 1 {
		t.Fatalf("results = %d, want 1: %s", len(body.Results), raw)
	}
	if body.Results[0].SessionID != "sess-1" || body.Results[0].EntryIndex != 4 {
		t.Errorf("result coords = (%q, %d), want (sess-1, 4)", body.Results[0].SessionID, body.Results[0].EntryIndex)
	}

	// Absent limit forwards 0 (provider applies its default).
	if _, _, _ = getJSON(t, base+"/api/v1/search?q=pipeline"); stub.gotLimit != 0 {
		t.Errorf("absent limit: provider got %d, want 0 (default sentinel)", stub.gotLimit)
	}
}

// TestMapHandlers_FrictionClusterShape_ThroughRouter is the M-B route-level wiring
// verification: it drives a POPULATED FrictionCluster end-to-end through the REAL
// internal/api Server (registered router + JSON encoder), proving (1) the
// /review/{projectHash}/change op is actually wired to the JSON handler and (2)
// FrictionCluster serializes with its full neutral shape (kind/label/file/count/
// sessions) across the HTTP boundary. The existing happy-path case only asserts
// `frictions` is a never-null array (the stub returns it empty); this case asserts
// the object fields themselves.
//
// Real failure signal: an UNregistered route does NOT 404 here. It falls through to
// the catch-all `/` handler, which returns 200 + the "Frontend not built yet" HTML
// placeholder. So a missing/mis-wired registration surfaces as a text/html
// Content-Type (and a failed JSON unmarshal), NOT as a non-200 status. The
// Content-Type assertion is therefore the load-bearing wiring check, and it is fatal
// BEFORE we attempt to decode the body.
func TestMapHandlers_FrictionClusterShape_ThroughRouter(t *testing.T) {
	stub := &mapStubProvider{
		frictions: []schema.FrictionCluster{
			{Kind: "retryLoop", Label: "retry loops", File: "internal/queue/retry.go", Count: 4, Sessions: 2},
		},
	}
	base := startMapTestServer(t, stub)

	status, contentType, raw := getJSON(t, base+"/api/v1/review/"+mapHandlerProjectHash.String()+"/change?branch=feat%2Fqueue-retry")
	// Content-Type is the real "route is wired" signal: a fall-through to the
	// catch-all `/` handler still returns 200 but with text/html, so assert this
	// fatally before unmarshal — that is the right reason for this test to fail.
	if contentType != defaults.ContentJSON.String() {
		t.Fatalf("Content-Type = %q, want %q — did /review/{projectHash}/change fall through to the catch-all frontend handler? (status: %d, body: %s)", contentType, defaults.ContentJSON, status, raw)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, raw)
	}

	var body struct {
		Frictions []schema.FrictionCluster `json:"frictions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, raw)
	}
	if len(body.Frictions) != 1 {
		t.Fatalf("frictions len = %d, want 1 (body: %s)", len(body.Frictions), raw)
	}
	got := body.Frictions[0]
	want := schema.FrictionCluster{Kind: "retryLoop", Label: "retry loops", File: "internal/queue/retry.go", Count: 4, Sessions: 2}
	if got != want {
		t.Errorf("FrictionCluster shape through router = %+v, want %+v", got, want)
	}
}
