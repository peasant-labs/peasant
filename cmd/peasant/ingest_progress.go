package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/internal/tui/kit"
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
//	r := newProgressRenderer(os.Stderr, progState)
//	go r.Run(ctx)       // tick-based rendering until ctx is cancelled
//	pipeline.Run(ctx)
//	cancel()            // stop renderer
//	r.Wait()            // blocks until renderer goroutine exits
//	r.Clear()           // erase progress lines before printing final summary
type progressRenderer struct {
	w      io.Writer
	input  io.Reader
	state  *ingest.ProgressState
	anim   *animation.Animation
	isTTY  bool
	order  []ingest.Stage
	wg     sync.WaitGroup
	cancel context.CancelFunc
	errMu  sync.Mutex
	err    error
}

const (
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
	cancel       context.CancelFunc
	width        int
	height       int
	presentation ingestprogress.Presentation
	theme        theme.Theme
}

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
		// shell after harvest exits. The model still ignores all key input.
		input = os.Stdin
	}
	r := &progressRenderer{
		w:     w,
		input: input,
		state: state,
		anim:  anim,
		isTTY: isTTY,
		order: ingest.StageOrder,
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
	if !r.isTTY {
		// Non-TTY: drain context without rendering.
		<-ctx.Done()
		return
	}
	model := newProgressModel(r.state, r.anim, r.order, theme.New(theme.ModeDark), time.Now())
	model.cancel = r.cancel
	program := tea.NewProgram(
		model,
		tea.WithOutput(r.w),
		tea.WithInput(r.input),
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
		r.errMu.Lock()
		r.err = err
		r.errMu.Unlock()
		if r.cancel != nil {
			r.cancel()
		}
		fmt.Fprintf(r.w, "warning: harvest progress renderer failed: %v\n", err)
	}
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

func progressTick() tea.Cmd {
	return tea.Tick(progressTickInterval, func(t time.Time) tea.Msg {
		return progressTickMsg(t)
	})
}

func (m progressModel) Init() tea.Cmd { return progressTick() }

func newProgressModel(state *ingest.ProgressState, anim *animation.Animation, order []ingest.Stage, th theme.Theme, startedAt time.Time) progressModel {
	return progressModel{state: state, anim: anim, order: order, theme: th, presentation: ingestprogress.New(startedAt, false)}
}

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			if m.cancel != nil {
				m.cancel()
			}
			m.stopped = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case progressStopMsg:
		m.stopped = true
		return m, tea.Quit
	case progressTickMsg:
		tickTime := time.Time(msg)
		m.presentation.Observe(tickTime, m.state.Snapshot())
		if m.anim != nil && len(m.anim.Frames) > 0 {
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
	if !m.theme.Mode.IsValid() {
		m.theme = theme.New(theme.ModeDark)
	}
	if m.presentation.StartedAt().IsZero() {
		m.presentation.Reset(time.Now(), false)
	}
	now := time.Now()
	m.presentation.Observe(now, m.state.Snapshot())
	var lines []string
	styles := m.theme.Styles()
	if m.anim != nil && len(m.anim.Frames) > 0 {
		lines = append(lines, m.anim.Frames[m.animFrame]...)
		lines = append(lines, "")
	}
	lines = append(lines, styles.Header.Render("local import progress"))
	available := 0
	if m.height > 0 {
		available = max(m.height-len(lines)-2, 0)
	}
	lines = append(lines, m.presentation.Lines(styles, now, available)...)
	lines = append(lines, "", styles.Muted.Render("ctrl+c to cancel"))
	panel := kit.NewPanel(m.theme)
	panel.SetSize(m.width, 0)
	for _, line := range lines {
		panel.Rendered(line)
	}
	return panel.View()
}
