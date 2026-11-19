// Package upgradeprogress owns the data-only terminal progress view for the
// explicit store upgrade.
//
// The store's Upgrade reports one UpgradeEvent per step moment. This package
// turns those events into the three answers the screen owes a person: that an
// upgrade is running (the header and the current step), what remains (the
// step list), and how much is done (a counted bar where the step reports a
// count, or elapsed time and write-ahead-log growth where it cannot). It
// renders no fraction for a step the store cannot count.
//
// Every terminal row is composed from the layout primitives in
// internal/tui/kit: ProgressMatrix owns the counted bar row, Panel owns the
// fitted block, and FitCell owns the two-column step line.
package upgradeprogress

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/mdrender"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// Outcome is how a finished upgrade ended. The set is closed.
type Outcome uint8

const (
	// OutcomeDone means every planned step committed.
	OutcomeDone Outcome = iota + 1
	// OutcomeStoppedByUser means the context was cancelled.
	OutcomeStoppedByUser
	// OutcomeRefusedLowDisk means the upgrade refused before its first step
	// because the filesystem reported too little free space.
	OutcomeRefusedLowDisk
	// OutcomeRefusedConsent means the migration consent was declined.
	OutcomeRefusedConsent
	// OutcomeFailedStep means the command failed.
	OutcomeFailedStep
)

// AllOutcomes is the closed outcome set, in declaration order.
var AllOutcomes = []Outcome{OutcomeDone, OutcomeStoppedByUser, OutcomeRefusedLowDisk, OutcomeRefusedConsent, OutcomeFailedStep}

// String returns the reader-facing lowercase name of an outcome.
func (o Outcome) String() string {
	switch o {
	case OutcomeDone:
		return "done"
	case OutcomeStoppedByUser:
		return "stopped by user"
	case OutcomeRefusedLowDisk:
		return "refused for low disk"
	case OutcomeRefusedConsent:
		return "refused consent"
	case OutcomeFailedStep:
		return "failed step"
	default:
		return "unknown"
	}
}

// stopped reports whether a cancellation or failure can mark an active step.
func (o Outcome) stopped() bool {
	return o == OutcomeStoppedByUser || o == OutcomeFailedStep
}

// outcomesByName is the closed set indexed by its reader-facing names. It is
// built once from AllOutcomes so a name and its typed outcome cannot drift
// apart, and validation is a typed map lookup rather than a comparison
// against a rendered string.
var outcomesByName = func() map[string]Outcome {
	names := make(map[string]Outcome, len(AllOutcomes))
	for _, outcome := range AllOutcomes {
		names[outcome.String()] = outcome
	}
	return names
}()

// NewOutcome validates a raw outcome at an input boundary.
func NewOutcome(raw string) (Outcome, error) {
	if outcome, ok := outcomesByName[raw]; ok {
		return outcome, nil
	}
	return 0, fmt.Errorf("upgradeprogress.NewOutcome: value %q is outside the closed outcome set; use \"done\", \"stopped by user\", \"refused for low disk\", \"refused consent\", or \"failed step\"", raw)
}

// Options configures a Model. The plan is what the store is about to do; the
// events fill it in.
type Options struct {
	Theme theme.Theme
	Plan  store.UpgradePlan
}

// EventMsg carries one store upgrade event.
type EventMsg struct{ Event store.UpgradeEvent }

// FinalMsg carries what the upgrade did and how it ended.
type FinalMsg struct {
	Report  store.UpgradeReport
	Outcome Outcome
	Err     error
}

// estimateEpoch is the fixed time origin the estestimator samples use. The
// event's elapsed duration is the only time coordinate, so the estimate is a
// pure function of the events and a capture renders it deterministically.
var estimateEpoch = time.Unix(0, 0)

// stepState is what the model retains for one planned step.
type stepState struct {
	started, finished bool
	last              store.UpgradeEvent
	estimator         kit.Estimator
	estimate          time.Duration
	hasEstimate       bool
}

