package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sync_custom_pattern_failures.yaml
var syncCustomPatternFailuresYAML []byte

type syncCustomPatternFailureKind string

const (
	syncCustomPatternFailureCategory syncCustomPatternFailureKind = "category"
	syncCustomPatternFailureRegex    syncCustomPatternFailureKind = "regex"
)

type syncCustomPatternFailureDocument struct {
	ExpectedCaseCount int                               `yaml:"expectedCaseCount"`
	Cases             []syncCustomPatternFailureFixture `yaml:"cases"`
}

type syncCustomPatternFailureFixture struct {
	Name                  string                       `yaml:"name"`
	Kind                  syncCustomPatternFailureKind `yaml:"kind"`
	Pattern               config.CustomPattern         `yaml:"pattern"`
	ExpectedErrorContains string                       `yaml:"expectedErrorContains"`
}

func loadSyncCustomPatternFailureFixtures(data []byte) ([]syncCustomPatternFailureFixture, error) {
	const path = "internal/api/testdata/sync_custom_pattern_failures.yaml"
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document syncCustomPatternFailureDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%s must contain exactly one YAML document", path)
	}
	if document.ExpectedCaseCount != len(document.Cases) || document.ExpectedCaseCount != 2 {
		return nil, fmt.Errorf("%s must define exactly two guarded cases", path)
	}
	seenNames := map[string]bool{}
	seenKinds := map[syncCustomPatternFailureKind]bool{}
	for _, fixture := range document.Cases {
		if fixture.Name == "" || seenNames[fixture.Name] || fixture.Pattern.ID == "" || fixture.Pattern.Pattern == "" || fixture.ExpectedErrorContains == "" {
			return nil, fmt.Errorf("%s contains a blank, duplicate, or vacuous case %q", path, fixture.Name)
		}
		if fixture.Kind != syncCustomPatternFailureCategory && fixture.Kind != syncCustomPatternFailureRegex {
			return nil, fmt.Errorf("%s case %q has unknown failure kind %q", path, fixture.Name, fixture.Kind)
		}
		if _, err := config.CustomPatternsToUserPatterns([]config.CustomPattern{fixture.Pattern}); err == nil || !strings.Contains(err.Error(), fixture.ExpectedErrorContains) {
			return nil, fmt.Errorf("%s case %q does not activate its expected conversion failure", path, fixture.Name)
		}
		seenNames[fixture.Name] = true
		seenKinds[fixture.Kind] = true
	}
	if !seenKinds[syncCustomPatternFailureCategory] || !seenKinds[syncCustomPatternFailureRegex] {
		return nil, fmt.Errorf("%s must cover both category and regex failures", path)
	}
	return document.Cases, nil
}

func TestSyncEndpointsRejectInvalidCustomPatternsBeforeSideEffects(t *testing.T) {
	fixtures, err := loadSyncCustomPatternFailureFixtures(syncCustomPatternFailuresYAML)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(defaults.EnvXDGConfigHome.String(), t.TempDir())
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			cfg := config.BaseConfig()
			cfg.Redaction.CustomPatterns = []config.CustomPattern{fixture.Pattern}
			handler := &syncHandler{config: cfg}

			previewRequest := httptest.NewRequest(http.MethodGet, "/api/v1/sync/redactions?session_id=11111111-1111-1111-1111-111111111111", nil)
			previewResponse := httptest.NewRecorder()
			handler.handleSyncRedactions(previewResponse, previewRequest)
			assertSyncCustomPatternFailure(t, "preview", previewResponse, fixture.ExpectedErrorContains)

			body, marshalErr := json.Marshal(pushRequest{SessionIDs: []string{"11111111-1111-1111-1111-111111111111"}, Visibility: "private"})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			pushRequest := httptest.NewRequest(http.MethodPost, "/api/v1/sync/push", bytes.NewReader(body))
			pushResponse := httptest.NewRecorder()
			handler.handleSyncPush(pushResponse, pushRequest)
			assertSyncCustomPatternFailure(t, "push", pushResponse, fixture.ExpectedErrorContains)
		})
	}
}

func assertSyncCustomPatternFailure(t *testing.T, endpoint string, response *httptest.ResponseRecorder, expected string) {
	t.Helper()
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("%s status = %d, want %d; body: %s", endpoint, response.Code, http.StatusInternalServerError, response.Body.String())
	}
	if got := response.Header().Get(defaults.HeaderContentType); got != defaults.ContentJSON.String() {
		t.Errorf("%s Content-Type = %q, want %q", endpoint, got, defaults.ContentJSON.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("%s response is not JSON: %v; body: %s", endpoint, err, response.Body.String())
	}
	message := payload["error"]
	if !strings.Contains(message, expected) {
		t.Errorf("%s error %q does not contain conversion failure %q", endpoint, message, expected)
	}
	if !strings.Contains(message, "no transcript was scanned or published") {
		t.Errorf("%s error %q does not state the fail-closed outcome", endpoint, message)
	}
	if !strings.Contains(message, "fix redaction.custom_patterns") {
		t.Errorf("%s error %q does not give the configuration remedy", endpoint, message)
	}
}

func TestSyncCustomPatternFailureFixtureStrictness(t *testing.T) {
	if _, err := loadSyncCustomPatternFailureFixtures(append(syncCustomPatternFailuresYAML, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("accepted a second YAML document")
	}
	if _, err := loadSyncCustomPatternFailureFixtures(bytes.Replace(syncCustomPatternFailuresYAML, []byte("expectedCaseCount:"), []byte("unknown: true\nexpectedCaseCount:"), 1)); err == nil {
		t.Fatal("accepted an unknown YAML field")
	}
}
