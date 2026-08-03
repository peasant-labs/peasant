package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
)

// executeMetricsCmd runs the metrics command under a test root with
// --data-dir=dataDir (parallel-safe; no t.Setenv), capturing output.
func executeMetricsCmd(t *testing.T, dataDir string, args []string) (string, error) {
	t.Helper()
	return executeWithDataDir(t, BuildMetricsCommand(), dataDir, args)
}

// TestMetricsCmd_Flags verifies all expected flags on the compute subcommand.
func TestMetricsCmd_Flags(t *testing.T) {
	t.Parallel()
	cmd := BuildMetricsCommand()

	// Find the "compute" subcommand.
	var computeCmd *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "compute" {
			computeCmd = c
			break
		}
	}
	if computeCmd == nil {
		t.Fatal("compute subcommand not found under metrics")
	}

	for _, name := range []string{"session", "force", "verbose", "dry-run"} {
		f := computeCmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("flag --%s not registered on metrics compute", name)
		}
	}
}

// TestMetricsCompute_DryRun_NoWrites verifies --dry-run does not write metrics.
// With an empty store, "No sessions found." is returned before the dry-run logic.
func TestMetricsCompute_DryRun_NoWrites(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	output, err := executeMetricsCmd(t, dataHome, []string{"compute", "--dry-run"})
	if err != nil {
		t.Fatalf("metrics compute --dry-run: unexpected error: %v\noutput: %s", err, output)
	}
	// Empty store: early exit with "No sessions found." (no writes either way).
	if !strings.Contains(output, "No sessions found") {
		t.Errorf("expected 'No sessions found' on empty store; got: %s", output)
	}

	// Verify DB exists (store was opened) but no session_metrics rows.
	dbPath := filepath.Join(dataHome, string(defaults.AppName), "peasant.db")
	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		t.Errorf("--dry-run should still open (create) the DB; file not found at %s", dbPath)
	}
}

// TestMetricsCompute_EmptyStore verifies a compute against an empty store
// reports 0 sessions computed.
func TestMetricsCompute_EmptyStore(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	output, err := executeMetricsCmd(t, dataHome, []string{"compute"})
	if err != nil {
		t.Fatalf("metrics compute on empty store: unexpected error: %v\noutput: %s", err, output)
	}
	if !strings.Contains(output, "No sessions found") && !strings.Contains(output, "0 session(s) computed") {
		t.Errorf("expected 'No sessions found' or '0 sessions computed'; got: %s", output)
	}
}

// TestMetricsCompute_InvalidSession verifies --session with an invalid ID returns error.
func TestMetricsCompute_InvalidSession(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()

	// Session IDs must match the UUID regex; "not a valid id!" should fail.
	_, err := executeMetricsCmd(t, dataHome, []string{"compute", "--session=not a valid id!"})
	if err == nil {
		t.Fatal("expected error for invalid --session, got nil")
	}
	if !strings.Contains(err.Error(), "invalid session ID") {
		t.Errorf("error should mention 'invalid session ID', got: %v", err)
	}
}