// Model is the upgrade progress child. It holds the plan, the latest event of
// each step, and the final report; it renders no stored state.
type Model struct {
	theme            theme.Theme
	plan             store.UpgradePlan
	steps            map[int]*stepState
	width, height    int
	final            bool
	outcome          Outcome
	report           store.UpgradeReport
	err              error
	spinner          kit.Spinner
	spinnerRequested bool
}

// New builds a Model for one plan. Elapsed time comes from the events, so the
// model reads no clock and a capture is deterministic.
func New(options Options) Model {
	return Model{
		theme:   options.Theme,
		plan:    options.Plan,
		steps:   map[int]*stepState{},
		spinner: kit.NewSpinner(options.Theme, ""),
	}
}

// SetSize sets the terminal region the surface draws into.
func (m *Model) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	m.width, m.height = width, height
}

var _ kit.Sizeable = (*Model)(nil)

// Init starts no animation: the spinner ticks only while a long uncounted
// step is running.
func (m Model) Init() tea.Cmd { return nil }

// Update applies one event, the final report, a resize, or a spinner frame.
func (m Model) Update(message tea.Msg) (Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil
	case EventMsg:
		return m.applyEvent(msg.Event)
	case FinalMsg:
		m.applyFinal(msg)
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if !m.runningLongUncounted() {
			// The spinner animates only while a long uncounted step runs; a
			// finished step must not keep the renderer awake.
			m.spinnerRequested = false
			return m, nil
		}
		return m, cmd
	default:
		return m, nil
	}
}

func (m Model) applyEvent(event store.UpgradeEvent) (Model, tea.Cmd) {
	if event.Index < 1 || event.Index > len(m.plan.Steps) {
		return m, nil
	}
	state := m.steps[event.Index]
	if state == nil {
		state = &stepState{estimator: kit.NewEstimator(kit.EstimateWindow)}
		m.steps[event.Index] = state
	}
	state.last = event
	switch event.Kind {
	case store.UpgradeEventStepStarted:
		state.started = true
	case store.UpgradeEventStepFinished:
		state.finished = true
	}
	if event.Step.Counts != store.UpgradeUnitNone && event.Kind == store.UpgradeEventStepProgress {
		// The elapsed duration is the time coordinate: the model reads no
		// clock, so a capture with the same events renders the same estimate.
		done, total := int(event.Done), int(event.Of)
		if estimate, ok := state.estimator.Estimate(estimateEpoch.Add(event.Elapsed), done, total); ok && !state.finished && done < total {
			state.estimate, state.hasEstimate = estimate, true
		}
	}
	if !m.runningLongUncounted() {
		m.spinnerRequested = false
		return m, nil
	}
	if !m.spinnerRequested {
		m.spinnerRequested = true
		return m, m.spinner.Tick()
	}
	return m, nil
}

func (m *Model) applyFinal(msg FinalMsg) {
	m.final = true
	m.outcome = msg.Outcome
	m.report = msg.Report
	m.err = msg.Err
	m.spinnerRequested = false
}

// runningLongUncounted reports whether a started, unfinished step is long and
// reports no count, which is the one case that shows a spinner.
func (m Model) runningLongUncounted() bool {
	for index, state := range m.steps {
		if !state.started || state.finished {
			continue
		}
		step := m.plan.Steps[index-1]
		if step.Cost.Long() && step.Counts == store.UpgradeUnitNone {
			return true
		}
	}
	return false
}

// stoppedStep is the step that was running when a stop or failure ended the
// upgrade. It is the last started step without a finished event.
func (m Model) stoppedStep() int {
	for index := 1; index <= len(m.plan.Steps); index++ {
		state := m.steps[index]
		if state != nil && state.started && !state.finished {
			return index
		}
	}
	return 0
}

