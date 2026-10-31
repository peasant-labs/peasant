package kickstart_test

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

// TestProgramDoneShowsSessionsExcludedBySelection: the local import result
// no longer counts sessions the selection left out as unchanged, so the
// mounted kickstart completion must name them, or they vanish from the
// screen. A run with none excluded keeps the old line.
func TestProgramDoneShowsSessionsExcludedBySelection(t *testing.T) {
	t.Parallel()
	if view := doneViewWithExcluded(t, 4); !strings.Contains(view, "4 excluded by selection") {
		t.Errorf("completion omits the excluded count:\n%s", view)
	}
	if view := doneViewWithExcluded(t, 0); strings.Contains(view, "excluded by selection") {
		t.Errorf("completion mentions exclusions on a run with none:\n%s", view)
	}
}

func doneViewWithExcluded(t *testing.T, excluded int) string {
	t.Helper()
	result := &ftue.IngestResult{New: 2, Updated: 1, Unchanged: 3, ExcludedBySelection: excluded}
	program, _ := newTestProgram(t, kickstart.ProgramDeps{AlreadyConnected: true, Ingest: func(context.Context) (*ftue.IngestResult, error) { return result, nil }})
	program.SetSize(120, 40)
	program = drainProgram(program, program.Init())
	program, command := advanceToCommit(program)
	program = drainCmds(t, program, command)
	if program.Phase() != kickstart.PhaseDone {
		t.Fatalf("excluded=%d: phase = %s, want done", excluded, program.Phase())
	}
	return strings.Join(strings.Fields(ansi.Strip(program.View())), " ")
}
