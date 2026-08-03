package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/spf13/cobra"
	"zombiezen.com/go/sqlite/sqlitex"
)

// testConfigYAML is a minimal config that disables all source providers.
// Tests that need a specific provider use --source-provider + --source-path
// flags to enable it with a temp dir, preventing accidental ingestion of
// real session data from the developer's home directory.
const testConfigYAML = `version: 1
sources:
  claude-code:
    enabled: false
  opencode:
    enabled: false
  cursor:
    enabled: false
output:
  basePath: ""
`

// writeTestConfigFile writes the minimal all-disabled test config to a known
// path under dir and returns that path. Routing harvest's --config flag at this
// file (instead of mutating XDG_CONFIG_HOME) keeps the helper parallel-safe: no
// process env is touched, so callers run with t.Parallel().
func writeTestConfigFile(t *testing.T, dir string) string {
	t.Helper()
	configPath := filepath.Join(dir, string(defaults.Config.FileName))
	if err := os.WriteFile(configPath, []byte(testConfigYAML), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return configPath
}

// executeHarvestCmd runs the harvest command under a test root with --data-dir,
// --config-dir, and --config all scoped to dir (a single t.TempDir() isolates
// data + config), capturing combined stdout+stderr. args are the harvest
// sub-args; the "harvest" subcommand name is inserted automatically.
//
// Parallel-safe: it injects the dirs + config path per-invocation via flags,
// touching no process env — so callers use t.Parallel() instead of
// t.Setenv("XDG_DATA_HOME"/"XDG_CONFIG_HOME").
//
// The config written under dir disables all source providers, so tests never
// load ambient local configuration or ingest local session data. Tests that need
// a provider re-enable it via --source-provider
// + --source-path; tests that need a bespoke config pass their own --config.
func executeHarvestCmd(t *testing.T, dir string, args []string) (string, error) {
	t.Helper()
	configPath := writeTestConfigFile(t, dir)
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.AddCommand(BuildHarvestCommand())

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{
		"harvest",
		"--data-dir", dir,
		"--config-dir", dir,
		"--config", configPath,
	}, args...))
	err := root.Execute()
	return buf.String(), err
}

// TestHarvestCmd_Flags verifies all expected flags are registered with correct
// names and defaults. No real pipeline I/O occurs.
func TestHarvestCmd_Flags(t *testing.T) {
	t.Parallel()
	cmd := BuildHarvestCommand()

	type flagCheck struct {
		name         string
		defaultValue string
	}

	stringFlags := []flagCheck{
		{"source-provider", ""},
		{"source-path", ""},
		{"output", ""},
		{"since", ""},
	}

	boolFlags := []flagCheck{
		{"dry-run", "false"},
		{"force", "false"},
		{"include-active", "false"},
		{"verbose", "false"},
		{"debug", "false"},
		{defaults.JSONFlagName, "false"},
		{"all", "false"},
		{"detect-commits", "false"},
	}

	sliceFlags := []flagCheck{
		{"session", "[]"},
	}

	for _, fc := range stringFlags {
		f := cmd.Flags().Lookup(fc.name)
		if f == nil {
			t.Errorf("flag --%s not registered", fc.name)
			continue
		}
		if f.DefValue != fc.defaultValue {
			t.Errorf("flag --%s: want default %q, got %q", fc.name, fc.defaultValue, f.DefValue)
		}
	}

	for _, fc := range boolFlags {
		f := cmd.Flags().Lookup(fc.name)
		if f == nil {
			t.Errorf("flag --%s not registered", fc.name)
			continue
		}
		if f.DefValue != fc.defaultValue {
			t.Errorf("flag --%s: want default %q, got %q", fc.name, fc.defaultValue, f.DefValue)
		}
	}

	for _, fc := range sliceFlags {
		f := cmd.Flags().Lookup(fc.name)
		if f == nil {
			t.Errorf("flag --%s not registered", fc.name)
			continue
		}
		if f.DefValue != fc.defaultValue {
			t.Errorf("flag --%s: want default %q, got %q", fc.name, fc.defaultValue, f.DefValue)
		}
	}
}

// TestHarvestCmd_InvalidProvider checks that providing an unrecognized
// --source-provider value produces an appropriate error message.
func TestHarvestCmd_InvalidProvider(t *testing.T) {
	t.Parallel()
	// We need a real path for --source-path to pass the NewResolvedPath check,
	// and a real directory for --output so the pipeline can resolve paths.
	tmpDir := t.TempDir()
	output, err := executeHarvestCmd(t, tmpDir, []string{
		"--source-provider=bogus",
		"--source-path=" + tmpDir,
		"--output=" + tmpDir,
		"--dry-run",
	})
	if err == nil {
		t.Fatalf("expected error for unknown harness, got nil; output: %s", output)
	}
	if !strings.Contains(err.Error(), "unknown harness") {
		t.Errorf("error should mention 'unknown provider', got: %v", err)
	}
}

// TestHarvestCmd_DryRun exercises the --dry-run flag end-to-end.
// With an empty source directory, the pipeline discovers zero sessions.
// We verify: no error, output contains "sessions", and no files are written.
func TestHarvestCmd_DryRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("dry-run with empty source: unexpected error: %v\noutput: %s", err, output)
	}

	if !strings.Contains(output, "sessions") {
		t.Errorf("dry-run output should contain 'sessions', got: %s", output)
	}

	// Output directory should remain empty (no files written).
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatalf("failed to read output dir: %v", err)
	}
	if len(entries) > 0 {
		t.Errorf("dry-run should not write files; found %d entries in output dir", len(entries))
	}
}

// TestHarvestCmd_JSONOutput runs the pipeline with --json and verifies the
// output is valid, well-structured JSON containing expected top-level keys.
func TestHarvestCmd_JSONOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
		"--json",
	})
	if err != nil {
		t.Fatalf("json output with empty source: unexpected error: %v\noutput: %s", err, output)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %q", err, output)
	}

	for _, key := range []string{"summary", "sessions", "duration"} {
		if _, ok := result[key]; !ok {
			t.Errorf("JSON output missing key %q; got: %v", key, result)
		}
	}
}

// TestHarvestCmd_VerboseOutput checks that --verbose produces per-session lines
// (or at minimum does not error) even when no sessions are discovered.
func TestHarvestCmd_VerboseOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
		"--verbose",
	})
	if err != nil {
		t.Fatalf("verbose dry-run: unexpected error: %v\noutput: %s", err, output)
	}

	// With zero sessions the summary line must still appear.
	if !strings.Contains(output, "sessions") {
		t.Errorf("verbose output should contain 'sessions', got: %s", output)
	}
}

