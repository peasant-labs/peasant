// Package animation provides frame-based ASCII terminal animations
// for long-running CLI operations.
package animation

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

// Frame is a single animation frame as a slice of lines.
type Frame []string

// Animation holds a sequence of frames and playback settings.
type Animation struct {
	Frames   []Frame
	Interval time.Duration
}

// Renderer plays an Animation in the terminal using ANSI cursor control.
// In non-TTY environments (CI, pipes) it is a no-op.
type Renderer struct {
	w     io.Writer
	anim  *Animation
	isTTY bool
	mu    sync.Mutex
	frame int
	drawn int
	wg    sync.WaitGroup
}

const (
	curUp   = "\x1b[%dA"
	eraseLn = "\x1b[2K"
	cr      = "\r"
)

// NewRenderer creates a renderer that plays anim on w.
func NewRenderer(w io.Writer, anim *Animation) *Renderer {
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	r := &Renderer{w: w, anim: anim, isTTY: isTTY}
	r.wg.Add(1)
	return r
}

// Run plays the animation until ctx is cancelled.
func (r *Renderer) Run(ctx context.Context) {
	defer r.wg.Done()
	if !r.isTTY || r.anim == nil || len(r.anim.Frames) == 0 {
		<-ctx.Done()
		return
	}
	interval := r.anim.Interval
	if interval == 0 {
		interval = 300 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	r.mu.Lock()
	r.paint()
	r.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			r.frame = (r.frame + 1) % len(r.anim.Frames)
			r.paint()
			r.mu.Unlock()
		}
	}
}

// Wait blocks until Run returns.
func (r *Renderer) Wait() { r.wg.Wait() }

// IsTTY reports whether the renderer detected an interactive terminal.
func (r *Renderer) IsTTY() bool { return r.isTTY }

// Clear erases all drawn animation lines from the terminal.
func (r *Renderer) Clear() {
	if !r.isTTY {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.erase()
}

func (r *Renderer) erase() {
	if r.drawn == 0 {
		return
	}
	fmt.Fprintf(r.w, curUp, r.drawn)
	for i := 0; i < r.drawn; i++ {
		fmt.Fprint(r.w, eraseLn+cr)
		if i < r.drawn-1 {
			fmt.Fprint(r.w, "\n")
		}
	}
	fmt.Fprintf(r.w, curUp, r.drawn-1)
	r.drawn = 0
}

func (r *Renderer) paint() {
	r.erase()
	f := r.anim.Frames[r.frame]
	for _, line := range f {
		fmt.Fprint(r.w, line+"\n")
	}
	r.drawn = len(f)
}

// --- Sprite composition helpers ---

// sprite represents a positioned piece of ASCII art on a canvas.
type sprite struct {
	x, y  int
	lines []string
}

// at creates a sprite at position (x, y) with the given lines.
func at(x, y int, lines ...string) sprite {
	return sprite{x: x, y: y, lines: lines}
}

// compose builds a Frame by placing sprites onto a blank canvas.
// Non-space characters from later sprites overwrite earlier ones.
func compose(width, height int, sprites []sprite) Frame {
	canvas := make([][]byte, height)
	for i := range canvas {
		row := make([]byte, width)
		for j := range row {
			row[j] = ' '
		}
		canvas[i] = row
	}
	for _, sp := range sprites {
		for dy, line := range sp.lines {
			row := sp.y + dy
			if row < 0 || row >= height {
				continue
			}
			for dx := 0; dx < len(line); dx++ {
				ch := line[dx]
				col := sp.x + dx
				if col < 0 || col >= width || ch == ' ' {
					continue
				}
				canvas[row][col] = ch
			}
		}
	}
	frame := make(Frame, height)
	for i, row := range canvas {
		frame[i] = string(row)
	}
	return frame
}
