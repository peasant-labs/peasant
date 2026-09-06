package ingestprogress

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	TickInterval           = time.Second / 24
	animationFrameInterval = 300 * time.Millisecond
)

type TickMsg time.Time
type StopMsg struct{}

type Model struct {
	state         *ingest.ProgressState
	anim          *animation.Animation
	cancel        context.CancelFunc
	frame         int
	lastAnimation time.Time
	now           time.Time
	stopped       bool
	width, height int
	presentation  Presentation
	theme         theme.Theme
}

func NewModel(state *ingest.ProgressState, anim *animation.Animation, th theme.Theme, startedAt time.Time, cancel context.CancelFunc) Model {
	m := Model{state: state, anim: anim, cancel: cancel, now: startedAt, theme: th, presentation: New(startedAt, false)}
	m.presentation.Observe(startedAt, state.Snapshot())
	return m
}

func Tick() tea.Cmd           { return tea.Tick(TickInterval, func(t time.Time) tea.Msg { return TickMsg(t) }) }
func (m Model) Init() tea.Cmd { return Tick() }
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
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
	case tea.BackgroundColorMsg:
		if msg.IsDark() {
			m.theme = theme.New(theme.ModeDark)
		} else {
			m.theme = theme.New(theme.ModeLight)
		}
	case StopMsg:
		m.stopped = true
		return m, tea.Quit
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
func (m Model) View() tea.View {
	if m.stopped {
		return tea.NewView("")
	}
	return tea.NewView(m.Render())
}
func (m Model) Render() string {
	styles := m.theme.Styles()
	lines := []string{}
	if m.anim != nil && len(m.anim.Frames) > 0 {
		lines = append(lines, m.anim.Frames[m.frame]...)
		lines = append(lines, "")
	}
	lines = append(lines, styles.Header.Render("local import progress"))
	footer := []string{"", styles.Muted.Render("ctrl+c to cancel")}
	available := -1
	if m.height > 0 {
		available = max(m.height-len(lines)-len(footer), 0)
	}
	lines = append(lines, m.presentation.Lines(styles, m.now, available)...)
	if m.height > 0 && len(lines)+len(footer) > m.height {
		lines = lines[:max(m.height-len(footer), 0)]
	}
	lines = append(lines, footer...)
	if m.height > 0 && len(lines) > m.height {
		lines = lines[:m.height]
	}
	panel := kit.NewPanel(m.theme)
	panel.SetSize(m.width, m.height)
	for _, line := range lines {
		panel.Rendered(line)
	}
	return panel.View()
}
func Plain(v string) string { return strings.TrimSpace(v) }

var _ tea.Model = Model{}