// TestHarvestCmd_SourcePathReplaces verifies that --source-path replaces, rather
// than appends to, configured paths. The command must use only the valid override
// and must not consult the default ~/.claude/projects path.
func TestHarvestCmd_SourcePathReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("source-path override: unexpected error: %v\noutput: %s", err, output)
	}
	_ = output
}

// TestHarvestCmd_SourcePathWithoutProvider verifies that passing --source-path
// without --source-provider returns a clear error.
func TestHarvestCmd_SourcePathWithoutProvider(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	_, err := executeHarvestCmd(t, tmpDir, []string{
		"--source-path=" + tmpDir,
		"--dry-run",
	})
	if err == nil {
		t.Fatal("expected error when --source-path given without --source-provider, got nil")
	}
	if !strings.Contains(err.Error(), "--source-path requires --source-provider") {
		t.Errorf("error should mention '--source-path requires --source-provider', got: %v", err)
	}
}

// TestHarvestCmd_SourceProviderWithoutPath verifies that passing --source-provider
// without --source-path returns a clear error.
func TestHarvestCmd_SourceProviderWithoutPath(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	_, err := executeHarvestCmd(t, tmpDir, []string{
		"--source-provider=claude-code",
		"--output=" + tmpDir,
		"--dry-run",
	})
	if err == nil {
		t.Fatal("expected error when --source-provider given without --source-path, got nil")
	}
	if !strings.Contains(err.Error(), "--source-provider requires --source-path") {
		t.Errorf("error should mention '--source-provider requires --source-path', got: %v", err)
	}
}

// TestBuildSourceConfigs_DisabledProvider confirms that a disabled provider is
// not included in the source map returned by buildSourceConfigs.
func TestBuildSourceConfigs_DisabledProvider(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Sources.ClaudeCode.Enabled = false
	cfg.Sources.OpenCode.Enabled = false

	sources := buildSourceConfigs(cfg)
	if len(sources) != 0 {
		t.Errorf("expected 0 sources when all providers disabled, got %d", len(sources))
	}
}

// TestBuildSourceConfigs_EnabledBothProviders verifies that enabled providers
// are both represented in the resulting source map.
func TestBuildSourceConfigs_EnabledBothProviders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg := &config.Config{}
	cfg.Sources.ClaudeCode.Enabled = true
	cfg.Sources.ClaudeCode.Paths = []string{dir}
	cfg.Sources.OpenCode.Enabled = true
	cfg.Sources.OpenCode.Paths = []string{dir}

	sources := buildSourceConfigs(cfg)
	if len(sources) != 2 {
		t.Errorf("expected 2 sources with both providers enabled, got %d", len(sources))
	}
}

// TestDebugFlagHidden verifies the --debug flag is registered but hidden from help output (jw5).
func TestDebugFlagHidden(t *testing.T) {
	t.Parallel()
	cmd := BuildHarvestCommand()

	f := cmd.Flags().Lookup("debug")
	if f == nil {
		t.Fatal("--debug flag not registered")
	}
	if !f.Hidden {
		t.Error("--debug flag should be hidden from help output")
	}
}

// TestPrintSummary_ErrorsInParenthetical verifies printSummary includes errors
// in the parenthetical and active separately (1sq).
func TestPrintSummary_ErrorsInParenthetical(t *testing.T) {
	t.Parallel()
	result := &ingest.PipelineResult{
		Summary: ingest.PipelineSummary{
			New:       3,
			Updated:   2,
			Unchanged: 1,
			Active:    1,
			Errors:    2,
		},
		Duration: 1500 * time.Millisecond,
	}

	var buf bytes.Buffer
	sources := map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{"/tmp/test"}},
	}
	printSummary(&buf, result, false, false, "/tmp/out", "", sources, 0)
	output := buf.String()

	// The format should be: N sessions (X new, Y updated, Z unchanged, W errors), A active (debounced)
	if !strings.Contains(output, "3 new, 2 updated, 1 unchanged, 2 errors)") {
		t.Errorf("errors should be in parenthetical; got: %s", output)
	}
	if !strings.Contains(output, "1 active (debounced)") {
		t.Errorf("active should be shown separately as 'N active (debounced)'; got: %s", output)
	}
	// Total should be 3+2+1+1+2 = 9
	if !strings.Contains(output, "9 sessions") {
		t.Errorf("total should be 9 sessions; got: %s", output)
	}
}

// TestPrintSummary_IncludeActiveLabel verifies the dynamic active-session label.
func TestPrintSummary_IncludeActiveLabel(t *testing.T) {
	t.Parallel()
	result := &ingest.PipelineResult{
		Summary:  ingest.PipelineSummary{Active: 2},
		Duration: 100 * time.Millisecond,
	}
	sources := map[ingest.Harness]ingest.SourceConfig{}

	var buf bytes.Buffer
	printSummary(&buf, result, false, false, "/out", "", sources, 0)
	if !strings.Contains(buf.String(), "(debounced)") {
		t.Errorf("includeActive=false should show '(debounced)'; got: %s", buf.String())
	}

	buf.Reset()
	printSummary(&buf, result, false, true, "/out", "", sources, 0)
	if !strings.Contains(buf.String(), "(included)") {
		t.Errorf("includeActive=true should show '(included)'; got: %s", buf.String())
	}
}

// TestPrintSummary_DefaultShowsProviderAndPath verifies that default mode shows
// provider and output path, with subagent count suffix for sessions that have subagents.
func TestPrintSummary_DefaultShowsProviderAndPath(t *testing.T) {
	t.Parallel()
	parentID := ingest.SessionID("session-abc123")
	result := &ingest.PipelineResult{
		Summary:  ingest.PipelineSummary{New: 1},
		Duration: 100 * time.Millisecond,
		Sessions: []ingest.SessionResult{
			{
				SessionID:  "session-abc123",
				Harness:    ingest.HarnessClaudeCode,
				Status:     ingest.DiffNew,
				OutputPath: "/output/slug/session-abc123",
			},
			{
				SessionID:  "subagent-001",
				Harness:    ingest.HarnessClaudeCode,
				ParentUUID: &parentID,
				Status:     ingest.DiffNew,
				OutputPath: "/output/slug/session-abc123/subagents/subagent-001",
			},
		},
	}
	sources := map[ingest.Harness]ingest.SourceConfig{}

	var buf bytes.Buffer
	printSummary(&buf, result, false, false, "/output", "", sources, 0)
	output := buf.String()

	if !strings.Contains(output, string(defaults.HarnessClaudeCode)) {
		t.Errorf("default mode should show provider column; got: %s", output)
	}
	if !strings.Contains(output, "/output/slug/session-abc123") {
		t.Errorf("default mode should show output path; got: %s", output)
	}
	if !strings.Contains(output, "+ 1 subagent(s)") {
		t.Errorf("default mode should show subagent suffix; got: %s", output)
	}
	// Subagent should not appear as its own root row.
	if strings.Contains(output, "subagent-001  ->") {
		t.Errorf("default mode should not expand subagents; got: %s", output)
	}
}

