package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
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
//	r := newProgressRenderer(os.Stderr, progState)
//	go r.Run(ctx)       // tick-based rendering until ctx is cancelled
//	pipeline.Run(ctx)
//	cancel()            // stop renderer
//	r.Wait()            // blocks until renderer goroutine exits
//	r.Clear()           // erase progress lines before printing final summary
type progressRenderer struct {
	w     io.Writer
	state *ingest.ProgressState
	anim  *animation.Animation
	isTTY bool
	order []ingest.Stage
	wg    sync.WaitGroup
}

const (
	barWidth               = 24
	barFill                = '█'
	barEmpty               = '░'
	progressRendererFPS    = 24
	progressTickInterval   = time.Second / progressRendererFPS
	animationFrameInterval = 300 * time.Millisecond
)

type progressTickMsg time.Time

type progressStopMsg struct{}

type progressModel struct {
	state        *ingest.ProgressState
	anim         *animation.Animation
	order        []ingest.Stage
	animFrame    int
	lastAnimTick time.Time
	stopped      bool
}

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

// Run starts a small Bubble Tea program that reads a snapshot from state at a
// capped frame rate. Call as a goroutine; wg.Add(1) is called by
// newProgressRenderer so Wait() is safe to call immediately after go r.Run(ctx)
// without a startup race.
func (r *progressRenderer) Run(ctx context.Context) {
	defer r.wg.Done()
	if !r.isTTY {
		// Non-TTY: drain context without rendering.
		<-ctx.Done()
		return
	}
	program := tea.NewProgram(
		progressModel{state: r.state, anim: r.anim, order: r.order},
		tea.WithOutput(r.w),
		tea.WithInput(nil),
		tea.WithFPS(progressRendererFPS),
		tea.WithoutSignalHandler(),
	)
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			program.Send(progressStopMsg{})
		case <-finished:
		}
	}()
	_, err := program.Run()
	close(finished)
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		fmt.Fprintf(r.w, "warning: harvest progress renderer failed: %v\n", err)
	}
}

// Wait blocks until Run has returned.
func (r *progressRenderer) Wait() { r.wg.Wait() }

// IsTTY reports whether the renderer is writing to an interactive terminal.
// When false, rendering is a no-op and log suppression is not needed.
func (r *progressRenderer) IsTTY() bool { return r.isTTY }

// Clear is retained for the command path. Bubble Tea clears the rendered block
// when the model returns a blank view during shutdown, so no extra ANSI erase is
// necessary here.
func (r *progressRenderer) Clear() {}

func progressTick() tea.Cmd {
	return tea.Tick(progressTickInterval, func(t time.Time) tea.Msg {
		return progressTickMsg(t)
	})
}

func (m progressModel) Init() tea.Cmd { return progressTick() }

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case progressStopMsg:
		m.stopped = true
		return m, tea.Quit
	case progressTickMsg:
		if m.anim != nil && len(m.anim.Frames) > 0 {
			tickTime := time.Time(msg)
			if m.lastAnimTick.IsZero() || tickTime.Sub(m.lastAnimTick) >= animationFrameInterval {
				m.animFrame = (m.animFrame + 1) % len(m.anim.Frames)
				m.lastAnimTick = tickTime
			}
		}
		return m, progressTick()
	}
	return m, nil
}

func (m progressModel) View() tea.View {
	if m.stopped {
		return tea.NewView("")
	}
	return tea.NewView(m.render())
}

func (m progressModel) render() string {
	snap := m.state.Snapshot()
	var lines []string

	// Render animation frame above progress bars.
	if m.anim != nil && len(m.anim.Frames) > 0 {
		f := m.anim.Frames[m.animFrame]
		for _, line := range f {
			lines = append(lines, line)
		}
		lines = append(lines, "")
	}

	for _, stage := range m.order {
		sp, ok := snap[stage]
		if !ok {
			continue
		}
		lines = append(lines, renderProgressBar(stage, sp))
	}
	return strings.Join(lines, "\n")
}

// renderProgressBar formats a single stage bar as a fixed-width string.
func renderProgressBar(stage ingest.Stage, sp ingest.StageProgress) string {
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
		if sp.Done > 0 && filled == 0 {
			filled = 1
		}
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
