package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sync_push_validation.yaml
var syncPushValidationData []byte

type syncPushValidationFixture struct {
	Name           string                `yaml:"name"`
	SessionIDs     []string              `yaml:"sessionIds"`
	RedactionLevel redact.RedactionLevel `yaml:"redactionLevel"`
	Visibility     string                `yaml:"visibility"`
	ExpectedStatus int                   `yaml:"expectedStatus"`
	ExpectedError  string                `yaml:"expectedError"`
}

type syncPushValidationFixtures struct {
	ExpectedCaseCount int                         `yaml:"expectedCaseCount"`
	Cases             []syncPushValidationFixture `yaml:"cases"`
}

func loadSyncPushValidationFixtures() ([]syncPushValidationFixture, error) {
	return decodeSyncPushValidationFixtures(syncPushValidationData)
}

func decodeSyncPushValidationFixtures(data []byte) ([]syncPushValidationFixture, error) {
	const path = "internal/api/testdata/sync_push_validation.yaml"
	var fixtures syncPushValidationFixtures
	// Strict fields, then a decode asserted at EOF. The previous bare Unmarshal
	// accepted an unknown key silently, which left the field it was meant to set at
	// its zero value - and every field here has a meaningful zero, so a mistyped
	// expectedError became the empty string and matched nothing.
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		return nil, fmt.Errorf("api: could not parse %s while loading request-validation cases: %w; the push boundary test cannot run; fix the key to one the typed schema declares", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("api: %s must contain exactly one YAML document; anything after the first is silently ignored, so cases below it prove nothing (%v); remove the second document", path, err)
	}
	if len(fixtures.Cases) == 0 || fixtures.ExpectedCaseCount != len(fixtures.Cases) {
		return nil, fmt.Errorf("api: %s declares expectedCaseCount=%d but carries %d cases; a corpus that silently shrinks still passes every assertion it still contains; set the count to the cases actually present", path, fixtures.ExpectedCaseCount, len(fixtures.Cases))
	}
	seen := map[string]bool{}
	coveredArms := map[redactionRejectionArm]bool{}
	for index, fixture := range fixtures.Cases {
		for key, value := range map[string]string{
			"name":           fixture.Name,
			"redactionLevel": fixture.RedactionLevel.String(),
			"expectedError":  fixture.ExpectedError,
		} {
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("api: %s case %d has a blank %s; an empty expected value matches whatever the handler returned, turning the assertion into a guaranteed pass; supply the exact value", path, index, key)
			}
		}
		if len(fixture.SessionIDs) == 0 || fixture.ExpectedStatus == 0 {
			return nil, fmt.Errorf("api: %s case %d is incomplete; sessionIds and expectedStatus are required or the test cannot construct or verify the request", path, index)
		}
		if seen[fixture.Name] {
			return nil, fmt.Errorf("api: %s case %d duplicates the name %q; a failure must name exactly one scenario", path, index, fixture.Name)
		}
		seen[fixture.Name] = true
		coveredArms[redactionRejectionArmOf(fixture.RedactionLevel)] = true
	}
	if err := assertEveryRedactionRejectionArmCovered(path, coveredArms); err != nil {
		return nil, err
	}
	return fixtures.Cases, nil
}

func TestHandleSyncPush_RejectsInvalidRedactionLevelAsJSON(t *testing.T) {
	fixtures, err := loadSyncPushValidationFixtures()
	if err != nil {
		t.Fatal(err)
	}
	// If credential loading happens before request validation, this empty
	// isolated config home forces an authentication response instead of the
	// fixture's expected validation response.
	t.Setenv(defaults.EnvXDGConfigHome.String(), t.TempDir())

	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			body, err := json.Marshal(pushRequest{
				SessionIDs:     fixture.SessionIDs,
				RedactionLevel: fixture.RedactionLevel.String(),
				Visibility:     fixture.Visibility,
			})
			if err != nil {
				t.Fatalf("marshal %s request: %v", fixture.Name, err)
			}

			request := httptest.NewRequest("POST", "/api/v1/sync/push", bytes.NewReader(body))
			response := httptest.NewRecorder()
			handler := &syncHandler{store: new(store.Store), config: new(config.Config)}
			handler.handleSyncPush(response, request)

			if response.Code != fixture.ExpectedStatus {
				t.Fatalf("%s status = %d, want %d; body: %s", fixture.Name, response.Code, fixture.ExpectedStatus, response.Body.String())
			}
			if got := response.Header().Get(defaults.HeaderContentType); got != defaults.ContentJSON.String() {
				t.Errorf("%s Content-Type = %q, want %q", fixture.Name, got, defaults.ContentJSON.String())
			}
			var payload map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("%s response is not valid JSON: %v; body: %q", fixture.Name, err, response.Body.String())
			}
			if got := payload["error"]; got != fixture.ExpectedError {
				t.Errorf("%s error = %q, want %q", fixture.Name, got, fixture.ExpectedError)
			}
		})
	}
}