// TestPrintSummary_VerboseExpandsSubagents verifies that verbose mode expands
// subagents underneath their parent instead of collapsing them.
func TestPrintSummary_VerboseExpandsSubagents(t *testing.T) {
	t.Parallel()
	parentID := ingest.SessionID("session-abc123")
	result := &ingest.PipelineResult{
		Summary:  ingest.PipelineSummary{New: 1},
		Duration: 100 * time.Millisecond,
		Sessions: []ingest.SessionResult{
			{
				SessionID:  "session-abc123",
				Harness:    ingest.HarnessClaudeCode,
				Status:     ingest.DiffNew,
				OutputPath: "/output/slug/session-abc123",
			},
			{
				SessionID:  "subagent-001",
				Harness:    ingest.HarnessClaudeCode,
				ParentUUID: &parentID,
				Status:     ingest.DiffNew,
				OutputPath: "/output/slug/session-abc123/subagents/subagent-001",
			},
			{
				SessionID:  "subagent-002",
				Harness:    ingest.HarnessClaudeCode,
				ParentUUID: &parentID,
				Status:     ingest.DiffNew,
				OutputPath: "/output/slug/session-abc123/subagents/subagent-002",
			},
		},
	}
	sources := map[ingest.Harness]ingest.SourceConfig{}

	var buf bytes.Buffer
	printSummary(&buf, result, true, false, "/output", "", sources, 0)
	output := buf.String()

	// Root session should appear.
	if !strings.Contains(output, "session-abc123  ->  /output/slug/session-abc123") {
		t.Errorf("verbose should show root session with path; got: %s", output)
	}
	// Subagents should be expanded (not collapsed with suffix).
	if !strings.Contains(output, "subagent-001") {
		t.Errorf("verbose should expand subagent-001; got: %s", output)
	}
	if !strings.Contains(output, "subagent-002") {
		t.Errorf("verbose should expand subagent-002; got: %s", output)
	}
	// No "+ N subagent(s)" suffix in verbose mode.
	if strings.Contains(output, "subagent(s)") {
		t.Errorf("verbose should not show collapsed subagent suffix; got: %s", output)
	}
}

// TestPrintSummary_Truncation verifies that when more than 20 root sessions,
// output is truncated to first 10 + "... and N more ..." + last 10.
func TestPrintSummary_Truncation(t *testing.T) {
	t.Parallel()
	// Build 25 root sessions.
	sessions := make([]ingest.SessionResult, 25)
	for i := range sessions {
		sessions[i] = ingest.SessionResult{
			SessionID:  ingest.SessionID(fmt.Sprintf("session-%02d", i)),
			Harness:    ingest.HarnessClaudeCode,
			Status:     ingest.DiffNew,
			OutputPath: fmt.Sprintf("/out/session-%02d", i),
		}
	}
	result := &ingest.PipelineResult{
		Summary:  ingest.PipelineSummary{New: 25},
		Duration: 100 * time.Millisecond,
		Sessions: sessions,
	}
	sources := map[ingest.Harness]ingest.SourceConfig{}

	var buf bytes.Buffer
	printSummary(&buf, result, false, false, "/out", "", sources, 0)
	output := buf.String()

	// First 10 should appear.
	if !strings.Contains(output, "session-00") {
		t.Errorf("truncation: first session should appear; got: %s", output)
	}
	if !strings.Contains(output, "session-09") {
		t.Errorf("truncation: 10th session should appear; got: %s", output)
	}
	// Middle sessions (10-14) should NOT appear.
	if strings.Contains(output, "session-10") {
		t.Errorf("truncation: session-10 should be hidden; got: %s", output)
	}
	// Truncation marker: 25 - 20 = 5 more.
	if !strings.Contains(output, "... and 5 more ...") {
		t.Errorf("truncation: should show '... and 5 more ...'; got: %s", output)
	}
	// Last 10 should appear.
	if !strings.Contains(output, "session-24") {
		t.Errorf("truncation: last session should appear; got: %s", output)
	}
	if !strings.Contains(output, "session-15") {
		t.Errorf("truncation: 16th session should appear in last 10; got: %s", output)
	}
}

// TestPrintSummary_TruncationBoundary20 verifies that exactly 20 root sessions
// are shown without truncation (boundary: truncThreshold = 20).
func TestPrintSummary_TruncationBoundary20(t *testing.T) {
	t.Parallel()
	sessions := make([]ingest.SessionResult, 20)
	for i := range sessions {
		id := fmt.Sprintf("session-%03d", i+1)
		sessions[i] = ingest.SessionResult{
			SessionID:  ingest.SessionID(id),
			Harness:    ingest.HarnessClaudeCode,
			Status:     ingest.DiffNew,
			OutputPath: fmt.Sprintf("/output/slug/%s", id),
		}
	}
	result := &ingest.PipelineResult{
		Summary:  ingest.PipelineSummary{New: 20},
		Duration: 100 * time.Millisecond,
		Sessions: sessions,
	}
	sources := map[ingest.Harness]ingest.SourceConfig{}

	var buf bytes.Buffer
	printSummary(&buf, result, false, false, "/output", "", sources, 0)
	output := buf.String()

	// Should NOT truncate at exactly 20.
	if strings.Contains(output, "... and") {
		t.Errorf("20 sessions should not be truncated; got: %s", output)
	}
	// All 20 session IDs must appear.
	for i := 1; i <= 20; i++ {
		id := fmt.Sprintf("session-%03d", i)
		if !strings.Contains(output, id) {
			t.Errorf("session %s should appear; got: %s", id, output)
		}
	}
}

