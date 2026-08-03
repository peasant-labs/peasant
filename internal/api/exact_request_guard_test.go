package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/exact_request_guard.yaml
var exactRequestGuardYAML []byte

const exactRequestGuardCaseCount = 13

type exactRequestGuardFixture struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	RequiredNames     []string `yaml:"requiredNames"`
	Cases             []struct {
		Name              string   `yaml:"name"`
		Handler           string   `yaml:"handler"`
		RawQuery          string   `yaml:"rawQuery"`
		PathHash          string   `yaml:"pathHash"`
		Provider          bool     `yaml:"provider"`
		Status            int      `yaml:"status"`
		Code              string   `yaml:"code"`
		RequiredFragments []string `yaml:"requiredFragments"`
	} `yaml:"cases"`
}

func decodeExactRequestGuardFixture(source []byte) (exactRequestGuardFixture, error) {
	var fixture exactRequestGuardFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode exact request guard fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fixture, fmt.Errorf("exact request guard fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.ExpectedCaseCount != exactRequestGuardCaseCount || len(fixture.RequiredNames) != exactRequestGuardCaseCount || len(fixture.Cases) != exactRequestGuardCaseCount {
		return fixture, fmt.Errorf("exact request guard fixture cardinality mismatch")
	}
	required := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if name == "" {
			return fixture, fmt.Errorf("exact request guard fixture has an empty required name")
		}
		if _, duplicate := required[name]; duplicate {
			return fixture, fmt.Errorf("exact request guard fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || testCase.Handler == "" || testCase.PathHash == "" || testCase.Status == 0 || testCase.Code == "" || len(testCase.RequiredFragments) == 0 {
			return fixture, fmt.Errorf("exact request guard fixture has an incomplete case %q", testCase.Name)
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return fixture, fmt.Errorf("exact request guard fixture repeats case %q", testCase.Name)
		}
		if _, ok := required[testCase.Name]; !ok {
			return fixture, fmt.Errorf("exact request guard fixture has unknown case %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		for _, fragment := range testCase.RequiredFragments {
			if fragment == "" {
				return fixture, fmt.Errorf("exact request guard fixture case %q has an empty message fragment", testCase.Name)
			}
		}
	}
	for name := range required {
		if _, ok := seen[name]; !ok {
			return fixture, fmt.Errorf("exact request guard fixture is missing required case %q", name)
		}
	}
	return fixture, nil
}

type requestGuardProvider struct {
	mockDataProvider
	calls int
}

func (p *requestGuardProvider) ProjectSummaries(ctx context.Context) (*codemap.ProjectSummariesResult, error) {
	p.calls++
	return p.mockDataProvider.ProjectSummaries(ctx)
}
func (p *requestGuardProvider) ResolveProject(ctx context.Context, project string) (*schema.ProjectResolutionPayload, error) {
	p.calls++
	return p.mockDataProvider.ResolveProject(ctx, project)
}
func (p *requestGuardProvider) MapGraph(ctx context.Context, hash schema.ProjectHash, commit string) (*schema.MapGraphPayload, error) {
	p.calls++
	return p.mockDataProvider.MapGraph(ctx, hash, commit)
}
func (p *requestGuardProvider) MapNodeDetail(ctx context.Context, hash schema.ProjectHash, path string) (*schema.MapNodeDetailPayload, error) {
	p.calls++
	return p.mockDataProvider.MapNodeDetail(ctx, hash, path)
}
func (p *requestGuardProvider) ReviewChanges(ctx context.Context, hash schema.ProjectHash) (*schema.ReviewListPayload, error) {
	p.calls++
	return p.mockDataProvider.ReviewChanges(ctx, hash)
}
func (p *requestGuardProvider) ChangeDiff(ctx context.Context, hash schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error) {
	p.calls++
	return p.mockDataProvider.ChangeDiff(ctx, hash, branch, file)
}
func (p *requestGuardProvider) Search(ctx context.Context, query string, limit int) (*schema.SearchPayload, error) {
	p.calls++
	return p.mockDataProvider.Search(ctx, query, limit)
}

func TestExactRequestGuardFixture(t *testing.T) {
	fixture, err := decodeExactRequestGuardFixture(exactRequestGuardYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			provider := &requestGuardProvider{}
			var configured DataProvider
			if testCase.Provider {
				configured = provider
			}
			server := NewServer(ServerConfig{Provider: configured})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.URL.RawQuery = testCase.RawQuery
			if testCase.PathHash == "valid" {
				request.SetPathValue("projectHash", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			}
			response := httptest.NewRecorder()
			switch testCase.Handler {
			case "map":
				server.handleMapGraph(response, request)
			case "node":
				server.handleMapNode(response, request)
			case "resolve":
				server.handleProjectResolve(response, request)
			case "changes":
				server.handleReviewChanges(response, request)
			case "diff":
				server.handleReviewDiff(response, request)
			case "summaries":
				server.handleProjectSummaries(response, request)
			case "search":
				server.handleSearch(response, request)
			default:
				t.Fatalf("unsupported fixture handler %q", testCase.Handler)
			}
			if response.Code != testCase.Status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, testCase.Status, response.Body.String())
			}
			var body struct{ Error, Code string }
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != testCase.Code {
				t.Fatalf("code = %q, want %q", body.Code, testCase.Code)
			}
			for _, fragment := range testCase.RequiredFragments {
				if !strings.Contains(body.Error, fragment) {
					t.Fatalf("error %q is missing required fragment %q", body.Error, fragment)
				}
			}
			if provider.calls != 0 {
				t.Fatalf("provider calls = %d, want 0", provider.calls)
			}
		})
	}
}

func TestExactRequestGuardFixtureRejectsMutations(t *testing.T) {
	unknown := bytes.Replace(exactRequestGuardYAML, []byte("expectedCaseCount:"), []byte("unknown: true\nexpectedCaseCount:"), 1)
	if _, err := decodeExactRequestGuardFixture(unknown); err == nil {
		t.Fatal("unknown-field mutation unexpectedly validated")
	}
	trailing := append(append([]byte{}, exactRequestGuardYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeExactRequestGuardFixture(trailing); err == nil {
		t.Fatal("trailing-document mutation unexpectedly validated")
	}
	renamed := bytes.Replace(exactRequestGuardYAML, []byte("name: duplicate commit is rejected"), []byte("name: renamed duplicate behavior"), 1)
	if _, err := decodeExactRequestGuardFixture(renamed); err == nil {
		t.Fatal("count-preserving semantic rename unexpectedly validated")
	}
	deleted := bytes.Replace(exactRequestGuardYAML, []byte("  - {name: short search query is rejected, handler: search, rawQuery: \"q=a\", pathHash: none, provider: true, status: 400, code: search_query_too_short, requiredFragments: [fewer than two non-whitespace characters, internal/api.handleSearch, No provider call was made, Enter at least two characters]}\n"), nil, 1)
	if _, err := decodeExactRequestGuardFixture(deleted); err == nil {
		t.Fatal("semantic deletion unexpectedly validated")
	}
}
