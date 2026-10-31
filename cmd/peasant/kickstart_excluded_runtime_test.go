package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

// TestKickstartImportShowsSessionsExcludedBySelection runs the real kickstart
// import runner over two Claude sessions with a committed selection that keeps
// both out, and reads the wizard's result page. Before the fix the page shows
// no trace of the two sessions; the runner and page APIs used here all exist
// on develop, so this fails at runtime there, not at compile time.
//
// Not parallel: the runner swaps the process-wide slog default while it runs.
func TestKickstartImportShowsSessionsExcludedBySelection(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(filepath.Join(src, "-tmp-qa-sel"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		sid := fmt.Sprintf("cccccccc-0000-4000-8000-00000000000%d", i)
		line := `{"type":"user","sessionId":"` + sid + `","uuid":"u1","cwd":"/tmp/qa-sel","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
		if err := os.WriteFile(filepath.Join(src, "-tmp-qa-sel", sid+".jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\nselection:\n  mode: selected\n", src, filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "kickstart"}
	cmd.Flags().String("data-dir", dir, "")

	runner, progress := buildFTUEIngestRunner(cmd, configPath)
	page := ftue.NewIngestPage("Local import")
	start := page.Start(runner, &ftue.WizardAnswers{SelectionMode: config.SelectionModeSelected}, progress)
	batch, ok := start().(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("IngestPage.Start did not return the run batch: %T", start())
	}
	page.Update(batch[0]())
	if view := ansi.Strip(page.View(100, 40)); !strings.Contains(view, "2 sessions excluded by selection") {
		t.Errorf("kickstart import page does not report the 2 sessions the selection excluded:\n%s", view)
	}
}