// TestPrintSummary_TruncationBoundary21 verifies that exactly 21 root sessions
// triggers minimum truncation: first 10 + "... and 1 more ..." + last 10.
func TestPrintSummary_TruncationBoundary21(t *testing.T) {
	t.Parallel()
	sessions := make([]ingest.SessionResult, 21)
	for i := range sessions {
		id := fmt.Sprintf("session-%03d", i+1)
		sessions[i] = ingest.SessionResult{
			SessionID:  ingest.SessionID(id),
			Harness:    ingest.HarnessClaudeCode,
			Status:     ingest.DiffNew,
			OutputPath: fmt.Sprintf("/output/slug/%s", id),
		}
	}
	result := &ingest.PipelineResult{
		Summary:  ingest.PipelineSummary{New: 21},
		Duration: 100 * time.Millisecond,
		Sessions: sessions,
	}
	sources := map[ingest.Harness]ingest.SourceConfig{}

	var buf bytes.Buffer
	printSummary(&buf, result, false, false, "/output", "", sources, 0)
	output := buf.String()

	// Exactly 1 session truncated (21 - 2*10 = 1).
	if !strings.Contains(output, "... and 1 more ...") {
		t.Errorf("21 sessions should show '... and 1 more ...'; got: %s", output)
	}
	// First 10 sessions must appear (session-001 through session-010).
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("session-%03d", i)
		if !strings.Contains(output, id) {
			t.Errorf("session %s (first 10) should appear; got: %s", id, output)
		}
	}
	// Last 10 sessions must appear (session-012 through session-021).
	for i := 12; i <= 21; i++ {
		id := fmt.Sprintf("session-%03d", i)
		if !strings.Contains(output, id) {
			t.Errorf("session %s (last 10) should appear; got: %s", id, output)
		}
	}
	// session-011 must NOT appear (the one truncated).
	if strings.Contains(output, "session-011") {
		t.Errorf("session-011 should be truncated (hidden); got: %s", output)
	}
}

// TestHarvestCmd_DryRun_CustomPatternCount verifies that --dry-run output includes
// the count of custom patterns loaded from config.
func TestHarvestCmd_DryRun_CustomPatternCount(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	configYAML := fmt.Sprintf(`version: 1
redaction:
  level: minimal
  custom_patterns:
    - id: pat-a
      category: secrets
      pattern: "SECRET-[A-Z]+"
      replacement: "<REDACTED>"
    - id: pat-b
      category: pii
      pattern: "[0-9]{3}-[0-9]{2}-[0-9]{4}"
      replacement: "<SSN>"
sources:
  claude-code:
    enabled: true
    paths:
      - %s
output:
  basePath: %s
`, sourceDir, outputDir)

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	output, err := executeHarvestCmd(t, dir, []string{
		"--config=" + configPath,
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("dry-run with custom patterns: unexpected error: %v\noutput: %s", err, output)
	}

	if !strings.Contains(output, "Custom patterns: 2") {
		t.Errorf("dry-run output should contain 'Custom patterns: 2', got: %s", output)
	}
}

// TestHarvestCmd_AllImpliesIncludeActive verifies that --all implies --include-active,
// which is observable in the summary output: active sessions are labeled "(included)"
// instead of "(debounced)" when --include-active is in effect.
func TestHarvestCmd_AllImpliesIncludeActive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	// Run with --all (which should imply --include-active and --force).
	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
		"--all",
	})
	if err != nil {
		t.Fatalf("--all dry-run: unexpected error: %v\noutput: %s", err, output)
	}

	// With --all implying --include-active, printSummary should use "(included)"
	// instead of the default "(debounced)" label for active sessions.
	if !strings.Contains(output, "(included)") {
		t.Errorf("--all should imply --include-active, expected '(included)' in output; got: %s", output)
	}
	if strings.Contains(output, "(debounced)") {
		t.Errorf("--all should imply --include-active, unexpected '(debounced)' in output; got: %s", output)
	}
}

// TestHarvestCmd_DryRun_DoesNotCreateDB verifies that --dry-run does NOT create
// the analytics database file. Uses --data-dir (via the helper) so we can assert
// on a known path without touching the real data directory.
func TestHarvestCmd_DryRun_DoesNotCreateDB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("dry-run should succeed; got error: %v\noutput: %s", err, output)
	}

	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if _, err := os.Stat(dbPath); err == nil {
		t.Errorf("dry-run should NOT create DB file at %s", dbPath)
	}
}

// TestHarvestCmd_NonDryRun_CreatesDB verifies that a non-dry-run ingest creates
// the analytics database file. An empty source directory yields 0 sessions
// (which is fine — the store should still be opened and the file created).
func TestHarvestCmd_NonDryRun_CreatesDB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
	})
	if err != nil {
		t.Fatalf("non-dry-run should succeed; got error: %v\noutput: %s", err, output)
	}

	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Errorf("non-dry-run should create DB file at %s, but it does not exist", dbPath)
	}
}

// TestHarvestVerify_AnnotationEngineSection verifies that 'ingest verify' shows
// the Annotation Engine subsection within section 4 (Table Statistics) after
// a DB is created by a non-dry-run ingest. The subsection must include:
// seed data counts with [OK]/[FAIL], taxonomy chain check, and annotation counts.
// Also verifies the schema version string is updated to v1-v16.
// This output-only derivative test runs the harvest pipeline and
// provisions a SQLite DB just to assert rendered output for one flag/condition
// (base vs --verbose vs seed-failure). Rework the AnnotationEngineSection
// variants onto a SINGLE shared setup that stubs the pipeline/DB provisioning,
// so only the formatter input varies instead of re-provisioning per test.
func TestHarvestVerify_AnnotationEngineSection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	// Step 1: Non-dry-run ingest creates DB with migrations + seed data.
	_, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
	})
	if err != nil {
		t.Fatalf("non-dry-run ingest: unexpected error: %v", err)
	}

	// Step 2: Run verify and capture output.
	output, err := executeHarvestCmd(t, dir, []string{"verify"})
	if err != nil {
		t.Fatalf("ingest verify: unexpected error: %v\noutput: %s", err, output)
	}

	// Schema version updated to v1-v16.
	if !strings.Contains(output, "v1-v20 (migrations)") {
		t.Errorf("verify output should show 'v1-v20 (migrations)' schema version\noutput: %s", output)
	}

	// Annotation Engine subsection header present (subsection of section 4).
	if !strings.Contains(output, "Annotation Engine:") {
		t.Errorf("verify output missing 'Annotation Engine:' subsection header\noutput: %s", output)
	}

	// Seed data counts with [OK] markers.
	if !strings.Contains(output, "annotation_types:") {
		t.Errorf("verify output missing 'annotation_types:' count line\noutput: %s", output)
	}
	if !strings.Contains(output, "annotators:") {
		t.Errorf("verify output missing 'annotators:' count line\noutput: %s", output)
	}
	if !strings.Contains(output, "annotation_type_deps:") {
		t.Errorf("verify output missing 'annotation_type_deps:' count line\noutput: %s", output)
	}
	if !strings.Contains(output, "[OK]") {
		t.Errorf("verify output missing '[OK]' markers in seed verification\noutput: %s", output)
	}

	// Taxonomy chain check.
	if !strings.Contains(output, "taxonomy chain:") {
		t.Errorf("verify output missing 'taxonomy chain:' check line\noutput: %s", output)
	}
	if !strings.Contains(output, "valid family") {
		t.Errorf("verify output missing valid family→class chain message\noutput: %s", output)
	}

	// Annotation count by target kind.
	// The verify command outputs one of two forms depending on whether annotations exist:
	//   "annotations by kind: (none yet...)"  — no annotations (empty DB or no sessions)
	//   "annotations (<kind>): <count>"       — annotations present (classifiers ran)
	// An OR-condition is required here because the test is not fully isolated: the cobra
	// default for --config is derived from defaults.Config.FilePath, which is evaluated at
	// package init time using the ambient XDG_CONFIG_HOME — before any t.Setenv call. If the
	// local config.yaml has other providers (e.g. opencode) enabled, those sessions
	// are ingested into the temp DB and the classifier produces annotation rows, making the
	// "none yet" path unreachable. The section's presence (either form) is the observable
	// invariant we can reliably assert on.
	hasAnnotationsByKind := strings.Contains(output, "annotations by kind:")
	hasAnnotationsByTarget := strings.Contains(output, "annotations (")
	if !hasAnnotationsByKind && !hasAnnotationsByTarget {
		t.Errorf("verify output missing annotation count section (expected 'annotations by kind:' or 'annotations (<kind>):')\noutput: %s", output)
	}

	// Sample Data stays at section 5 (no renumbering).
	// (It only appears with --verbose, so verify it's NOT numbered 6 here.)
}

