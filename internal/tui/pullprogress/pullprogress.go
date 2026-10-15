// Package pullprogress owns the bulk collective-pull progress surface.
//
// A bulk pull walks many transcripts in sequence, so the surface is one
// progress row with a done/total count, framed by a header naming the run
// and a footer naming the interrupt. It composes only the layout primitives
// in internal/tui/kit: the bar row comes from kit.ProgressBar and the block
// is fitted through kit.Panel, so this surface never pads, places, aligns,
// or paints a background itself.
package pullprogress

import (
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// pullLabel is the single progress-row label. Harvest passes raw stage
// names; a bulk pull has one stage, so one fixed lowercase label covers it.
const pullLabel = "pull"

// pullHeader names the run in lowercase chrome.
const pullHeader = "collective pull progress"

// pullCancelHint is the steady-state footer. It mirrors the harvest
// interrupt hint so every long run names the same key.
const pullCancelHint = "ctrl+c to cancel"

// pullCanceling names the window between the interrupt and the run
// stopping: the current transcript is still unwinding.
const pullCanceling = "canceling pull; waiting for current work to stop"

// pullCanceled names the retained final state of an interrupted run.
const pullCanceled = "pull canceled"

// Model is the bulk-pull progress surface: one done/total row in a panel
// with a header and an interrupt footer. Done counts completed transcripts
// (pulled, up to date, or failed); total is the listed cohort size.
type Model struct {
	theme     theme.Theme
	done      int
	total     int
	width     int
	canceling bool
	canceled  bool
}

// New builds a Model over theme t.
func New(t theme.Theme) Model {
	return Model{theme: t}
}

// SetTotal sets the listed cohort size. A non-positive total means the
// cohort is not known yet; the bar then shows no count until it is set.
func (m *Model) SetTotal(total int) {
	if total < 0 {
		total = 0
	}
	m.total = total
}

// SetDone sets the completed count. It is clamped at zero; callers pass
// the cumulative completed outcomes after each transcript.
func (m *Model) SetDone(done int) {
	if done < 0 {
		done = 0
	}
	m.done = done
}

// Done reports the completed count.
func (m Model) Done() int { return m.done }

// Total reports the listed cohort size.
func (m Model) Total() int { return m.total }

// MarkCanceling retains the interrupt acknowledgment while the current
// transcript unwinds. The bar keeps its counts; only the footer changes.
func (m *Model) MarkCanceling() {
	m.canceling = true
}

// MarkCanceled retains the interrupted final state. The bar keeps the
// counts reached before the stop; the footer names the cancellation.
func (m *Model) MarkCanceled() {
	m.canceling = false
	m.canceled = true
}

// SetSize sets the panel width the surface draws into. Height is always
// derived from the three content rows, so the height argument is accepted
// for the kit.Sizeable capability and ignored.
func (m *Model) SetSize(width, _ int) {
	if width < 0 {
		width = 0
	}
	m.width = width
}

var _ kit.Sizeable = (*Model)(nil)

// View renders the header, the done/total bar row, and the interrupt
// footer through one kit panel, so every line shares one width and one
// background in both themes.
func (m Model) View() string {
	styles := m.theme.Styles()
	complete := m.total > 0 && m.done >= m.total
	steady := !m.canceling && !m.canceled
	ended := complete && steady
	bar := kit.ProgressBar(pullLabel, m.done, m.total, ended, false)
	footer := pullCancelHint
	if m.canceling {
		footer = pullCanceling
	}
	if m.canceled {
		footer = pullCanceled
	}
	panel := kit.NewPanel(m.theme)
	panel.SetSize(m.width, 0)
	panel.Rendered(styles.Header.Render(pullHeader))
	panel.Rendered(styles.Base.Render(bar))
	panel.Rendered(styles.Muted.Render(footer))
	return panel.View()
}
