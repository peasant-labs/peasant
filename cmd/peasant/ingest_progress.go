package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
	"golang.org/x/term"
)

// progressRenderer renders per-stage progress bars inline in a terminal.
// It redraws the same lines in-place using ANSI cursor control at a fixed
// 10 Hz tick rate. In non-TTY environments (CI, pipes) it is a no-op;
// the existing printSummary output is unaffected.
//
// Usage:
//
//	progState := ingest.NewProgressState()
//	ctx, cancel := context.WithCancel(ctx)
//	r := newProgressRenderer(os.Stderr, progState)
//	go r.Run(ctx)       // tick-based rendering until ctx is cancelled
//	pipeline.Run(ctx)
//	cancel()            // stop renderer
//	r.Wait()            // blocks until renderer goroutine exits
//	r.Clear()           // erase progress lines before printing final summary
type progressRenderer struct {
	w         io.Writer
	state     *ingest.ProgressState
	anim      *animation.Animation
	isTTY     bool
	mu        sync.Mutex
	order     []ingest.Stage
	drawn     int // number of lines currently on screen
	animFrame int // current animation frame index
	animTick  int // tick counter for animation frame rate
	wg        sync.WaitGroup
}

const (
	barWidth          = 24
	barFill           = '█'
	barEmpty          = '░'
	ansiUp            = "\x1b[%dA" // move cursor up N lines
	ansiEraseLn       = "\x1b[2K"  // erase current line
	ansiCR            = "\r"
	animTicksPerFrame = 3 // at 10 Hz tick, advance animation every 3 ticks (~3.3 fps)
)

// newProgressRenderer creates a tick-based renderer that reads from state and writes to w.
// TTY detection is performed on w if it is an *os.File; otherwise rendering
// is disabled (no-op mode for pipes/CI).
func newProgressRenderer(w io.Writer, state *ingest.ProgressState, anim *animation.Animation) *progressRenderer {
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	r := &progressRenderer{
		w:     w,
		state: state,
		anim:  anim,
		isTTY: isTTY,
		order: ingest.StageOrder,
	}
	// Add before the caller launches go r.Run(ctx) so Wait() has no race window.
	r.wg.Add(1)
	return r
}

// Run ticks at 10 Hz, reading a snapshot from state and redrawing the progress
// display. Runs until ctx is cancelled, then does a final paint before returning.
// Call as a goroutine; wg.Add(1) is called by newProgressRenderer so Wait() is
// safe to call immediately after go r.Run(ctx) without a startup race.
func (r *progressRenderer) Run(ctx context.Context) {
	defer r.wg.Done()
	if !r.isTTY {
		// Non-TTY: drain context without rendering.
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond) // 10 Hz
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.redraw() // final paint to show completed state
			r.mu.Unlock()
			return
		case <-ticker.C:
			r.mu.Lock()
			if r.anim != nil && len(r.anim.Frames) > 0 {
				r.animTick++
				if r.animTick%animTicksPerFrame == 0 {
					r.animFrame = (r.animFrame + 1) % len(r.anim.Frames)
				}
			}
			r.redraw()
			r.mu.Unlock()
		}
	}
}

// Wait blocks until Run has returned.
func (r *progressRenderer) Wait() { r.wg.Wait() }

// IsTTY reports whether the renderer is writing to an interactive terminal.
// When false, rendering is a no-op and log suppression is not needed.
func (r *progressRenderer) IsTTY() bool { return r.isTTY }

// Clear erases all progress lines from the terminal so the final summary
// prints cleanly from the top of the progress block.
func (r *progressRenderer) Clear() {
	if !r.isTTY {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.erase()
}

// erase moves the cursor up and clears all drawn lines.
// Must be called with r.mu held.
func (r *progressRenderer) erase() {
	if r.drawn == 0 {
		return
	}
	// Move up to the first drawn line.
	fmt.Fprintf(r.w, ansiUp, r.drawn)
	// Erase each line.
	for i := 0; i < r.drawn; i++ {
		fmt.Fprint(r.w, ansiEraseLn+ansiCR)
		if i < r.drawn-1 {
			fmt.Fprint(r.w, "\n")
		}
	}
	// Return cursor to the start of the first drawn line.
	fmt.Fprintf(r.w, ansiUp, r.drawn-1)
	r.drawn = 0
}

// redraw erases current lines and redraws all stage bars from the current snapshot.
// Must be called with r.mu held.
func (r *progressRenderer) redraw() {
	snap := r.state.Snapshot()
	r.erase()
	lines := 0

	// Render animation frame above progress bars.
	if r.anim != nil && len(r.anim.Frames) > 0 {
		f := r.anim.Frames[r.animFrame]
		for _, line := range f {
			fmt.Fprint(r.w, line+"\n")
			lines++
		}
		fmt.Fprint(r.w, "\n")
		lines++
	}

	for _, stage := range r.order {
		sp, ok := snap[stage]
		if !ok {
			continue
		}
		fmt.Fprint(r.w, r.renderBar(stage, sp)+"\n")
		lines++
	}
	r.drawn = lines
}

// renderBar formats a single stage bar as a fixed-width string.
func (r *progressRenderer) renderBar(stage ingest.Stage, sp ingest.StageProgress) string {
	// Status icon.
	icon := "○" // not started
	switch {
	case sp.HasErr:
		icon = "✗"
	case sp.Ended:
		icon = "✓"
	case sp.Done > 0 || sp.Total > 0:
		icon = "●"
	}

	// Fill fraction.
	var filled int
	if sp.Total > 0 {
		filled = sp.Done * barWidth / sp.Total
		if filled > barWidth {
			filled = barWidth
		}
	} else if sp.Ended {
		filled = barWidth
	}
	barStr := strings.Repeat(string(barFill), filled) +
		strings.Repeat(string(barEmpty), barWidth-filled)

	// Count label.
	var count string
	if sp.Total > 0 {
		count = fmt.Sprintf("%d/%d", sp.Done, sp.Total)
	} else if sp.Ended {
		count = "done"
	}

	// Stage name padded to a fixed width.
	const nameWidth = 13
	name := stage.String()
	if len(name) < nameWidth {
		name = name + strings.Repeat(" ", nameWidth-len(name))
	}

	return fmt.Sprintf(" %s %s  %s  %s", icon, name, barStr, count)
}