// TestHarvestVerify_AnnotationEngineSection_Verbose verifies that 'ingest verify --verbose'
// shows per-type details (type_id, status, class/family) and per-annotator details,
// and that Sample Data stays at section 5 (not renumbered).
// This output-only derivative test runs the harvest pipeline and
// provisions a SQLite DB just to assert rendered output for one flag/condition
// (base vs --verbose vs seed-failure). Rework the AnnotationEngineSection
// variants onto a SINGLE shared setup that stubs the pipeline/DB provisioning,
// so only the formatter input varies instead of re-provisioning per test.
func TestHarvestVerify_AnnotationEngineSection_Verbose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	// Create DB with seed data.
	_, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
	})
	if err != nil {
		t.Fatalf("non-dry-run ingest: unexpected error: %v", err)
	}

	// Run verify --verbose and capture output.
	output, err := executeHarvestCmd(t, dir, []string{"verify", "--verbose"})
	if err != nil {
		t.Fatalf("ingest verify --verbose: unexpected error: %v\noutput: %s", err, output)
	}

	// Annotation engine subsection still present within section 4.
	if !strings.Contains(output, "Annotation Engine:") {
		t.Errorf("verbose verify missing 'Annotation Engine:' subsection\noutput: %s", output)
	}

	// Per-type verbose details section header.
	if !strings.Contains(output, "Annotation Type Details") {
		t.Errorf("verbose verify missing 'Annotation Type Details' header\noutput: %s", output)
	}

	// Seed type_ids should appear in verbose output.
	for _, typeID := range []string{
		"quality.session_approval",
		"quality.session_outcome",
		"quality.user_frustration",
		"metadata.session_scope",
	} {
		if !strings.Contains(output, typeID) {
			t.Errorf("verbose verify missing type_id %q\noutput: %s", typeID, output)
		}
	}

	// Annotators verbose section present.
	if !strings.Contains(output, "Annotators (--verbose):") {
		t.Errorf("verbose verify missing 'Annotators (--verbose):' section\noutput: %s", output)
	}

	// Seed annotator names should appear.
	for _, name := range []string{"outcome-classifier", "frustration-classifier", "scope-classifier"} {
		if !strings.Contains(output, name) {
			t.Errorf("verbose verify missing annotator %q\noutput: %s", name, output)
		}
	}

	// Sample Data stays at section 5 (not renumbered to 6).
	if !strings.Contains(output, "5. Sample Data") {
		t.Errorf("verbose verify: Sample Data should still be '5. Sample Data'\noutput: %s", output)
	}
	if strings.Contains(output, "6. Sample Data") {
		t.Errorf("verbose verify: Sample Data should NOT be renumbered to '6. Sample Data'\noutput: %s", output)
	}
}

// TestHarvestCmd_NonDryRun_WiresV2Stages verifies that non-dry-run ingest
// opens the DB and creates it (the v2 stages are wired but don't fail
// on an empty source directory — they simply produce 0 indexed/computed).
func TestHarvestCmd_NonDryRun_WiresV2Stages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--json",
	})
	if err != nil {
		t.Fatalf("non-dry-run with v2 stages: unexpected error: %v\noutput: %s", err, output)
	}

	// Parse JSON output and verify indexed/computed are present (both 0 for empty source).
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, output)
	}
	summary, ok := result["summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected 'summary' to be object; got: %v", result["summary"])
	}
	// indexed and computed should be present in JSON (even if 0).
	if _, ok := summary["Indexed"]; !ok {
		t.Error("JSON summary missing 'Indexed' field")
	}
	if _, ok := summary["Computed"]; !ok {
		t.Error("JSON summary missing 'Computed' field")
	}
}

// TestParseSinceDuration_Valid verifies that valid duration strings are parsed correctly.
func TestParseSinceDuration_Valid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		wantDays int // approximate days in the past (±1 for month rounding)
	}{
		{"7d", 7},
		{"1d", 1},
		{"30d", 30},
		{"2w", 14},
		{"1w", 7},
		{"4w", 28},
		{"3m", 90}, // approximate
		{"1m", 30}, // approximate
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			cutoff, err := parseSinceDuration(tc.input)
			if err != nil {
				t.Fatalf("parseSinceDuration(%q) returned error: %v", tc.input, err)
			}
			elapsed := time.Since(cutoff)
			elapsedDays := int(elapsed.Hours() / 24)
			// Allow ±4 days tolerance for month boundaries (Feb=28, months vary).
			if elapsedDays < tc.wantDays-4 || elapsedDays > tc.wantDays+4 {
				t.Errorf("parseSinceDuration(%q): want ~%d days ago, got ~%d days ago",
					tc.input, tc.wantDays, elapsedDays)
			}
		})
	}
}

