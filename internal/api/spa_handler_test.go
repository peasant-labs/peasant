package api

import (
	"bytes"
	_ "embed"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/spa_dynamic_routes.yaml
var spaDynamicRoutesFixtureYAML []byte

type spaDynamicRoutesFixture struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	RequiredNames     []string `yaml:"requiredNames"`
	Cases             []struct {
		Name                string `yaml:"name"`
		Path                string `yaml:"path"`
		ExpectedStatus      int    `yaml:"expectedStatus"`
		ExpectedContentType string `yaml:"expectedContentType"`
		ExpectedBody        string `yaml:"expectedBody"`
	} `yaml:"cases"`
}

var requiredSPADynamicRouteNames = map[string]struct{}{
	"dotted timestamp session serves projects HTML":        {},
	"dotted timestamp session serves projects flight data": {},
	"dotted project name serves Map HTML":                  {},
	"dotted project name serves Map flight data":           {},
	"dotted project name serves Review HTML":               {},
	"dotted project name serves Review flight data":        {},
	"missing Next static chunk stays not found":            {},
	"unrelated file extension stays not found":             {},
}

func loadSPADynamicRoutesFixture(t *testing.T) spaDynamicRoutesFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(spaDynamicRoutesFixtureYAML))
	decoder.KnownFields(true)
	var fixture spaDynamicRoutesFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode SPA dynamic route fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("SPA dynamic route fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.ExpectedCaseCount != len(requiredSPADynamicRouteNames) || len(fixture.RequiredNames) != fixture.ExpectedCaseCount || len(fixture.Cases) != fixture.ExpectedCaseCount {
		t.Fatalf("SPA dynamic route fixture cardinality = expected %d required %d cases %d, want %d each", fixture.ExpectedCaseCount, len(fixture.RequiredNames), len(fixture.Cases), len(requiredSPADynamicRouteNames))
	}
	required := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if _, known := requiredSPADynamicRouteNames[name]; !known {
			t.Fatalf("SPA dynamic route fixture has unknown required name %q", name)
		}
		if _, duplicate := required[name]; duplicate {
			t.Fatalf("SPA dynamic route fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if _, requiredCase := required[testCase.Name]; !requiredCase {
			t.Fatalf("SPA dynamic route fixture has unknown case %q", testCase.Name)
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			t.Fatalf("SPA dynamic route fixture repeats case %q", testCase.Name)
		}
		if testCase.Path == "" || testCase.ExpectedStatus == 0 || testCase.ExpectedContentType == "" || testCase.ExpectedBody == "" {
			t.Fatalf("SPA dynamic route fixture case %q has an empty required field", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
	}
	return fixture
}

// TestSPAHandler_GitkeepOnly_ServesNotBundledNotice covers the degraded build:
// web/out embedded with ONLY the tracked `.gitkeep` placeholder and no
// index.html — i.e. `go install …@tag`, `nix build`, or a bare `go build` that
// never ran `make web`/`make web-stub`. The SPA root must serve the built-in
// "not bundled" notice (HTTP 200), NOT a bare 404.
func TestSPAHandler_GitkeepOnly_ServesNotBundledNotice(t *testing.T) {
	h := &spaHandler{fs: http.FS(fstest.MapFS{
		".gitkeep": {Data: []byte{}},
	})}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / : status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / : Content-Type = %q, want text/html…", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not bundled in this build") {
		t.Fatalf("GET / : body missing the not-bundled notice; got %q", body)
	}
}

// TestSPAHandler_RealIndex_ServesDashboard guards against regressing the normal
// serve: when a real index.html is embedded, the SPA root serves it (not the
// notice).
func TestSPAHandler_RealIndex_ServesDashboard(t *testing.T) {
	const want = "<html><body>REAL DASHBOARD</body></html>"
	h := &spaHandler{fs: http.FS(fstest.MapFS{
		"index.html": {Data: []byte(want)},
		".gitkeep":   {Data: []byte{}},
	})}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / : status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "REAL DASHBOARD") {
		t.Fatalf("GET / : expected the real dashboard, got %q", body)
	}
	if strings.Contains(body, "not bundled") {
		t.Fatalf("GET / : served the not-bundled notice instead of the real index.html")
	}
}

func TestSPAHandler_DynamicRoutesPreserveDottedIdentities(t *testing.T) {
	fixture := loadSPADynamicRoutesFixture(t)
	h := &spaHandler{fs: http.FS(fstest.MapFS{
		"index.html":          {Data: []byte("ROOT HTML")},
		"index.txt":           {Data: []byte("ROOT FLIGHT")},
		"projects/index.html": {Data: []byte("PROJECT HTML")},
		"projects/index.txt":  {Data: []byte("PROJECT FLIGHT")},
		"map/index.html":      {Data: []byte("MAP HTML")},
		"map/index.txt":       {Data: []byte("MAP FLIGHT")},
		"review/index.html":   {Data: []byte("REVIEW HTML")},
		"review/index.txt":    {Data: []byte("REVIEW FLIGHT")},
	})}

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.Path, nil))
			if recorder.Code != testCase.ExpectedStatus {
				t.Errorf("GET %s status = %d, want %d", testCase.Path, recorder.Code, testCase.ExpectedStatus)
			}
			if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, testCase.ExpectedContentType) {
				t.Errorf("GET %s Content-Type = %q, want prefix %q", testCase.Path, contentType, testCase.ExpectedContentType)
			}
			if body := recorder.Body.String(); !strings.Contains(body, testCase.ExpectedBody) {
				t.Errorf("GET %s body = %q, want content %q", testCase.Path, body, testCase.ExpectedBody)
			}
		})
	}

	if got := len(fixture.Cases); got != fixture.ExpectedCaseCount {
		t.Fatalf("executed %d SPA dynamic route fixture cases, want %d", got, fixture.ExpectedCaseCount)
	}
}
