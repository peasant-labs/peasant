// Package ingestprogress owns the data-only progress child shared by local
// ingest surfaces.
package ingestprogress

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

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
	// Waiting is why the run has not started, such as another harvest
	// holding the data dir's lock (ingest.ProgressState.Waiting); empty when
	// it is not waiting.
	Waiting string
	// Lane is nil when the pipeline has not exposed an instrumented writer.
	Lane *ingest.WriteLaneProgress
}

// LaneSnapshot reads the existing pipeline recorder without requiring every
// progress source to implement writer-lane reporting.
func LaneSnapshot(source any) *ingest.WriteLaneProgress {
	reader, ok := source.(interface {
		WriteLaneSnapshot() (ingest.WriteLaneProgress, bool)
	})
	if !ok {
		return nil
	}
	lane, available := reader.WriteLaneSnapshot()
	if !available {
		return nil
	}
	return &lane
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
	byteEstimate        bool
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
	// waiting is the latest observed reason the run has not started.
	waiting         string
	lane            *ingest.WriteLaneProgress
	candidateCount  int
	candidatesKnown bool
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
		m.waiting = msg.Waiting
		m.lane = msg.Lane
		candidate := msg.Snapshot[ingest.StageExtract]
		m.candidateCount, m.candidatesKnown = candidate.CandidateCount, candidate.CandidatesKnown
		m.observe(msg.At, msg.Snapshot)
	case CancelRequestedMsg:
		m.retainEstimate()
	case FinalMsg:
		m.lane = nil
		m.waiting = ""
		candidate := msg.Snapshot[ingest.StageExtract]
		m.candidateCount, m.candidatesKnown = candidate.CandidateCount, candidate.CandidatesKnown
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
			lastTotal: sp.Total, progress: sp, estimator: kit.NewEstimator(kit.EstimateWindow)}
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
		done, total, bytes := estimateProgress(sp)
		o, seen := m.observations[stage]
		if !seen {
			o = observation{startedAt: at, lastAt: at, lastDone: done, lastTotal: total, byteEstimate: bytes, progress: sp,
				estimateEligible: total > 0 && !sp.HasErr && !m.retry, estimator: kit.NewEstimator(kit.EstimateWindow)}
			o.estimator.Estimate(at, done, total)
			m.observations[stage] = o
			continue
		}
		if o.progress.Ended {
			continue
		}
		o.estimate.ok = false
		if total != o.lastTotal || bytes != o.byteEstimate {
			if bytes != o.byteEstimate {
				o.estimator = kit.NewEstimator(kit.EstimateWindow)
			}
			o.lastAt, o.lastDone, o.lastTotal, o.byteEstimate, o.progress = at, done, total, bytes, sp
			o.estimateEligible = total > 0 && !sp.HasErr && !m.retry
			o.estimator.Estimate(at, done, total)
			m.observations[stage] = o
			continue
		}
		if total <= 0 || sp.HasErr || m.retry || done < o.lastDone {
			o.estimateEligible = false
		}
		eta, ok := o.estimator.Estimate(at, done, total)
		if o.estimateEligible && hasFocus && stage == focus && !sp.Ended && done < total && ok {
			o.estimate = estimate{value: eta, ok: true}
		}
		o.lastAt, o.lastDone, o.lastTotal, o.progress = at, done, total, sp
		m.observations[stage] = o
	}
}

func estimateProgress(sp ingest.StageProgress) (int, int, bool) {
	if sp.BytesTotal > 0 {
		// Scaling to KiB on narrow platforms avoids integer overflow.
		maxInt := int64(^uint(0) >> 1)
		scale := int64(1)
		if sp.BytesTotal > maxInt {
			scale = sp.BytesTotal/maxInt + 1
		}
		return int(sp.BytesDone / scale), int(sp.BytesTotal / scale), true
	}
	return sp.Done, sp.Total, false
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
	for i, line := range kit.ProgressMatrix(rows) {
		rendered := styles.Base.Render(line.Bar)
		sp := m.observations[ingest.StageOrder[i]].progress
		if sp.BytesTotal > 0 {
			rendered += styles.Muted.Render("  " + defaults.HumanByteSize(sp.BytesDone) + "/" + defaults.HumanByteSize(sp.BytesTotal))
		}
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
	detail := []string{}
	if m.candidatesKnown {
		detail = append(detail, styles.Muted.Render(fmt.Sprintf("%sextract candidates: %d %s (not confirmed refreshes)", kit.Indent(2), m.candidateCount, ingest.SessionWord(m.candidateCount))))
	}
	if m.lane != nil {
		busy := "busy unknown"
		if m.lane.Elapsed > 0 {
			fraction := float64(m.lane.Busy) / float64(m.lane.Elapsed)
			busy = fmt.Sprintf("%.0f%% busy since run start", min(max(fraction, 0), 1)*100)
		}
		detail = append(detail, styles.Muted.Render(fmt.Sprintf("%swriter lane: %d waiting · %s", kit.Indent(2), m.lane.Queued, busy)))
	}
	detail = append(detail, styles.Muted.Render("  total elapsed: "+displayDuration(now.Sub(m.startedAt))), styles.Muted.Render(estimateText))
	if m.waiting != "" {
		detail = append([]string{styles.Base.Render("  " + m.waiting)}, detail...)
	}
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
