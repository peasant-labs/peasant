// Package pullprogress owns the bulk collective-pull progress surface.
//
// A bulk pull walks many transcripts in sequence, so the surface is one
// progress row with a done/total count and an elapsed readout, framed by a
// header naming the run and a footer naming the interrupt. It composes only
// the layout primitives in internal/tui/kit: the bar row comes from
// kit.ProgressMatrix and the block is fitted through kit.Panel, so this
// surface never pads, places, aligns, or paints a background itself.
package pullprogress

import (
	"time"

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
// stopping: the current transcript is still unwinding. It stays terse
// chrome like the other footers so it never wraps early at 80 columns.
const pullCanceling = "canceling pull..."

// pullCanceled names the retained final state of an interrupted run.
const pullCanceled = "pull canceled"

// Model is the bulk-pull progress surface: one done/total row with an
// elapsed readout in a panel with a header and an interrupt footer. Done
// counts completed transcripts (pulled, up to date, or failed); total is
// the listed cohort size; elapsed is the run time the reporter hands over
// after each transcript, so a slow run shows whether to wait or cancel.
type Model struct {
	theme     theme.Theme
	done      int
	total     int
	elapsed   time.Duration
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

// SetElapsed sets the run time shown beside the count. It is clamped at
// zero; callers pass the time since the run started after each transcript.
// The surface owns no clock, so captures stay deterministic.
func (m *Model) SetElapsed(d time.Duration) {
	if d < 0 {
		d = 0
	}
	m.elapsed = d
}

// Elapsed reports the run time shown beside the count.
func (m Model) Elapsed() time.Duration { return m.elapsed }

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

// View renders the header, the done/total bar row with its elapsed
// readout, and the interrupt footer through one kit panel, so every line
// shares one width and one background in both themes. The bar row follows
// the ingest pattern: kit.ProgressMatrix owns the row and the duration
// column, the bar paints in the base role, and the elapsed readout paints
// in the muted role.
func (m Model) View() string {
	styles := m.theme.Styles()
	complete := m.total > 0 && m.done >= m.total
	steady := !m.canceling && !m.canceled
	ended := complete && steady
	lines := kit.ProgressMatrix([]kit.ProgressRow{{
		Label:   pullLabel,
		Done:    m.done,
		Total:   m.total,
		Ended:   ended,
		Elapsed: "elapsed: " + formatPullElapsed(m.elapsed),
	}})
	bar := styles.Base.Render(lines[0].Bar)
	if lines[0].Elapsed != "" {
		bar += styles.Muted.Render("  " + lines[0].Elapsed)
	}
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
	panel.Rendered(bar)
	panel.Rendered(styles.Muted.Render(footer))
	return panel.View()
}

// formatPullElapsed renders a run time the way the ingest surfaces do: whole
// seconds, never negative.
func formatPullElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