// activeStep is the step the view focuses: a started, unfinished step; else
// the step that stopped the upgrade; else the next pending step; else the
// last planned step.
func (m Model) activeStep() int {
	if index := m.stoppedStep(); index != 0 {
		return index
	}
	for index := 1; index <= len(m.plan.Steps); index++ {
		if state := m.steps[index]; state == nil || !state.finished {
			return index
		}
	}
	if len(m.plan.Steps) == 0 {
		return 0
	}
	return len(m.plan.Steps)
}

// totalElapsed sums the finished steps' durations and the running step's
// elapsed time. A capture supplies both through events.
func (m Model) totalElapsed() time.Duration {
	var total time.Duration
	for _, state := range m.steps {
		if state.started && state.last.Elapsed > 0 {
			total += state.last.Elapsed
		}
	}
	return total
}

// View renders the header, the step list, and the footer through one kit
// panel. It occupies only its content rows; the caller owns the surrounding
// terminal.
func (m Model) View() string {
	styles := m.theme.Styles()
	header := styles.Header.Render(m.headerLine())
	footer := m.footerLines()
	allRows := m.stepRows()
	reserved := 1 + len(footer)
	windowRows := false
	if m.height > 0 && len(allRows)+reserved > m.height {
		// Reserve the row the hidden-steps line needs before selecting rows,
		// so a windowed plan never drops that line for lack of space.
		reserved++
		windowRows = true
	}
	rows, hidden := m.windowRows(allRows, reserved)
	lines := make([]string, 0, 2+len(rows)+len(footer))
	lines = append(lines, header)
	if windowRows && hidden > 0 {
		lines = append(lines, styles.Muted.Render(fmt.Sprintf("  … %d earlier step(s)", hidden)))
	}
	lines = append(lines, rows...)
	lines = append(lines, footer...)
	panel := kit.NewPanel(m.theme)
	panel.SetSize(m.width, 0)
	for _, line := range lines {
		panel.Rendered(line)
	}
	return panel.View()
}

func (m Model) headerLine() string {
	var header string
	switch {
	case m.plan.From == m.plan.To && len(m.plan.Steps) > 0:
		header = fmt.Sprintf("upgrading the local store · schema %d · %d owed step(s)", m.plan.From, len(m.plan.Steps))
	case m.plan.From == m.plan.To:
		header = fmt.Sprintf("upgrading the local store · schema %d", m.plan.From)
	default:
		header = fmt.Sprintf("upgrading the local store · schema %d → %d", m.plan.From, m.plan.To)
	}
	if m.plan.StoreBytes > 0 {
		header += " · " + formatBytes(m.plan.StoreBytes)
	}
	return header
}

// stepRows renders one line per planned step, plus the active step's detail
// line when the step reports a count or is a running long uncounted step.
func (m Model) stepRows() []string {
	if len(m.plan.Steps) == 0 {
		return []string{m.theme.Styles().Muted.Render("  no steps to run")}
	}
	rows := make([]string, 0, len(m.plan.Steps))
	for index := 1; index <= len(m.plan.Steps); index++ {
		step := m.plan.Steps[index-1]
		row, detail := m.stepLine(index, step, m.steps[index])
		rows = append(rows, row)
		if detail != "" {
			rows = append(rows, detail)
		}
	}
	return rows
}

func (m Model) stepLine(index int, step store.UpgradeStep, state *stepState) (string, string) {
	icon, iconStyle := m.stepIcon(index, state)
	right := fmt.Sprintf("step %d of %d", index, len(m.plan.Steps))
	detail := ""
	if state != nil && (state.started || state.finished) {
		right += "  " + displayDuration(state.last.Elapsed)
		if !state.finished {
			detail = m.runningDetail(step, state)
		}
	} else if step.Cost.Long() && step.Counts == store.UpgradeUnitNone {
		right += "  long"
	}
	return m.fitRow(icon+" "+step.Title, iconStyle, right), detail
}

