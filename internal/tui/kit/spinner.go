package kit

import (
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// SpinnerMinSize is the smallest region a Spinner renders into: one frame
// glyph plus at least a couple of label cells.
var SpinnerMinSize = Size{Width: 3, Height: 1}

// Spinner is THE async-progress surface for the kit: any operation the user
// waits on (a scan, a network publish, a load) shows one. It wraps
// bubbles/v2's spinner with a theme-styled frame and a trailing label, at a
// deliberately slow cadence so the motion reads as "working" rather than
// frantic (per the research polish notes) and stays gentle for reduced-motion
// sensibilities.
type Spinner struct {
	theme theme.Theme
	inner spinner.Model
	label string
	width int
}

// NewSpinner builds a Spinner over theme t with the given label. Start it by
// issuing the command from [Spinner.Tick] (typically from the parent's Init or
// when the async work begins).
func NewSpinner(t theme.Theme, label string) Spinner {
	styles := t.Styles()
	m := spinner.New(spinner.WithSpinner(spinner.Dot))
	// Route the frame color through the theme bundle - never a raw color.
	m.Style = styles.Selected
	return Spinner{
		theme: t,
		inner: m,
		label: label,
		width: SpinnerMinSize.Width,
	}
}

// SetLabel returns a copy of s with the trailing label replaced.
func (s Spinner) SetLabel(label string) Spinner {
	s.label = label
	return s
}

// Label reports the current trailing label.
func (s Spinner) Label() string { return s.label }

// SetSize sets the width the Spinner (frame + label) is clipped to. Height is
// always one row; the height argument is accepted to satisfy Sizeable and
// otherwise ignored.
func (s *Spinner) SetSize(width, _ int) {
	if width < 0 {
		width = 0
	}
	s.width = width
}

var _ Sizeable = (*Spinner)(nil)

// Tick returns the command that advances the spinner one frame. Issue it to
// start the animation; each resulting tick is fed back through Update, which
// returns the next tick, keeping the loop alive until the parent stops
// forwarding.
func (s Spinner) Tick() tea.Cmd { return s.inner.Tick }

// ownsTick reports whether msg was emitted by this concrete spinner model.
// Bubbles includes the model ID in every tick so sibling async components can
// reject each other's animation work before updating.
func (s Spinner) ownsTick(msg spinner.TickMsg) bool { return msg.ID == s.inner.ID() }

// Update advances the wrapped spinner on its tick message and returns the
// concrete Spinner plus the command for the next frame. Non-tick messages
// leave it unchanged.
func (s Spinner) Update(msg tea.Msg) (Spinner, tea.Cmd) {
	var cmd tea.Cmd
	s.inner, cmd = s.inner.Update(msg)
	return s, cmd
}

// CurrentFrame reports the glyph the spinner is currently showing (the
// wrapped model's rendered frame, unstyled by the label), primarily so tests
// can assert a tick advanced the animation without depending on the frame
// index the underlying model keeps unexported.
func (s Spinner) CurrentFrame() string { return s.inner.View() }

// View renders the current spinner frame followed by the label, clipped to
// the configured width. Below SpinnerMinSize it renders just the truncated
// label.
func (s Spinner) View() string {
	styles := s.theme.Styles()
	frame := s.inner.View()
	var line string
	if s.label != "" {
		line = frame + styles.Base.Render(" "+s.label)
	} else {
		line = frame
	}
	if s.width > 0 {
		return truncateLine(line, s.width)
	}
	return line
}
