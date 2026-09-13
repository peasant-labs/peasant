// Package ingestprogress owns the data-only progress child shared by local
// ingest surfaces.
package ingestprogress

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const estimateWindow = 5 * time.Second

type FinalOutcome uint8

const (
	FinalSucceeded FinalOutcome = iota + 1
	FinalCanceled
	FinalFailed
)

type Options struct {
	Theme     theme.Theme
	StartedAt time.Time
	Retry     bool
}

type ObserveMsg struct {
	At              time.Time
	Snapshot        map[ingest.Stage]ingest.StageProgress
	CancelRequested bool
}

type CancelRequestedMsg struct{ At time.Time }

type FinalMsg struct {
	At       time.Time
	Snapshot map[ingest.Stage]ingest.StageProgress
	Outcome  FinalOutcome
}

type estimate struct {
	value time.Duration
	ok    bool
}

type observation struct {
	startedAt, lastAt   time.Time
	lastDone, lastTotal int
	progress            ingest.StageProgress
	estimateEligible    bool
	estimate            estimate
	estimator           kit.Estimator
}

type Model struct {
	theme                    theme.Theme
	startedAt, now, latestAt time.Time
	retry, final             bool
	width, height            int
	observations             map[ingest.Stage]observation
	retainedEstimate         estimate
	retaining                bool
}

func New(options Options) Model {
	return Model{theme: options.Theme, startedAt: options.StartedAt, now: options.StartedAt,
		retry: options.Retry, observations: map[ingest.Stage]observation{}}
}

func (m *Model) SetSize(width, height int) { m.width, m.height = width, height }

func (m Model) Update(message tea.Msg) (Model, tea.Cmd) {
	if m.final {
		return m, nil
	}
	switch msg := message.(type) {
	case ObserveMsg:
		if !m.latestAt.IsZero() && msg.At.Before(m.latestAt) {
			return m, nil
		}
		if msg.CancelRequested {
			m.retainEstimate()
		}
		m.observe(msg.At, msg.Snapshot)
	case CancelRequestedMsg:
		m.retainEstimate()
	case FinalMsg:
		if msg.Outcome == FinalCanceled {
			m.retainEstimate()
		} else {
			m.retaining = false
			m.retainedEstimate = estimate{}
		}
		m.applyFinal(msg.At, msg.Snapshot)
		m.final = true
	}
	return m, nil
}

func (m *Model) applyFinal(at time.Time, snapshot map[ingest.Stage]ingest.StageProgress) {
	previous := m.observations
	m.observations = map[ingest.Stage]observation{}
	m.now, m.latestAt = at, at
	for _, stage := range ingest.StageOrder {
		sp := snapshot[stage]
		if !sp.Started {
			continue
		}
		startedAt, lastAt := at, at
		if old, ok := previous[stage]; ok {
			if old.startedAt.Before(at) {
				startedAt = old.startedAt
			}
			// Final data replaces speculative counts and errors, but a stage
			// already ended before completion must not acquire the remaining
			// operation time. Cap later speculative end times at completion.
			if old.progress.Ended && sp.Ended && old.lastAt.Before(at) {
				lastAt = old.lastAt
			}
		}
		m.observations[stage] = observation{startedAt: startedAt, lastAt: lastAt, lastDone: sp.Done,
			lastTotal: sp.Total, progress: sp, estimator: kit.NewEstimator(estimateWindow)}
	}
}

func (m *Model) retainEstimate() {
	if m.retaining {
		return
	}
	m.retainedEstimate = m.currentEstimate()
	m.retaining = true
}