// fitRow composes the step line from two fitted cells. right is ASCII, so its
// byte length is its display width.
func (m Model) fitRow(left string, leftStyle lipgloss.Style, right string) string {
	styles := m.theme.Styles()
	if m.width <= 0 {
		return leftStyle.Render(left) + "  " + styles.Muted.Render(right)
	}
	rightWidth := len(right)
	leftWidth := m.width - rightWidth - 2
	if leftWidth < 12 {
		leftWidth = 12
	}
	return leftStyle.Render(kit.FitCell(left, leftWidth)) + "  " + styles.Muted.Render(kit.FitCell(right, rightWidth))
}

func (m Model) stepIcon(index int, state *stepState) (string, lipgloss.Style) {
	styles := m.theme.Styles()
	if state == nil || !state.started {
		return "○", styles.Muted
	}
	if state.finished {
		return "✓", styles.Success
	}
	if m.final && m.outcome.stopped() && index == m.activeStep() {
		return "✗", styles.Danger
	}
	return "●", styles.Accent
}

// runningDetail renders the active step's second line: a counted bar or the
// spinner with elapsed time and write-ahead-log growth.
func (m Model) runningDetail(step store.UpgradeStep, state *stepState) string {
	styles := m.theme.Styles()
	if step.Counts != store.UpgradeUnitNone && state.last.Of > 0 {
		lines := kit.ProgressMatrix([]kit.ProgressRow{{
			Label: strings.ToLower(step.Counts.String()), Done: int(state.last.Done), Total: int(state.last.Of),
		}})
		detail := styles.Base.Render("    " + lines[0].Bar)
		if lines[0].Elapsed != "" {
			detail += styles.Muted.Render("  " + lines[0].Elapsed)
		}
		estimate := "estimate pending"
		if state.hasEstimate {
			estimate = "estimate " + displayDuration(state.estimate)
		}
		return detail + styles.Muted.Render("  "+estimate)
	}
	if step.Cost.Long() && step.Counts == store.UpgradeUnitNone {
		frame := m.spinner.View()
		return styles.Base.Render("    "+frame) + styles.Muted.Render(fmt.Sprintf(
			"  %s elapsed · write-ahead log grew %s",
			displayDuration(state.last.Elapsed), formatBytes(state.last.WALBytes)))
	}
	return ""
}

