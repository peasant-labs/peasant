package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// AppModel is the top-level Bubble Tea model that delegates to sub-views.
type AppModel struct {
	activeTab int
	tabNames  []string
	dashboard DashboardModel
	sessions  SessionModel
	trends    TrendsModel
	width     int
	height    int
}

// NewApp creates the top-level app model with the given sessions.
func NewApp(s []ingest.Session) AppModel {
	return AppModel{
		activeTab: 0,
		tabNames:  []string{"Dashboard", "Sessions", "Trends"},
		dashboard: NewDashboard(s),
		sessions:  NewSession(s),
		trends:    NewTrends(s),
	}
}

func (m AppModel) Init() tea.Cmd {
	return tea.RequestWindowSize
}

func (m AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Propagate to all sub-models
		m.dashboard, _ = m.dashboard.Update(msg)
		m.sessions, _ = m.sessions.Update(msg)
		m.trends, _ = m.trends.Update(msg)
		return m, nil

	case tea.KeyPressMsg:
		key := msg.String()

		// Quit is always handled
		if key == defaults.KeyQuit.String() || key == defaults.KeyInterrupt.String() {
			return m, tea.Quit
		}

		// If in session detail, only intercept esc; let session handle the rest
		if m.activeTab == 1 && m.sessions.InDetail() {
			var cmd tea.Cmd
			m.sessions, cmd = m.sessions.Update(msg)
			return m, cmd
		}

		// Global navigation
		switch key {
		case defaults.KeyTab.String():
			m.activeTab = (m.activeTab + 1) % 3
			return m, nil
		case defaults.KeyShiftTab.String():
			m.activeTab = (m.activeTab + 2) % 3
			return m, nil
		case defaults.Key1.String():
			m.activeTab = 0
			return m, nil
		case defaults.Key2.String():
			m.activeTab = 1
			return m, nil
		case defaults.Key3.String():
			m.activeTab = 2
			return m, nil
		}
	}

	// Delegate to active sub-model
	var cmd tea.Cmd
	switch m.activeTab {
	case 0:
		m.dashboard, cmd = m.dashboard.Update(msg)
	case 1:
		m.sessions, cmd = m.sessions.Update(msg)
	case 2:
		m.trends, cmd = m.trends.Update(msg)
	}
	return m, cmd
}

func (m AppModel) View() tea.View {
	// Tab bar
	var tabs []string
	for i, name := range m.tabNames {
		if i == m.activeTab {
			tabs = append(tabs, ActiveTab.Render(name))
		} else {
			tabs = append(tabs, InactiveTab.Render(name))
		}
	}
	tabBar := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)

	// Content
	var content string
	switch m.activeTab {
	case 0:
		content = m.dashboard.View()
	case 1:
		content = m.sessions.View()
	case 2:
		content = m.trends.View()
	}

	// Constrain content height
	contentHeight := m.height - 3
	if contentHeight < 1 {
		contentHeight = 1
	}
	content = lipgloss.NewStyle().Height(contentHeight).MaxHeight(contentHeight).Background(baseBg).Render(content)

	// Help bar
	helpText := "tab/shift-tab: switch • 1-3: jump • q: quit"
	if m.activeTab == 1 && m.sessions.InDetail() {
		helpText = "esc: back • ↑↓: scroll • q: quit"
	} else if m.activeTab == 2 {
		helpText = "↑↓/j/k: scroll • tab/shift-tab: switch • q: quit"
	}
	helpBar := HelpStyle.Render(helpText)

	result := lipgloss.JoinVertical(lipgloss.Left, tabBar, content, helpBar)
	view := tea.NewView(FillBg(ContentBg.Width(m.width).Height(m.height).Render(result)))
	view.AltScreen = true
	return view
}
