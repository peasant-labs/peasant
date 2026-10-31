package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dayvidpham/bestiary"
)

// runModelsSyncNoInput runs `models sync` with an empty stdin, the way a
// script or CI job runs it, against an unreachable models.dev so the command
// takes the bundled-snapshot fallback.
func runModelsSyncNoInput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	cmd := buildModelsCommand(bestiary.NewClient(bestiary.WithBaseURL("http://127.0.0.1:1")))
	cmd.PersistentFlags().String("data-dir", "", "")
	cmd.PersistentFlags().String("config-dir", "", "")
	cmd.PersistentFlags().String("config", "", "")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "sync"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// TestModelsSyncRunsWithoutAPrompt guards #689: --dry-run writes nothing, so
// it must not ask y/N, and --yes must skip the prompt so the sync can be
// scripted without piping "y".
func TestModelsSyncRunsWithoutAPrompt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--dry-run"}, "would be synced (dry-run)"},
		{[]string{"--yes"}, "model(s) synced"},
	} {
		out, err := runModelsSyncNoInput(t, c.args...)
		if err != nil || strings.Contains(out, "Continue? [y/N]") || !strings.Contains(out, c.want) {
			t.Errorf("models sync %v with no input: err = %v, want %q and no prompt:\n%s", c.args, err, c.want, out)
		}
	}
}

// TestModelsSyncStillAsksWithoutAFlag proves a plain sync keeps its gate: with
// nobody to answer, it refuses, names --yes, and reports no sync.
func TestModelsSyncStillAsksWithoutAFlag(t *testing.T) {
	t.Parallel()
	out, err := runModelsSyncNoInput(t)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("models sync with no input: err = %v, want a refusal naming --yes:\n%s", err, out)
	}
	if !strings.Contains(out, "Continue? [y/N]") || strings.Contains(out, "model(s) synced") {
		t.Fatalf("models sync with no input: want the prompt and no sync:\n%s", out)
	}
}