func (m Model) footerLines() []string {
	styles := m.theme.Styles()
	total := m.totalElapsed()
	if m.final {
		if m.err != nil && len(m.plan.Steps) > 0 && len(m.report.Steps) == len(m.plan.Steps) {
			// The command returns the full error too. Bound the inline block
			// so an arbitrarily long error cannot consume the terminal.
			panel := kit.NewPanel(m.theme)
			panel.SetSize(m.width, 0)
			message := strings.Join(strings.Fields(mdrender.Sanitize(m.err.Error())), " ")
			panel.Wrapped(styles.Danger, "  the command then failed: "+message)
			errorLines := strings.Split(panel.View(), "\n")
			if len(errorLines) > 4 {
				errorLines = append(errorLines[:3], styles.Muted.Render("  … see the full error below"))
			}
			return append([]string{styles.Danger.Render(fmt.Sprintf(
				"  all %d step(s) committed · the store is at schema %d", len(m.report.Steps), m.report.To))}, errorLines...)
		}
		switch m.outcome {
		case OutcomeDone:
			return []string{styles.Muted.Render(fmt.Sprintf(
				"  finished %d step(s) in %s · schema %d → %d", len(m.report.Steps), displayDuration(total), m.report.From, m.report.To))}
		case OutcomeStoppedByUser:
			if m.stoppedStep() == 0 {
				if len(m.report.Steps) == 0 {
					return []string{styles.Danger.Render("  stopped before any step ran · nothing was changed")}
				}
				return []string{styles.Danger.Render(fmt.Sprintf(
					"  stopped before the next step · %d step(s) finished and are kept", len(m.report.Steps)))}
			}
			return []string{
				styles.Danger.Render("  stopped by user · the running step rolled back; finished steps are kept"),
				styles.Muted.Render(fmt.Sprintf("  %d step(s) finished before the stop · total elapsed %s", len(m.report.Steps), displayDuration(total))),
			}
		case OutcomeRefusedLowDisk:
			return []string{styles.Danger.Render("  refused: not enough free disk · nothing was changed")}
		case OutcomeRefusedConsent:
			return []string{styles.Danger.Render("  consent was declined · nothing was changed")}
		case OutcomeFailedStep:
			if m.stoppedStep() == 0 {
				if len(m.report.Steps) == 0 {
					return []string{styles.Danger.Render("  failed before any step ran · nothing was changed")}
				}
				return []string{styles.Danger.Render(fmt.Sprintf(
					"  failed before the next step · %d step(s) finished and are kept", len(m.report.Steps)))}
			}
			return []string{
				styles.Danger.Render("  failed: the running step rolled back; the steps before it are kept"),
				styles.Muted.Render(fmt.Sprintf("  %d step(s) finished before the failure · total elapsed %s", len(m.report.Steps), displayDuration(total))),
			}
		}
	}
	if len(m.plan.Steps) == 0 {
		return []string{styles.Muted.Render("  the store is already current")}
	}
	lines := []string{
		styles.Muted.Render(fmt.Sprintf("  total elapsed %s · %d step(s) planned", displayDuration(total), len(m.plan.Steps))),
		styles.Muted.Render("  ctrl+c rolls back the current step; finished steps are kept"),
	}
	if m.plan.HasLongStep() {
		lines = append(lines, styles.Muted.Render("  long steps read all stored rows or text; a precise fraction is not available"))
	}
	return lines
}

// windowRows keeps every row when they fit in the rows the header and footer
// leave. Otherwise it keeps the active step's pair first, then the rows after
// it (what remains), then rows before it as space allows. The returned hidden
// count is how many rows before the active step were dropped.
func (m Model) windowRows(rows []string, reserved int) ([]string, int) {
	if m.height <= 0 || len(rows)+reserved <= m.height {
		return rows, 0
	}
	available := m.height - reserved
	if available <= 0 {
		return nil, len(rows)
	}
	start, end := m.activeRowBoundary(rows)
	keep := make([]bool, len(rows))
	used := 0
	selectRow := func(i int) {
		if i < 0 ||
			i >= len(rows) ||
			keep[i] ||
			used >= available {
			return
		}
		keep[i] = true
		used++
	}
	for i := start; i < end; i++ {
		selectRow(i)
	}
	for i := start; i < len(rows); i++ {
		selectRow(i)
	}
	for i := start - 1; i >= 0; i-- {
		selectRow(i)
	}
	window := make([]string, 0, used)
	for i := range rows {
		if keep[i] {
			window = append(window, rows[i])
		}
	}
	hidden := 0
	for i := 0; i < start; i++ {
		if !keep[i] {
			hidden++
		}
	}
	return window, hidden
}

// activeRowBoundary maps the active step to the row range it occupies: one
// line, or two when its detail line is present.
func (m Model) activeRowBoundary(rows []string) (start, end int) {
	active := m.activeStep()
	if active == 0 || len(rows) == 0 {
		return 0, 1
	}
	position := fmt.Sprintf("step %d of %d", active, len(m.plan.Steps))
	found := -1
	for i, row := range rows {
		if strings.Contains(ansi.Strip(row), position) {
			found = i
			break
		}
	}
	if found < 0 {
		return 0, 1
	}
	end = found + 1
	if end < len(rows) && strings.HasPrefix(ansi.Strip(rows[end]), "    ") {
		end++
	}
	return found, end
}

func displayDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// formatBytes renders a byte count in human units.
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit || suffix == "TiB" {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}
