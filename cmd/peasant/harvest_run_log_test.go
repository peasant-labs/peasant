package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harvest_run_log.yaml
var harvestRunLogYAML []byte

type harvestRunLogFixture struct {
	Name         string   `yaml:"name"`
	Args         []string `yaml:"args"`
	SetupHarvest bool     `yaml:"setup_harvest"`
	WantError    bool     `yaml:"want_error"`
	WantLog      bool     `yaml:"want_log"`
	JSONStdout   bool     `yaml:"json_stdout"`
	LogContains  []string `yaml:"log_contains"`
}

func loadHarvestRunLogFixtures(t testing.TB) []harvestRunLogFixture {
	t.Helper()
	var document struct {
		RequiredNames []string               `yaml:"required_names"`
		Cases         []harvestRunLogFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(harvestRunLogYAML, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate harvest run log fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("harvest run log", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// harvestRunLogName is the file name the issue fixes: harvest--<unix>.log.
var harvestRunLogName = regexp.MustCompile(`^harvest--[0-9]+\.log$`)

// seedHarvestRunLogSource writes one good and one malformed Claude Code session
// and a config that reads only them, returning the config path.
func seedHarvestRunLogSource(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "projects")
	project := filepath.Join(src, "-tmp-qa-run-log")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	record := func(sid, uuid string) string {
		return `{"type":"user","sessionId":"` + sid + `","uuid":"` + uuid + `","cwd":"/tmp/qa-run-log","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
	}
	good := "aaaaaaaa-0000-4000-8000-000000000001"
	bad := "dddddddd-0000-4000-8000-000000000001"
	if err := os.WriteFile(filepath.Join(project, good+".jsonl"), []byte(record(good, "u1")+record(good, "u2")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, bad+".jsonl"), []byte(record(bad, "u1")+"{\"type\":\"user\",\"broken\n"+record(bad, "u3")), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n", src, filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// runHarvestWithStateDir runs the production harvest command under a root that
// carries --state-dir, returning stdout, stderr, and the command error.
func runHarvestWithStateDir(t *testing.T, dir, configPath, stateDir string, args []string) (string, string, error) {
	t.Helper()
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.PersistentFlags().String("state-dir", "", "")
	root.AddCommand(BuildHarvestCommand())
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"harvest"}, append(args,
		"--data-dir", dir, "--config-dir", dir, "--config", configPath, "--state-dir", stateDir)...))
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

// TestHarvestWritesOneRunLogToTheStateDir proves each harvest, harvest logs,
// and harvest index run writes exactly one harvest--<unix>.log under the state
// log directory, with private permissions, holding the summary counts, the
// warnings, and each failed session's reason, and prints its path.
func TestHarvestWritesOneRunLogToTheStateDir(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadHarvestRunLogFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			configPath := seedHarvestRunLogSource(t, dir)
			if fixture.SetupHarvest {
				if _, stderr, err := runHarvestWithStateDir(t, dir, configPath, t.TempDir(), nil); err == nil {
					t.Fatalf("setup harvest should fail the malformed session:\n%s", stderr)
				}
			}
			stateDir := t.TempDir()
			stdout, stderr, err := runHarvestWithStateDir(t, dir, configPath, stateDir, fixture.Args)
			if (err != nil) != fixture.WantError {
				t.Fatalf("harvest %v error = %v, want error %v\nstdout:\n%s\nstderr:\n%s", fixture.Args, err, fixture.WantError, stdout, stderr)
			}
			var document map[string]any
			if fixture.JSONStdout {
				if decodeErr := json.Unmarshal([]byte(stdout), &document); decodeErr != nil {
					t.Fatalf("stdout is not one JSON document: %v\n%s", decodeErr, stdout)
				}
			}

			logDir := filepath.Join(stateDir, "peasant", "logs")
			entries, readErr := os.ReadDir(logDir)
			if !fixture.WantLog {
				if readErr == nil && len(entries) > 0 {
					t.Fatalf("harvest %v wrote %d log file(s), want none", fixture.Args, len(entries))
				}
				if strings.Contains(stdout+stderr, "harvest log") {
					t.Fatalf("harvest %v announced a log it should not write:\n%s\n%s", fixture.Args, stdout, stderr)
				}
				return
			}
			if readErr != nil {
				t.Fatalf("read log directory: %v", readErr)
			}
			if len(entries) != 1 {
				t.Fatalf("harvest %v wrote %d files in %s, want exactly one", fixture.Args, len(entries), logDir)
			}
			name := entries[0].Name()
			if !harvestRunLogName.MatchString(name) {
				t.Fatalf("log file name %q does not match harvest--<unix>.log", name)
			}
			path := filepath.Join(logDir, name)
			if fixture.JSONStdout {
				if got := document["runLog"]; got != path {
					t.Fatalf("JSON runLog = %v, want %s\n%s", got, path, stdout)
				}
			} else if !strings.Contains(stdout, "harvest log: "+path+"\n") {
				t.Fatalf("harvest %v did not print the log path %s on stdout:\n%s", fixture.Args, path, stdout)
			}
			if strings.Contains(stderr, path) {
				t.Fatalf("harvest %v printed the log path on stderr:\n%s", fixture.Args, stderr)
			}
			if runtime.GOOS != "windows" {
				info, statErr := os.Stat(path)
				if statErr != nil {
					t.Fatal(statErr)
				}
				if info.Mode().Perm() != 0o600 {
					t.Fatalf("log file mode = %v, want 0600", info.Mode().Perm())
				}
				dirInfo, statErr := os.Stat(logDir)
				if statErr != nil {
					t.Fatal(statErr)
				}
				if dirInfo.Mode().Perm() != 0o700 {
					t.Fatalf("log directory mode = %v, want 0700", dirInfo.Mode().Perm())
				}
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, want := range fixture.LogContains {
				if !strings.Contains(string(body), want) {
					t.Errorf("log does not contain %q:\n%s", want, body)
				}
			}
		})
	}
}

// TestHarvestRunLogHoldsEveryWarningAndIndexFailure proves the log carries each
// warning source the command prints (pipeline diagnostics, the store error,
// discovery diagnostics, index coverage) and names a session the INDEX stage
// could not index with its recorded reason, while a session that failed once
// and then indexed is not reported.
func TestHarvestRunLogHoldsEveryWarningAndIndexFailure(t *testing.T) {
	t.Parallel()
	failedID := ingest.SessionID("bbbbbbbb-0000-4000-8000-000000000001")
	recoveredID := ingest.SessionID("bbbbbbbb-0000-4000-8000-000000000002")
	indexReason := "parse entries: unexpected end of input"
	retryReason := "transient lock"
	result := &ingest.PipelineResult{
		Summary:     ingest.PipelineSummary{New: 2, StoreError: errors.New("store unavailable")},
		Diagnostics: []ingest.DiagnosticEntry{{Location: "/tmp/qa/meta.json", Message: "metadata refused"}},
		DiscoveryDiagnostics: []ingest.DiscoveryDiagnostic{{
			Provider: ingest.HarnessCodex, Location: "/tmp/qa/codex", Summary: "unreadable", Detail: "permission denied",
		}},
		IndexCoverage: &ingest.IndexCoverage{FailedAttempts: 1, Empty: 1},
		IndexLog: []ingest.IndexLogEntry{
			{SessionID: failedID, Harness: ingest.HarnessClaudeCode, Outcome: ingest.IndexOutcomeError, ErrorMessage: &indexReason},
			{SessionID: recoveredID, Harness: ingest.HarnessClaudeCode, Outcome: ingest.IndexOutcomeError, ErrorMessage: &retryReason},
			{SessionID: recoveredID, Harness: ingest.HarnessClaudeCode, Outcome: ingest.IndexOutcomeIndexed},
		},
	}
	var b strings.Builder
	renderHarvestRunLog(&b, result, harvestOutputOptions{mode: harvestIndexOnly})
	body := b.String()
	for _, want := range []string{
		"command: peasant harvest index\n",
		"  index failures: 1\n",
		"warnings: 4\n",
		"  warning: /tmp/qa/meta.json: metadata refused\n",
		"  warning: store unavailable\n",
		"  warning: codex discovery skipped /tmp/qa/codex: unreadable; permission denied\n",
		"  warning: 1 session(s) have no stored entries\n",
		"failed sessions: 1\n",
		"  claude-code " + string(failedID) + ": not indexed: " + indexReason + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("log does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, string(recoveredID)) {
		t.Errorf("log reports a session that indexed on a later attempt:\n%s", body)
	}
}
