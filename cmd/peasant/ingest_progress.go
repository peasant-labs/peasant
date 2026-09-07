package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/harvestprogress"
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"golang.org/x/term"
)

// progressProgram hosts the inline harvest Bubble Tea program.
// It uses Bubble Tea's renderer instead of hand-rolled ANSI erase/redraw logic,
// so redraws are diffed and capped. In non-TTY environments (CI, pipes) it is a
// no-op; the existing printSummary output is unaffected.
//
// Usage:
//
//	progState := ingest.NewProgressState()
//	ctx, cancel := context.WithCancel(ctx)
//	r := newProgressProgram(os.Stderr, progState, nil, cancel)
//	go r.Run(ctx)       // cancellation stays mounted until operation returns
//	pipeline.Run(ctx)
//	r.Finish(ingestprogress.FinalMsg{Outcome: ingestprogress.FinalSucceeded})
//	r.Wait()            // blocks until renderer goroutine exits
//	r.Clear()           // erase progress lines before printing final summary
type progressProgram struct {
	w        io.Writer
	input    io.Reader
	state    *ingest.ProgressState
	anim     *animation.Animation
	isTTY    bool
	theme    theme.Theme
	wg       sync.WaitGroup
	cancel   context.CancelFunc
	errMu    sync.Mutex
	err      error
	finish   chan ingestprogress.FinalMsg
	stopOnce sync.Once
}

type harvestPipeline interface {
	Run(context.Context) (*ingest.PipelineResult, error)
}

type harvestProgressProgram interface {
	Run(context.Context)
	Finish(ingestprogress.FinalMsg)
	Wait()
	Err() error
}

type harvestCompletionKind uint8

const (
	harvestCompletionSucceeded harvestCompletionKind = iota + 1
	harvestCompletionCanceled
	harvestCompletionFailed
)

type harvestExecution struct {
	kind     harvestCompletionKind
	result   *ingest.PipelineResult
	runErr   error
	ctxErr   error
	uiErr    error
	at       time.Time
	snapshot map[ingest.Stage]ingest.StageProgress
}

func executeHarvest(ctx context.Context, pipeline harvestPipeline, progress *ingest.ProgressState, program harvestProgressProgram) harvestExecution {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return harvestExecution{kind: harvestCompletionCanceled, ctxErr: ctxErr, at: time.Now(), snapshot: progress.Snapshot()}
	}
	go program.Run(ctx)
	result, runErr := pipeline.Run(ctx)
	completedAt := time.Now()
	ctxErr := ctx.Err()
	snapshot := progress.Snapshot()
	kind := harvestCompletionSucceeded
	finalOutcome := ingestprogress.FinalSucceeded
	if ctxErr != nil {
		kind = harvestCompletionCanceled
		finalOutcome = ingestprogress.FinalCanceled
	} else if runErr != nil {
		kind = harvestCompletionFailed
		finalOutcome = ingestprogress.FinalFailed
	}
	program.Finish(ingestprogress.FinalMsg{At: completedAt, Snapshot: snapshot, Outcome: finalOutcome})
	program.Wait()
	return harvestExecution{kind: kind, result: result, runErr: runErr, ctxErr: ctxErr, uiErr: program.Err(), at: completedAt, snapshot: snapshot}
}

const progressProgramFPS = 24

// newProgressProgram creates a tick-based program that reads from state and writes to w.
// TTY detection is performed on w if it is an *os.File; otherwise rendering
// is disabled (no-op mode for pipes/CI).
func newProgressProgram(w io.Writer, state *ingest.ProgressState, anim *animation.Animation, cancel ...context.CancelFunc) *progressProgram {
	isTTY := false
	var input io.Reader
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	if isTTY && term.IsTerminal(int(os.Stdin.Fd())) {
		// Bubble Tea's renderer can ask the terminal about supported modes. Reading
		// stdin lets it consume those replies instead of leaking them back to the
		// shell after harvest exits. Ctrl+C cancels the pipeline from raw mode.
		input = os.Stdin
	}
	r := &progressProgram{
		w:      w,
		input:  input,
		state:  state,
		anim:   anim,
		isTTY:  isTTY,
		theme:  theme.New(theme.ModeDark),
		finish: make(chan ingestprogress.FinalMsg, 1),
	}
	if len(cancel) > 0 {
		r.cancel = cancel[0]
	}
	// Add before the caller launches go r.Run(ctx) so Wait() has no race window.
	r.wg.Add(1)
	return r
}

// Run starts a small Bubble Tea program that reads a snapshot from state at a
// capped frame rate. Call as a goroutine; wg.Add(1) is called by
// newProgressProgram so Wait() is safe to call immediately after go r.Run(ctx)
// without a startup race.
func (r *progressProgram) Run(ctx context.Context) {
	defer r.wg.Done()
	if ctx == nil {
		ctx = context.Background()
	}
	if !r.isTTY {
		// Operation cancellation is not renderer completion, even without a TTY.
		<-r.finish
		return
	}
	model := r.newModel(ctx, time.Now())
	program := tea.NewProgram(
		model,
		tea.WithOutput(r.w),
		tea.WithInput(r.input),
		tea.WithFPS(progressProgramFPS),
		tea.WithoutSignalHandler(),
	)
	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		operationDone := ctx.Done()
		for {
			select {
			case <-operationDone:
				program.Send(harvestprogress.CancelMsg{})
				operationDone = nil
			case final := <-r.finish:
				program.Send(final)
				return
			case <-finished:
				return
			}
		}
	}()
	_, err := program.Run()
	close(finished)
	<-watcherDone
	if err != nil {
		r.errMu.Lock()
		r.err = err
		r.errMu.Unlock()
		if r.cancel != nil {
			r.cancel()
		}
		fmt.Fprintf(r.w, "warning: harvest progress renderer failed: %v\n", err)
	}
}

func (r *progressProgram) newModel(ctx context.Context, startedAt time.Time) harvestprogress.Model {
	return harvestprogress.New(harvestprogress.Options{Progress: r.state, Theme: r.theme, Animation: r.anim, StartedAt: startedAt, Cancel: r.cancel, ContextErr: ctx.Err})
}

// Finish publishes the command-owned final outcome. Cancellation alone must not
// unmount progress while the pipeline is still unwinding.
func (r *progressProgram) Finish(final ingestprogress.FinalMsg) {
	r.stopOnce.Do(func() { r.finish <- final })
}

// Wait blocks until Run has returned.
func (r *progressProgram) Wait() { r.wg.Wait() }

func (r *progressProgram) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

// IsTTY reports whether the renderer is writing to an interactive terminal.
// When false, rendering is a no-op and log suppression is not needed.
func (r *progressProgram) IsTTY() bool { return r.isTTY }

// Clear is retained for the command path. Bubble Tea clears the rendered block
// when the model returns a blank view during shutdown, so no extra ANSI erase is
// necessary here.
func (r *progressProgram) Clear() {}

var _ harvestPipeline = (*ingest.Pipeline)(nil)
var _ harvestProgressProgram = (*progressProgram)(nil)
