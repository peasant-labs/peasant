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
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"golang.org/x/term"
)

// progressRenderer renders per-stage progress bars inline in a terminal.
// It uses Bubble Tea's renderer instead of hand-rolled ANSI erase/redraw logic,
// so redraws are diffed and capped. In non-TTY environments (CI, pipes) it is a
// no-op; the existing printSummary output is unaffected.
//
// Usage:
//
//	progState := ingest.NewProgressState()
//	ctx, cancel := context.WithCancel(ctx)
//	r := newProgressRenderer(os.Stderr, progState, nil, cancel)
//	go r.Run(ctx)       // cancellation stays mounted until operation returns
//	pipeline.Run(ctx)
//	r.Stop(ctx.Err() != nil)
//	r.Wait()            // blocks until renderer goroutine exits
//	r.Clear()           // erase progress lines before printing final summary
type progressRenderer struct {
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
	stop     chan bool
	stopOnce sync.Once
}

const progressRendererFPS = 24

// newProgressRenderer creates a tick-based renderer that reads from state and writes to w.
// TTY detection is performed on w if it is an *os.File; otherwise rendering
// is disabled (no-op mode for pipes/CI).
func newProgressRenderer(w io.Writer, state *ingest.ProgressState, anim *animation.Animation, cancel ...context.CancelFunc) *progressRenderer {
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
	r := &progressRenderer{
		w:     w,
		input: input,
		state: state,
		anim:  anim,
		isTTY: isTTY,
		theme: theme.New(theme.ModeDark),
		stop:  make(chan bool, 1),
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
// newProgressRenderer so Wait() is safe to call immediately after go r.Run(ctx)
// without a startup race.
func (r *progressRenderer) Run(ctx context.Context) {
	defer r.wg.Done()
	if ctx == nil {
		ctx = context.Background()
	}
	if !r.isTTY {
		// Operation cancellation is not renderer completion, even without a TTY.
		<-r.stop
		return
	}
	model := ingestprogress.NewModel(r.state, r.anim, r.theme, time.Now(), r.cancel)
	program := tea.NewProgram(
		model,
		tea.WithOutput(r.w),
		tea.WithInput(r.input),
		tea.WithFPS(progressRendererFPS),
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
				program.Send(ingestprogress.CancelMsg{})
				operationDone = nil
			case canceled := <-r.stop:
				program.Send(ingestprogress.StopMsg{Canceled: canceled || ctx.Err() != nil, At: time.Now()})
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

// Stop acknowledges operation completion. Cancellation alone must not unmount
// progress while the pipeline is still unwinding.
func (r *progressRenderer) Stop(canceled bool) {
	r.stopOnce.Do(func() { r.stop <- canceled })
}

// Wait blocks until Run has returned.
func (r *progressRenderer) Wait() { r.wg.Wait() }

func (r *progressRenderer) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

// IsTTY reports whether the renderer is writing to an interactive terminal.
// When false, rendering is a no-op and log suppression is not needed.
func (r *progressRenderer) IsTTY() bool { return r.isTTY }

// Clear is retained for the command path. Bubble Tea clears the rendered block
// when the model returns a blank view during shutdown, so no extra ANSI erase is
// necessary here.
func (r *progressRenderer) Clear() {}
