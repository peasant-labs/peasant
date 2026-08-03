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

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sync_redactions_validation.yaml
var syncRedactionsValidationData []byte

type syncRedactionsValidationFixture struct {
	Name           string                `yaml:"name"`
	SessionID      string                `yaml:"sessionId"`
	Level          redact.RedactionLevel `yaml:"level"`
	ExpectedStatus int                   `yaml:"expectedStatus"`
	ExpectedError  string                `yaml:"expectedError"`
}

type syncRedactionsValidationFixtures struct {
	ExpectedCaseCount int                               `yaml:"expectedCaseCount"`
	Cases             []syncRedactionsValidationFixture `yaml:"cases"`
}

func loadSyncRedactionsValidationFixtures() ([]syncRedactionsValidationFixture, error) {
	return decodeSyncRedactionsValidationFixtures(syncRedactionsValidationData)
}

func decodeSyncRedactionsValidationFixtures(data []byte) ([]syncRedactionsValidationFixture, error) {
	const path = "internal/api/testdata/sync_redactions_validation.yaml"
	var fixtures syncRedactionsValidationFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		return nil, fmt.Errorf("api: could not parse %s while loading preview request-validation cases: %w; the GET boundary test cannot run; fix the key to one the typed schema declares", path, err)
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
			"name":          fixture.Name,
			"sessionId":     fixture.SessionID,
			"level":         fixture.Level.String(),
			"expectedError": fixture.ExpectedError,
		} {
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("api: %s case %d has a blank %s; an empty expected value matches whatever the handler returned, turning the assertion into a guaranteed pass; supply the exact value", path, index, key)
			}
		}
		if fixture.ExpectedStatus == 0 {
			return nil, fmt.Errorf("api: %s case %d has no expectedStatus, so the test cannot verify the response code", path, index)
		}
		if seen[fixture.Name] {
			return nil, fmt.Errorf("api: %s case %d duplicates the name %q; a failure must name exactly one scenario", path, index, fixture.Name)
		}
		seen[fixture.Name] = true
		coveredArms[redactionRejectionArmOf(fixture.Level)] = true
	}
	if err := assertEveryRedactionRejectionArmCovered(path, coveredArms); err != nil {
		return nil, err
	}
	return fixtures.Cases, nil
}

func TestHandleSyncRedactionsRejectsInvalidLevelAsJSON(t *testing.T) {
	fixtures, err := loadSyncRedactionsValidationFixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			request := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/sync/redactions?session_id=%s&level=%s", fixture.SessionID, fixture.Level), nil)
			response := httptest.NewRecorder()
			new(syncHandler).handleSyncRedactions(response, request)

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
