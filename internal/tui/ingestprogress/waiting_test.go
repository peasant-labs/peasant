package ingestprogress

import (
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// TestViewSaysWhyTheRunIsWaiting proves the shared local-import progress
// view, which kickstart mounts, shows the run-lock wait reason while a run
// waits for another harvest, and drops it once the run is no longer waiting.
func TestViewSaysWhyTheRunIsWaiting(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	m := New(Options{Theme: theme.New(theme.ModeDark), StartedAt: start})
	m.SetSize(80, 24)
	m, _ = m.Update(ObserveMsg{At: start.Add(time.Second), Snapshot: map[ingest.Stage]ingest.StageProgress{}, Waiting: ingest.RunLockWaitNotice})
	if got := m.View(); !strings.Contains(got, "another harvest is running") {
		t.Errorf("waiting run: view does not say why it is waiting:\n%s", got)
	}
	m, _ = m.Update(ObserveMsg{At: start.Add(2 * time.Second), Snapshot: map[ingest.Stage]ingest.StageProgress{}})
	if got := m.View(); strings.Contains(got, "another harvest") {
		t.Errorf("run no longer waiting: view still shows the wait reason:\n%s", got)
	}
}