// TestParseSinceDuration_Invalid verifies that invalid duration strings are rejected.
func TestParseSinceDuration_Invalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input   string
		wantMsg string
	}{
		{"", "invalid duration"},
		{"d", "invalid duration"},
		{"abc", "positive integer"},
		{"0d", "positive integer"},
		{"-1w", "positive integer"},
		{"2x", "invalid duration unit"},
		{"2y", "invalid duration unit"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			_, err := parseSinceDuration(tc.input)
			if err == nil {
				t.Fatalf("parseSinceDuration(%q) should return error", tc.input)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("parseSinceDuration(%q) error = %q, want containing %q",
					tc.input, err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestHarvestCmd_SessionFlag verifies --session flag is registered and accepts session IDs.
func TestHarvestCmd_SessionFlag(t *testing.T) {
	t.Parallel()
	cmd := BuildHarvestCommand()

	f := cmd.Flags().Lookup("session")
	if f == nil {
		t.Fatal("--session flag not registered")
	}
	if f.DefValue != "[]" {
		t.Errorf("--session default should be '[]', got %q", f.DefValue)
	}
}

// TestHarvestCmd_SinceFlag verifies --since flag is registered with correct default.
func TestHarvestCmd_SinceFlag(t *testing.T) {
	t.Parallel()
	cmd := BuildHarvestCommand()

	f := cmd.Flags().Lookup("since")
	if f == nil {
		t.Fatal("--since flag not registered")
	}
	if f.DefValue != "" {
		t.Errorf("--since default should be empty, got %q", f.DefValue)
	}
}

// TestHarvestCmd_SessionFlag_InvalidID verifies that an invalid --session ID produces an error.
func TestHarvestCmd_SessionFlag_InvalidID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	_, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
		"--session=not a valid session id!!!",
	})
	if err == nil {
		t.Fatal("expected error for invalid session ID, got nil")
	}
	if !strings.Contains(err.Error(), "invalid session ID") {
		t.Errorf("error should mention 'invalid session ID', got: %v", err)
	}
}

// TestHarvestCmd_SinceFlag_InvalidDuration verifies that an invalid --since value produces an error.
func TestHarvestCmd_SinceFlag_InvalidDuration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	_, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
		"--since=xyz",
	})
	if err == nil {
		t.Fatal("expected error for invalid --since value, got nil")
	}
	if !strings.Contains(err.Error(), "invalid duration") {
		t.Errorf("error should mention 'invalid duration', got: %v", err)
	}
}

// TestHarvestCmd_SinceFlag_ValidDuration verifies that a valid --since value does not error.
func TestHarvestCmd_SinceFlag_ValidDuration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	output, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
		"--dry-run",
		"--since=2w",
	})
	if err != nil {
		t.Fatalf("valid --since=2w should not error: %v\noutput: %s", err, output)
	}
}

// --- selection filter tests ---
// These drive buildSelectionFilterWithRecorder, the closure runHarvest itself
// builds. Cases that do not exercise a withheld conflict discard the recorder
// explicitly here, so the discard is visible in the test that chooses it.

// TestBuildSelectionFilter_ProviderNotInSelection verifies that a session from an
// unselected provider is rejected.
func TestBuildSelectionFilter_ProviderNotInSelection(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Selection.Mode = config.SelectionModeSelected
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		string(defaults.HarnessClaudeCode): {},
	}

	filter, _ := buildSelectionFilterWithRecorder(cfg, nil)
	session := ingest.DiscoveredSession{
		SessionID: "sess-1",
		Harness:   ingest.HarnessOpenCode,
	}
	if filter(session) {
		t.Error("filter should reject session from unselected provider")
	}
}

// TestBuildSelectionFilter_ProviderImportAll verifies that a provider with no projects
// and no sessions passes all sessions through (import-all semantics).
func TestBuildSelectionFilter_ProviderImportAll(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Selection.Mode = config.SelectionModeSelected
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		string(defaults.HarnessClaudeCode): {}, // empty = import all
	}

	filter, _ := buildSelectionFilterWithRecorder(cfg, nil)
	session := ingest.DiscoveredSession{
		SessionID:   "any-session",
		Harness:     ingest.HarnessClaudeCode,
		ProjectName: "anything",
	}
	if !filter(session) {
		t.Error("filter should pass all sessions for import-all provider")
	}
}

// TestBuildSelectionFilter_ExplicitSessionID verifies matching by explicit session ID.
func TestBuildSelectionFilter_ExplicitSessionID(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Selection.Mode = config.SelectionModeSelected
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		string(defaults.HarnessClaudeCode): {
			Sessions: []string{"target-sess"},
		},
	}

	filter, _ := buildSelectionFilterWithRecorder(cfg, nil)

	// Matching session ID should pass.
	if !filter(ingest.DiscoveredSession{
		SessionID: "target-sess",
		Harness:   ingest.HarnessClaudeCode,
	}) {
		t.Error("filter should pass explicitly listed session ID")
	}

	// Non-matching session ID should fail.
	if filter(ingest.DiscoveredSession{
		SessionID: "other-sess",
		Harness:   ingest.HarnessClaudeCode,
	}) {
		t.Error("filter should reject session not in allowlist")
	}
}

// TestBuildSelectionFilter_ProjectMatchByName verifies matching by project name
// when git resolver is nil.
func TestBuildSelectionFilter_ProjectMatchByName(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Selection.Mode = config.SelectionModeSelected
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		string(defaults.HarnessClaudeCode): {
			Projects: []config.ProjectSelection{
				{Name: "my-project"},
			},
		},
	}

	filter, _ := buildSelectionFilterWithRecorder(cfg, nil)

	if !filter(ingest.DiscoveredSession{
		SessionID:   "sess-1",
		Harness:     ingest.HarnessClaudeCode,
		ProjectName: "my-project",
	}) {
		t.Error("filter should pass session matching project by name")
	}

	if filter(ingest.DiscoveredSession{
		SessionID:   "sess-2",
		Harness:     ingest.HarnessClaudeCode,
		ProjectName: "other-project",
	}) {
		t.Error("filter should reject session from different project")
	}
}

