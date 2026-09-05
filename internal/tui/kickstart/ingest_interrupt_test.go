package kickstart_test

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

// blockingIngest returns an ingest runner that reports the context it was
// handed on started, then blocks until that context is canceled. It models the
// real failure: a long local import the runner cannot finish on its own, which
// must stay interruptible from the import screen.
func blockingIngest(started chan<- context.Context) kickstart.IngestFunc {
	return func(ctx context.Context) (*ftue.IngestResult, error) {
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// runIngestChildren runs each child of the commit batch in its own goroutine
// so the blocking ingest runner does not stall the test. Results are
// discarded; the test observes the attempt through the context it captured.
func runIngestChildren(cmd tea.Cmd) {
	for _, child := range unwrapBatch(cmd) {
		if child == nil {
			continue
		}
		go child()
	}
}

// awaitIngestStart waits for the in-flight ingest runner to report the context
// startIngest derived for the attempt.
func awaitIngestStart(t *testing.T, ch <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-ch:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatal("local ingest runner never started")
		return nil
	}
}

// startCommittedIngest drives a program with a blocking ingest runner through
// the receipt commit into PhaseIngest, starts the attempt's commands, seeds one
// started stage, and observes one progress tick so the bar row renders.
func startCommittedIngest(t *testing.T, started chan context.Context) kickstart.Program {
	t.Helper()
	progress := &fixtureProgressSource{}
	var tick func(time.Time) tea.Msg
	p, _ := newTestProgram(t, kickstart.ProgramDeps{
		Ingest:   blockingIngest(started),
		Progress: progress,
		Tick: func(_ time.Duration, callback func(time.Time) tea.Msg) tea.Cmd {
			tick = callback
			return func() tea.Msg { return callback(time.Now()) }
		},
	})
	p = declineOAuth(t, p)
	p, cmd := advanceToCommit(p)
	if !p.Committed() || p.Phase() != kickstart.PhaseIngest {
		t.Fatalf("committed/phase=%t/%s, want true/ingest", p.Committed(), p.Phase())
	}
	runIngestChildren(cmd)
	progress.Set([]progressStageFixture{{Stage: ingest.StageDiscover, Started: true, Done: 1, Total: 4}})
	if tick == nil {
		t.Fatal("starting local ingest did not schedule the injected progress tick")
	}
	p, _ = p.Update(tick(time.Now()))
	return p
}

// TestProgram_IngestCtrlCCancelsAndQuits proves that while local import runs,
// ctrl+c cancels the in-flight ingest and quits kickstart. The pre-fix flow
// dropped every key here, trapping the user on the import screen.
func TestProgram_IngestCtrlCCancelsAndQuits(t *testing.T) {
	t.Parallel()
	started := make(chan context.Context, 1)
	p := startCommittedIngest(t, started)
	ingestCtx := awaitIngestStart(t, started)

	view := stripRender(p.View())
	for _, want := range []string{"importing transcripts", "local import progress", "discover", "1/4", "█", "ctrl+c to quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("local import view omits %q:\n%s", want, view)
		}
	}
	// The started stage carries its elapsed duration on its own bar row,
	// right of the counts.
	discoverRow := ""
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "discover") {
			discoverRow = strings.TrimSpace(line)
		}
	}
	if discoverRow == "" {
		t.Fatalf("local import view omits the discover bar row:\n%s", view)
	}
	if !strings.Contains(discoverRow, "1/4  ") {
		t.Errorf("discover row omits the duration column after its counts in %q", discoverRow)
	}

	p, quit := p.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	requireCanceled(t, ingestCtx, "ctrl+c")
	if quit == nil || quit() != (tea.QuitMsg{}) {
		t.Fatal("ctrl+c on the local import screen did not quit kickstart")
	}
}

// TestProgram_IngestQQuitsLikeCtrlC proves plain q cancels the in-flight
// ingest and quits kickstart exactly like ctrl+c: both keys share the quit
// binding the import screen matches.
func TestProgram_IngestQQuitsLikeCtrlC(t *testing.T) {
	t.Parallel()
	started := make(chan context.Context, 1)
	p := startCommittedIngest(t, started)
	ingestCtx := awaitIngestStart(t, started)

	p, quit := p.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	requireCanceled(t, ingestCtx, "q")
	if quit == nil || quit() != (tea.QuitMsg{}) {
		t.Fatal("q on the local import screen did not quit kickstart")
	}
}

// TestProgram_IngestEscIsIgnored proves esc neither quits kickstart nor stops
// the in-flight import: quitting stays on ctrl+c or q only.
func TestProgram_IngestEscIsIgnored(t *testing.T) {
	t.Parallel()
	started := make(chan context.Context, 1)
	p := startCommittedIngest(t, started)
	ingestCtx := awaitIngestStart(t, started)

	p, cmd := p.Update(press(tea.KeyEscape))
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("esc on the local import screen quit kickstart")
		}
	}
	if p.Phase() != kickstart.PhaseIngest {
		t.Fatalf("esc phase = %s, want the local import screen (ingest)", p.Phase())
	}
	select {
	case <-ingestCtx.Done():
		t.Fatal("esc canceled the in-flight local import")
	default:
	}

	// Release the blocking runner through the quit path so no goroutine
	// outlives the test.
	p, quit := p.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	requireCanceled(t, ingestCtx, "cleanup")
	if quit == nil || quit() != (tea.QuitMsg{}) {
		t.Fatal("q on the local import screen did not quit kickstart")
	}
}