func (m *Model) observe(at time.Time, snapshot map[ingest.Stage]ingest.StageProgress) {
	m.now, m.latestAt = at, at
	focus, hasFocus := m.FocusedStage()
	for _, stage := range ingest.StageOrder {
		sp := snapshot[stage]
		if !sp.Started {
			continue
		}
		o, seen := m.observations[stage]
		if !seen {
			o = observation{startedAt: at, lastAt: at, lastDone: sp.Done, lastTotal: sp.Total, progress: sp,
				estimateEligible: sp.Total > 0 && !sp.HasErr && !m.retry, estimator: kit.NewEstimator(estimateWindow)}
			o.estimator.Estimate(at, sp.Done, sp.Total)
			m.observations[stage] = o
			continue
		}
		if o.progress.Ended {
			continue
		}
		o.estimate.ok = false
		if sp.Total != o.lastTotal {
			o.lastAt, o.lastDone, o.lastTotal, o.progress = at, sp.Done, sp.Total, sp
			o.estimateEligible = sp.Total > 0 && !sp.HasErr && !m.retry
			o.estimator.Estimate(at, sp.Done, sp.Total)
			m.observations[stage] = o
			continue
		}
		if sp.Total <= 0 || sp.HasErr || m.retry || sp.Done < o.lastDone {
			o.estimateEligible = false
		}
		eta, ok := o.estimator.Estimate(at, sp.Done, sp.Total)
		if o.estimateEligible && hasFocus && stage == focus && !sp.Ended && sp.Done < sp.Total && ok {
			o.estimate = estimate{value: eta, ok: true}
		}
		o.lastAt, o.lastDone, o.lastTotal, o.progress = at, sp.Done, sp.Total, sp
		m.observations[stage] = o
	}
}

func (m Model) FocusedStage() (ingest.Stage, bool) {
	var latest, active, failed ingest.Stage
	var hasLatest, hasActive, hasFailed bool
	for _, stage := range ingest.StageOrder {
		o, ok := m.observations[stage]
		if !ok || !o.progress.Started {
			continue
		}
		latest, hasLatest = stage, true
		if !o.progress.Ended {
			active, hasActive = stage, true
		}
		if o.progress.HasErr {
			failed, hasFailed = stage, true
		}
	}
	if hasFailed {
		return failed, true
	}
	if hasActive {
		return active, true
	}
	return latest, hasLatest
}

func (m Model) currentEstimate() estimate {
	if focus, ok := m.FocusedStage(); ok {
		return m.observations[focus].estimate
	}
	return estimate{}
}

func (m Model) View() string {
	now := m.now
	if now.Before(m.startedAt) {
		now = m.startedAt
	}
	styles := m.theme.Styles()
	focus, hasFocus := m.FocusedStage()
	rows := make([]kit.ProgressRow, 0, len(ingest.StageOrder))
	focusIdx := -1
	for _, stage := range ingest.StageOrder {
		sp := ingest.StageProgress{}
		row := kit.ProgressRow{Label: strings.ToLower(stage.String())}
		if o, ok := m.observations[stage]; ok && o.progress.Started {
			sp = o.progress
			end := now
			if sp.Ended && o.lastAt.Before(end) {
				end = o.lastAt
			}
			row.Elapsed = displayDuration(end.Sub(o.startedAt))
		}
		row.Done, row.Total, row.Ended, row.HasErr = sp.Done, sp.Total, sp.Ended, sp.HasErr
		if hasFocus && stage == focus {
			focusIdx = len(rows)
		}
		rows = append(rows, row)
	}
	lines := make([]string, 0, len(rows)+2)
	for _, line := range kit.ProgressMatrix(rows) {
		rendered := styles.Base.Render(line.Bar)
		if line.Elapsed != "" {
			rendered += styles.Muted.Render("  " + line.Elapsed)
		}
		lines = append(lines, rendered)
	}
	estimate := m.currentEstimate()
	if m.retaining {
		estimate = m.retainedEstimate
	}
	estimateText := "  estimate unavailable"
	if estimate.ok {
		estimateText = "  estimate: " + displayDuration(estimate.value)
	}
	detail := []string{styles.Muted.Render("  total elapsed: " + displayDuration(now.Sub(m.startedAt))), styles.Muted.Render(estimateText)}
	lines = append(lines, detail...)
	if m.height < 0 || len(lines) <= m.height {
		return strings.Join(lines, "\n")
	}
	if m.height == 0 {
		return ""
	}
	selected, used := map[int]bool{}, 0
	if focusIdx >= 0 && used < m.height {
		selected[focusIdx], used = true, used+1
	}
	useDetail := used+len(detail) <= m.height
	if useDetail {
		used += len(detail)
	}
	for i := len(rows) - 1; i >= 0 && used < m.height; i-- {
		if !selected[i] {
			selected[i], used = true, used+1
		}
	}
	window := make([]string, 0, used)
	for i := range rows {
		if selected[i] {
			window = append(window, lines[i])
		}
	}
	if useDetail {
		window = append(window, detail...)
	}
	return strings.Join(window, "\n")
}

func displayDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

var _ kit.Sizeable = (*Model)(nil)