// TestBuildSelectionFilter_ProjectMatchByGitRemote_MixedFormsNormalize
// regression-locks the selection-matching fix at the ingest layer: a configured gitRemote
// rule and the git resolver's raw remote for the same repo can be in
// DIFFERENT forms (here: HTTPS config vs the local clone's SSH remote) and
// must still be recognized as the same project.
func TestBuildSelectionFilter_ProjectMatchByGitRemote_MixedFormsNormalize(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Selection.Mode = config.SelectionModeSelected
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		string(defaults.HarnessClaudeCode): {
			Projects: []config.ProjectSelection{
				{GitRemote: "https://github.com/example-org/garden-app.git"},
			},
		},
	}

	stubGit := &stubBranchGitResolver{remote: "git@github.com:example-org/garden-app.git"}
	filter, _ := buildSelectionFilterWithRecorder(cfg, stubGit)

	if !filter(ingest.DiscoveredSession{
		SessionID:  "sess-1",
		Harness:    ingest.HarnessClaudeCode,
		SourcePath: "/tmp/test/session.json",
	}) {
		t.Error("filter should pass a session whose local SSH remote names the same repo as an HTTPS-form config rule")
	}

	// A different repo, in the same SSH form family, must not match.
	stubGitOther := &stubBranchGitResolver{remote: "git@github.com:example-org/other-repo.git"}
	filterOther, _ := buildSelectionFilterWithRecorder(cfg, stubGitOther)
	if filterOther(ingest.DiscoveredSession{
		SessionID:  "sess-2",
		Harness:    ingest.HarnessClaudeCode,
		SourcePath: "/tmp/test/session2.json",
	}) {
		t.Error("filter should reject a session whose remote names a different repo")
	}
}

// TestBuildSelectionFilter_BranchFilter verifies branch-level filtering with
// AutoIngestNewBranches disabled.
func TestBuildSelectionFilter_BranchFilter(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Selection.Mode = config.SelectionModeSelected
	cfg.Selection.AutoIngestNewBranches = false
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		string(defaults.HarnessClaudeCode): {
			Projects: []config.ProjectSelection{
				{
					Name:     "my-project",
					Branches: []string{"main", "develop"},
				},
			},
		},
	}

	filter, _ := buildSelectionFilterWithRecorder(cfg, nil)

	// Without a git resolver, branch cannot be resolved, so even a matching project
	// with branch filtering won't pass (branch is empty, not in allowlist).
	if filter(ingest.DiscoveredSession{
		SessionID:   "sess-1",
		Harness:     ingest.HarnessClaudeCode,
		ProjectName: "my-project",
	}) {
		t.Error("filter should reject when branch cannot be resolved and auto-ingest is off")
	}
}

// TestBuildSelectionFilter_AutoIngestNewBranches verifies that when enabled,
// sessions from matched projects pass even if their branch isn't in the allowlist.
func TestBuildSelectionFilter_AutoIngestNewBranches(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Selection.Mode = config.SelectionModeSelected
	cfg.Selection.AutoIngestNewBranches = true
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		string(defaults.HarnessOpenCode): {
			Projects: []config.ProjectSelection{
				{
					Name:     "my-project",
					Branches: []string{"main"},
				},
			},
		},
	}

	// Use a stub git resolver that returns a "feature" branch (not in allowlist).
	stubGit := &stubBranchGitResolver{branch: "feature-xyz"}
	filter, _ := buildSelectionFilterWithRecorder(cfg, stubGit)

	session := ingest.DiscoveredSession{
		SessionID:   "sess-1",
		Harness:     ingest.HarnessOpenCode,
		SourcePath:  "/tmp/test/session.json",
		ProjectName: "my-project",
	}
	if !filter(session) {
		t.Error("filter should pass new branch when AutoIngestNewBranches is true")
	}

	// Disable auto-ingest: same session should now be rejected.
	cfg.Selection.AutoIngestNewBranches = false
	filter2, _ := buildSelectionFilterWithRecorder(cfg, stubGit)
	if filter2(session) {
		t.Error("filter should reject new branch when AutoIngestNewBranches is false")
	}
}

// stubBranchGitResolver is a minimal GitResolver for filter tests.
type stubBranchGitResolver struct {
	remote string
	branch string
}

func (s *stubBranchGitResolver) RemoteURL(_ context.Context, _ string) (string, error) {
	if s.remote != "" {
		return s.remote, nil
	}
	return "", fmt.Errorf("no remote")
}

func (s *stubBranchGitResolver) Branch(_ context.Context, _ string) (string, error) {
	if s.branch != "" {
		return s.branch, nil
	}
	return "", fmt.Errorf("no branch")
}

func (s *stubBranchGitResolver) Worktree(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("not implemented")
}

func (s *stubBranchGitResolver) TrackingBranch(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("not implemented")
}

func (s *stubBranchGitResolver) UserEmail(_ context.Context) (string, error) {
	return "", fmt.Errorf("not implemented")
}

func (s *stubBranchGitResolver) WalkUpRemoteURL(_ context.Context, _ string) (string, string, error) {
	if s.remote != "" {
		return s.remote, "", nil
	}
	return "", "", nil
}

// TestBuildSelectionFilter_ProjectMatchByGitRemote verifies matching by git remote URL.
func TestBuildSelectionFilter_ProjectMatchByGitRemote(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Selection.Mode = config.SelectionModeSelected
	cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
		string(defaults.HarnessOpenCode): {
			Projects: []config.ProjectSelection{
				{GitRemote: "https://github.com/org/repo.git"},
			},
		},
	}

	stubGit := &stubBranchGitResolver{remote: "https://github.com/org/repo.git"}
	filter, _ := buildSelectionFilterWithRecorder(cfg, stubGit)

	if !filter(ingest.DiscoveredSession{
		SessionID:  "sess-1",
		Harness:    ingest.HarnessOpenCode,
		SourcePath: "/tmp/test/session.json",
	}) {
		t.Error("filter should pass session matching project by git remote")
	}

	// Different remote should not match.
	stubGit2 := &stubBranchGitResolver{remote: "https://github.com/org/other-repo.git"}
	filter2, _ := buildSelectionFilterWithRecorder(cfg, stubGit2)
	if filter2(ingest.DiscoveredSession{
		SessionID:  "sess-2",
		Harness:    ingest.HarnessOpenCode,
		SourcePath: "/tmp/test/session.json",
	}) {
		t.Error("filter should reject session from different git remote")
	}
}

