package ftue

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestIngestPageShowsSessionsExcludedBySelection: kickstart chooses a
// selection, so the wizard's result must show the sessions it kept out
// instead of losing them from every count.
func TestIngestPageShowsSessionsExcludedBySelection(t *testing.T) {
	t.Parallel()
	result := &IngestResult{New: 1, Unchanged: 2, ExcludedBySelection: 3}
	page := NewIngestPage("Local import")
	command := page.Start(func(context.Context, WizardAnswers) (*IngestResult, error) { return result, nil }, &WizardAnswers{}, nil)
	batch := command().(tea.BatchMsg)
	page.Update(batch[0]())
	if view := ansi.Strip(page.View(100, 40)); !strings.Contains(view, "3 sessions excluded by selection") {
		t.Errorf("ingest result view lost the excluded count:\n%s", view)
	}
	if text := strings.Join(page.ingestCompletion().lines, "\n"); !strings.Contains(text, "3 sessions excluded by selection") {
		t.Errorf("ingest completion lost the excluded count:\n%s", text)
	}
}
