package ingestprogress

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	TickInterval           = time.Second / 24
	animationFrameInterval = 300 * time.Millisecond
)

type TickMsg time.Time

// CancelMsg reports operation cancellation without ending presentation lifetime.
type CancelMsg struct{}

// StopMsg is sent only after the operation returns. At freezes the final clock.
type StopMsg struct {
	Canceled bool
	At       time.Time
}

// ProgressSource supplies a non-blocking pipeline snapshot to the inline model.
type ProgressSource interface {
	Snapshot() map[ingest.Stage]ingest.StageProgress
}

type Model struct {
	state            ProgressSource
	anim             *animation.Animation
	cancel           context.CancelFunc
	frame            int
	lastAnimation    time.Time
	now              time.Time
	stopped          bool
	canceling        bool
	canceledEstimate string
	width, height    int
	presentation     Presentation
	theme            theme.Theme
}

func NewModel(state ProgressSource, anim *animation.Animation, th theme.Theme, startedAt time.Time, cancel context.CancelFunc) Model {
	m := Model{state: state, anim: anim, cancel: cancel, now: startedAt, theme: th, presentation: New(startedAt, false)}
	m.presentation.Observe(startedAt, state.Snapshot())
	return m
}

func Tick() tea.Cmd                                 { return tea.Tick(TickInterval, func(t time.Time) tea.Msg { return TickMsg(t) }) }
func (m Model) Init() tea.Cmd                       { return Tick() }
func (m Model) AvailableActions() []keymap.ActionID { return []keymap.ActionID{keymap.ActionQuit} }
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.stopped {
		return m, nil
	}
	switch msg := message.(type) {
	case tea.KeyPressMsg:
		if action, ok := keymap.Match(keymap.Default(), msg, m); ok && action == keymap.ActionQuit {
			m.beginCancel()
			if m.cancel != nil {
				m.cancel()
			}
			return m, nil
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case StopMsg:
		if msg.Canceled {
			m.beginCancel()
		}
		if !msg.At.IsZero() {
			m.now = msg.At
		}
		m.presentation.Observe(m.now, m.state.Snapshot())
		m.stopped = true
		return m, tea.Quit
	case CancelMsg:
		m.beginCancel()
	case TickMsg:
		m.now = time.Time(msg)
		m.presentation.Observe(m.now, m.state.Snapshot())
		if m.anim != nil && len(m.anim.Frames) > 0 && (m.lastAnimation.IsZero() || m.now.Sub(m.lastAnimation) >= animationFrameInterval) {
			m.frame = (m.frame + 1) % len(m.anim.Frames)
			m.lastAnimation = m.now
		}
		return m, Tick()
	}
	return m, nil
}

func (m *Model) beginCancel() {
	if !m.canceling {
		// Keep only the displayed estimate. Continue observing final counts,
		// errors and clocks while the operation acknowledges cancellation.
		m.canceledEstimate = m.presentation.estimateText()
		m.canceling = true
	}
}

func (m Model) View() tea.View {
	if m.stopped && !m.canceling {
		return tea.NewView("")
	}
	return tea.NewView(m.Render())
}
func (m Model) Render() string {
	styles := m.theme.Styles()
	lines := []string{}
	// In a short terminal, reclaim decoration before sacrificing the active
	// stage, timing roll-up or interrupt hint.
	if m.anim != nil && len(m.anim.Frames) > 0 && (m.height <= 0 || m.height >= len(m.anim.Frames[m.frame])+7) {
		for _, line := range m.anim.Frames[m.frame] {
			lines = append(lines, styles.Muted.Render(line))
		}
		lines = append(lines, "")
	}
	if m.height <= 0 || m.height > 3 {
		lines = append(lines, styles.Header.Render("local import progress"))
	}
	footer := []string{"", styles.Muted.Render("ctrl+c to cancel")}
	if m.canceling {
		status := "canceling harvest; waiting for current work to stop"
		if m.stopped {
			status = "harvest canceled"
		}
		footer[1] = styles.Muted.Render(status)
	}
	if m.height == 1 {
		footer = footer[1:]
	}
	available := -1
	if m.height > 0 {
		available = max(m.height-len(lines)-len(footer), 0)
	}
	lines = append(lines, m.presentation.lines(styles, m.now, available, m.canceledEstimate)...)
	if m.height > 0 && len(lines)+len(footer) > m.height {
		lines = lines[:max(m.height-len(footer), 0)]
	}
	lines = append(lines, footer...)
	if m.height > 0 && len(lines) > m.height {
		lines = lines[:m.height]
	}
	panel := kit.NewPanel(m.theme)
	// Inline output consumes only its content rows, unlike the full-screen
	// kickstart panel; leave the user's scrollback above it intact.
	panel.SetSize(m.width, 0)
	for _, line := range lines {
		panel.Rendered(line)
	}
	return panel.View()
}

var _ tea.Model = Model{}
var _ keymap.Availability = Model{}
var _ ProgressSource = (*ingest.ProgressState)(nil)