// TestHarvestVerify_AnnotationEngineSection_SeedFail verifies that 'ingest verify' shows
// [FAIL] in the annotation_types seed-count line when a seed row is missing.
// The check is >= 11 (11 seed types ship via migrations, incl. the user.custom_label
// type seeded by V36 and the quality.turn_outcome/quality.turn_flag pair seeded by
// V39); deleting 1 row drops the count to 10 < 11 → [FAIL].
// This output-only derivative test runs the harvest pipeline and
// provisions a SQLite DB just to assert rendered output for one flag/condition
// (base vs --verbose vs seed-failure). Rework the AnnotationEngineSection
// variants onto a SINGLE shared setup that stubs the pipeline/DB provisioning,
// so only the formatter input varies instead of re-provisioning per test.
func TestHarvestVerify_AnnotationEngineSection_SeedFail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()

	// Step 1: Create DB with seed data via a non-dry-run ingest.
	_, err := executeHarvestCmd(t, dir, []string{
		"--source-provider=claude-code",
		"--source-path=" + sourceDir,
		"--output=" + outputDir,
	})
	if err != nil {
		t.Fatalf("non-dry-run ingest: unexpected error: %v", err)
	}

	// Step 2: Delete one seed annotation_type row to drop count below the >= 9 threshold.
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("open pool for seed deletion: %v", err)
	}
	conn, err := pool.Take(context.Background())
	if err != nil {
		_ = pool.Close()
		t.Fatalf("take conn for seed deletion: %v", err)
	}
	delErr := sqlitex.ExecuteTransient(conn,
		`DELETE FROM annotation_types WHERE type_id = 'quality.session_approval'`, nil)
	pool.Put(conn)
	if closeErr := pool.Close(); closeErr != nil {
		t.Logf("pool close: %v", closeErr)
	}
	if delErr != nil {
		t.Fatalf("delete seed row: %v", delErr)
	}

	// Step 3: Run verify — seed mismatch is non-fatal (shown inline, not as error return).
	output, _ := executeHarvestCmd(t, dir, []string{"verify"})

	// Step 4: The annotation_types line should now show [FAIL].
	if !strings.Contains(output, "[FAIL]") {
		t.Errorf("verify output should contain '[FAIL]' after seed deletion\noutput: %s", output)
	}
}

// TestIsolateSourceProvider verifies that --source-path scoping
// makes the NAMED provider the sole active source: it enables that provider and
// disables default discovery of the others, so a path-scoped ingest never reads
// the other providers' real default dirs (~/.claude, opencode, codex).
func TestIsolateSourceProvider(t *testing.T) {
	t.Parallel()
	cases := []struct {
		named   defaults.Harness
		wantCC  bool
		wantOC  bool
		wantCdx bool
	}{
		{defaults.HarnessClaudeCode, true, false, false},
		{defaults.HarnessOpenCode, false, true, false},
		{defaults.HarnessCodex, false, false, true},
	}
	for _, tc := range cases {
		// Start with ALL providers enabled (the leak scenario the fix prevents).
		cfg := &config.Config{}
		cfg.Sources.ClaudeCode.Enabled = true
		cfg.Sources.OpenCode.Enabled = true
		cfg.Sources.Codex.Enabled = true

		isolateSourceProvider(cfg, tc.named)

		if cfg.Sources.ClaudeCode.Enabled != tc.wantCC ||
			cfg.Sources.OpenCode.Enabled != tc.wantOC ||
			cfg.Sources.Codex.Enabled != tc.wantCdx {
			t.Errorf("isolateSourceProvider(%s): enabled = {cc:%v oc:%v codex:%v}, want {cc:%v oc:%v codex:%v} — only the named provider must stay active",
				tc.named,
				cfg.Sources.ClaudeCode.Enabled, cfg.Sources.OpenCode.Enabled, cfg.Sources.Codex.Enabled,
				tc.wantCC, tc.wantOC, tc.wantCdx)
		}
	}
}

// TestHarvestCmd_SourcePathIsolatesProvider is the end-to-end proof that
// 0mp4l: with defaults that enable ALL providers, a seeded codex session at the
// codex DEFAULT dir is discovered by a bare harvest, but
// `harvest --source-provider claude-code --source-path <dir>` scopes the run to
// claude-code only — the codex default is NOT read (its session is absent from
// the output), while the claude session at the scoped path IS discovered.
func TestHarvestCmd_SourcePathIsolatesProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config")) // no config.yaml → BaseConfig defaults (all providers enabled)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	// Seed a codex rollout at the codex DEFAULT path ($HOME/.codex/sessions).
	const codexID = "c0dec0de-0000-4000-8000-000000000001"
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "05", "30", "rollout-2026-05-30T04-29-24-"+codexID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"timestamp":"2026-05-30T04:29:24.992Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-05-30T04:29:24.992Z","cwd":"/tmp/proj","originator":"codex_vscode","cli_version":"1.0","source":"vscode","thread_source":"user","model_provider":"openai"}}`, codexID)
	if err := os.WriteFile(rollout, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed a claude session in a separate source dir (the --source-path target).
	claudeDir := t.TempDir()
	const claudeID = "aaaaaaaa-0000-4000-8000-000000000002"
	claudeFile := filepath.Join(claudeDir, "-home-user-proj", claudeID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(claudeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeLine := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":%q,"sessionId":%q,"timestamp":"2026-05-30T04:29:24.992Z","cwd":"/tmp/proj","gitBranch":"main","version":"1.0","userType":"external"}`, claudeID, claudeID)
	if err := os.WriteFile(claudeFile, []byte(claudeLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sanity: a bare (unscoped) harvest DOES discover the codex default session.
	if out := runHarvestNoTestConfig(t); !strings.Contains(out, codexID) {
		t.Fatalf("precondition: bare harvest did not discover the seeded codex session %s — test would be vacuous:\n%s", codexID, out)
	}

	// Scoped: --source-path claude-code must isolate → codex default NOT read.
	out := runHarvestNoTestConfig(t, "--source-provider", string(defaults.HarnessClaudeCode), "--source-path", claudeDir)
	if !strings.Contains(out, claudeID) {
		t.Errorf("scoped harvest did not discover the claude session %s:\n%s", claudeID, out)
	}
	if strings.Contains(out, codexID) {
		t.Errorf("--source-path isolation leaked: codex default session %s was read:\n%s", codexID, out)
	}
}

// runHarvestNoTestConfig runs `harvest --dry-run [args...]` WITHOUT writing the
// all-disabled test config, so BaseConfig defaults (all providers enabled at
// their default paths) apply — required to exercise the isolation behavior.
func runHarvestNoTestConfig(t *testing.T, args ...string) string {
	t.Helper()
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", string(defaults.ResolveConfigFilePath()), "")
	root.AddCommand(BuildHarvestCommand())
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"harvest", "--dry-run"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("harvest %v: %v\n%s", args, err, buf.String())
	}
	return buf.String()
}
