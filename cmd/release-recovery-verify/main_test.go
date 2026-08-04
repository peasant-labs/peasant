package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type commandMutation string

const (
	commandMutationNone                   commandMutation = "none"
	commandMutationMissingToken           commandMutation = "missing_token"
	commandMutationSwappedGateReceipts    commandMutation = "swapped_gate_receipts"
	commandMutationMissingRecoveryConfirm commandMutation = "missing_recovery_confirmation"
)

type commandFixture struct {
	Responses []struct {
		Path   string `yaml:"path"`
		Query  string `yaml:"query"`
		Status int    `yaml:"status"`
		Body   string `yaml:"body"`
	} `yaml:"responses"`
	CommandCases []struct {
		Name      string          `yaml:"name"`
		Mutation  commandMutation `yaml:"mutation"`
		WantError string          `yaml:"want_error"`
	} `yaml:"command_cases"`
}

type workflowEnvironmentFixture struct {
	Workflow struct {
		PreflightEnv []struct {
			Key       string `yaml:"key"`
			TestValue string `yaml:"test_value"`
		} `yaml:"preflight_env"`
	} `yaml:"workflow"`
}

func TestRunPreflightUsesMountedWorkflowEnvironment(t *testing.T) {
	fixture := loadCommandFixture(t)
	environment := loadWorkflowEnvironmentFixture(t)
	for _, testCase := range fixture.CommandCases {
		t.Run(testCase.Name, func(t *testing.T) {
			server := newCommandFixtureServer(t, fixture)
			defer server.Close()
			for _, variable := range environment.Workflow.PreflightEnv {
				t.Setenv(variable.Key, variable.TestValue)
			}
			t.Setenv("GITHUB_API_URL", server.URL)
			applyCommandMutation(t, testCase.Mutation)
			var output bytes.Buffer
			err := run([]string{"preflight"}, func(config *runConfig) {
				config.httpClient = server.Client()
				config.now = func() time.Time { return time.Date(2026, time.August, 4, 22, 0, 0, 0, time.UTC) }
				config.stdout = &output
			})
			assertCommandError(t, err, testCase.WantError)
			if err == nil && !strings.Contains(output.String(), "immutable v0.1.0 recovery evidence verified") {
				t.Fatalf("release recovery command: success output = %q", output.String())
			}
		})
	}
}

func TestRunPrePublishRevalidatesRemoteState(t *testing.T) {
	fixture := loadCommandFixture(t)
	server := newCommandFixtureServer(t, fixture)
	defer server.Close()
	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("GH_TOKEN", "fixture-token")
	t.Setenv("RECOVERY_RUN_ID", "91001")
	t.Setenv("RECOVERY_HEAD_SHA", "1111111111111111111111111111111111111111")
	var output bytes.Buffer
	err := run([]string{"pre-publish"}, func(config *runConfig) {
		config.httpClient = server.Client()
		config.now = func() time.Time { return time.Date(2026, time.August, 4, 22, 0, 0, 0, time.UTC) }
		config.stdout = &output
	})
	if err != nil {
		t.Fatalf("release recovery command: pre-publish failed: %v", err)
	}
	if !strings.Contains(output.String(), "re-verified immediately before publication") {
		t.Fatalf("release recovery command: pre-publish output = %q", output.String())
	}
}

func loadCommandFixture(t *testing.T) commandFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "preflight_api.yaml"))
	if err != nil {
		t.Fatalf("release recovery command: read API fixture: %v", err)
	}
	var fixture commandFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("release recovery command: parse API fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("release recovery command: API fixture must have exact EOF: %v", err)
	}
	if len(fixture.Responses) != 18 || len(fixture.CommandCases) != 4 {
		t.Fatalf("release recovery command: fixture row-count guard failed: %+v", fixture)
	}
	seenRequests := make(map[string]struct{}, len(fixture.Responses))
	for _, response := range fixture.Responses {
		if response.Path == "" || response.Status == 0 || response.Body == "" {
			t.Fatalf("release recovery command: incomplete API response: %+v", response)
		}
		requestTarget := response.Path
		if response.Query != "" {
			requestTarget += "?" + response.Query
		}
		if _, exists := seenRequests[requestTarget]; exists {
			t.Fatalf("release recovery command: duplicate API request %q", requestTarget)
		}
		seenRequests[requestTarget] = struct{}{}
	}
	return fixture
}

func loadWorkflowEnvironmentFixture(t *testing.T) workflowEnvironmentFixture {
	t.Helper()
	root := commandRepoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "internal", "e2e", "testdata", "workflows", "recovery_contract.yaml"))
	if err != nil {
		t.Fatalf("release recovery command: read mounted workflow fixture: %v", err)
	}
	var fixture workflowEnvironmentFixture
	if err := yaml.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("release recovery command: parse mounted workflow fixture: %v", err)
	}
	if len(fixture.Workflow.PreflightEnv) != 14 {
		t.Fatalf("release recovery command: mounted environment row-count guard failed: %+v", fixture)
	}
	return fixture
}

func newCommandFixtureServer(t *testing.T, fixture commandFixture) *httptest.Server {
	t.Helper()
	responses := make(map[string]struct {
		status int
		body   string
	}, len(fixture.Responses))
	for _, response := range fixture.Responses {
		requestTarget := response.Path
		if response.Query != "" {
			requestTarget += "?" + response.Query
		}
		responses[requestTarget] = struct {
			status int
			body   string
		}{status: response.Status, body: response.Body}
	}
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer fixture-token" || request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		requestTarget := request.URL.Path
		if request.URL.RawQuery != "" {
			requestTarget += "?" + request.URL.RawQuery
		}
		response, ok := responses[requestTarget]
		if !ok {
			t.Errorf("release recovery command: unexpected API request %q", requestTarget)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(response.status)
		_, _ = io.WriteString(writer, response.body)
	}))
}

func applyCommandMutation(t *testing.T, mutation commandMutation) {
	t.Helper()
	switch mutation {
	case commandMutationNone:
	case commandMutationMissingToken:
		t.Setenv("GH_TOKEN", "")
	case commandMutationSwappedGateReceipts:
		e2e := os.Getenv("E2E_RUN_ID")
		releaseE2E := os.Getenv("RELEASE_E2E_RUN_ID")
		t.Setenv("E2E_RUN_ID", releaseE2E)
		t.Setenv("RELEASE_E2E_RUN_ID", e2e)
	case commandMutationMissingRecoveryConfirm:
		t.Setenv("CONFIRM_RECOVERY_COMMIT", "")
	default:
		t.Fatalf("release recovery command: unsupported mutation %q", mutation)
	}
}

func assertCommandError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("release recovery command: unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("release recovery command: error = %v, want substring %q", err, want)
	}
}

func commandRepoRoot(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("release recovery command: get working directory: %v", err)
	}
	for directory := workingDirectory; ; directory = filepath.Dir(directory) {
		data, err := os.ReadFile(filepath.Join(directory, "go.mod"))
		if err == nil && strings.Contains(string(data), "module github.com/peasant-labs/peasant") {
			return directory
		}
		if parent := filepath.Dir(directory); parent == directory {
			t.Fatalf("release recovery command: cannot locate repository root from %s", workingDirectory)
		}
	}
}
