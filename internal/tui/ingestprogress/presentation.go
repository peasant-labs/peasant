// Package ingestprogress owns the presentation-only state shared by local
// ingest surfaces. It observes pipeline progress without feeding anything back
// into the pipeline.
package ingestprogress

import (
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const estimateWindow = 5 * time.Second

type observation struct {
	startedAt        time.Time
	lastAt           time.Time
	lastDone         int
	lastTotal        int
	progress         ingest.StageProgress
	estimateEligible bool
	estimateValid    bool
	estimate         time.Duration
	estimator        kit.Estimator
}

// Presentation records elapsed clocks and qualified estimates for one ingest
// attempt. Its zero value is ready to use after Reset.
type Presentation struct {
	startedAt    time.Time
	retry        bool
	observations map[ingest.Stage]observation
}

// New starts a presentation clock for one ingest attempt.
func New(startedAt time.Time, retry bool) Presentation {
	return Presentation{startedAt: startedAt, retry: retry, observations: map[ingest.Stage]observation{}}
}

// StartedAt returns the attempt clock anchor.
func (p Presentation) StartedAt() time.Time { return p.startedAt }

// Reset starts a new attempt and discards all volatile timing observations.
func (p *Presentation) Reset(startedAt time.Time, retry bool) {
	*p = New(startedAt, retry)
}

// Observe records one snapshot. Estimates require a stable positive total and
// monotonic progress; only the focused stage computes an estimate.
func (p *Presentation) Observe(at time.Time, snapshot map[ingest.Stage]ingest.StageProgress) {
	if p.observations == nil {
		p.observations = map[ingest.Stage]observation{}
	}
	focus, hasFocus := p.Focus()
	for _, stage := range ingest.StageOrder {
		sp := snapshot[stage]
		if !sp.Started {
			continue
		}
		o, seen := p.observations[stage]
		if !seen {
			o = observation{startedAt: at, lastAt: at, lastDone: sp.Done, lastTotal: sp.Total, progress: sp,
				estimateEligible: sp.Total > 0 && !sp.HasErr && !p.retry, estimator: kit.NewEstimator(estimateWindow)}
			o.estimator.Estimate(at, sp.Done, sp.Total)
			p.observations[stage] = o
			continue
		}
		if o.progress.Ended {
			continue
		}
		o.estimateValid = false
		if sp.Total != o.lastTotal {
			o.lastAt, o.lastDone, o.lastTotal, o.progress = at, sp.Done, sp.Total, sp
			o.estimateEligible = sp.Total > 0 && !sp.HasErr && !p.retry
			o.estimator.Estimate(at, sp.Done, sp.Total)
			p.observations[stage] = o
			continue
		}
		if sp.Total <= 0 || sp.HasErr || p.retry || sp.Done < o.lastDone {
			o.estimateEligible = false
		}
		eta, ok := o.estimator.Estimate(at, sp.Done, sp.Total)
		if o.estimateEligible && hasFocus && stage == focus && !sp.Ended && sp.Done < sp.Total && ok {
			o.estimate, o.estimateValid = eta, true
		}
		o.lastAt, o.lastDone, o.lastTotal, o.progress = at, sp.Done, sp.Total, sp
		p.observations[stage] = o
	}
}

// Focus returns the latest failure, otherwise latest active stage, otherwise
// latest completed stage.
func (p Presentation) Focus() (ingest.Stage, bool) {
	var latest, active, failed ingest.Stage
	var hasLatest, hasActive, hasFailed bool
	for _, stage := range ingest.StageOrder {
		o, ok := p.observations[stage]
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

// Lines renders the canonical lower-case stage matrix and timing roll-up. The
// height budget is optional; when constrained, focus and detail are retained.
func (p Presentation) Lines(styles theme.Styles, now time.Time, height int) []string {
	return p.lines(styles, now, height, "")
}

func (p Presentation) estimateText() string {
	if focus, ok := p.Focus(); ok && p.observations[focus].estimateValid {
		return "  estimate: " + DisplayDuration(p.observations[focus].estimate)
	}
	return "  estimate unavailable"
}

// retainedEstimate is used only by the canceled inline surface; other consumers
// continue to render the live estimate, including its normal stall expiration.
func (p Presentation) lines(styles theme.Styles, now time.Time, height int, retainedEstimate string) []string {
	if now.Before(p.startedAt) {
		now = p.startedAt
	}
	focus, hasFocus := p.Focus()
	rows := make([]kit.ProgressRow, 0, len(ingest.StageOrder))
	focusIdx := -1
	for _, stage := range ingest.StageOrder {
		sp := ingest.StageProgress{}
		row := kit.ProgressRow{Label: strings.ToLower(stage.String())}
		if o, ok := p.observations[stage]; ok && o.progress.Started {
			sp = o.progress
			end := now
			if sp.Ended && o.lastAt.Before(end) {
				end = o.lastAt
			}
			row.Elapsed = DisplayDuration(end.Sub(o.startedAt))
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
	if retainedEstimate == "" {
		retainedEstimate = p.estimateText()
	}
	detail := []string{styles.Muted.Render("  total elapsed: " + DisplayDuration(now.Sub(p.startedAt))), styles.Muted.Render(retainedEstimate)}
	lines = append(lines, detail...)
	if height < 0 || len(lines) <= height {
		return lines
	}
	if height == 0 {
		return nil
	}
	selected, used := map[int]bool{}, 0
	if focusIdx >= 0 && used < height {
		selected[focusIdx], used = true, used+1
	}
	useDetail := used+len(detail) <= height
	if useDetail {
		used += len(detail)
	}
	for i := len(rows) - 1; i >= 0 && used < height; i-- {
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
	return window
}

// DisplayDuration keeps progress clocks stable and compact.
func DisplayDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
