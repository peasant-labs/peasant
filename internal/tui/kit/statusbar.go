package kit

import (
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// StatusBarMinSize is the smallest region a StatusBar draws into: a single
// row a few cells wide.
var StatusBarMinSize = Size{Width: 4, Height: 1}

// StatusBar is the bottom chrome row of a TUI page: a short status message on
// the left, an optional right-aligned segment, and the help-footer mount that
// renders keymap.FooterView for whatever component currently has focus. It is
// the ONE place the footer hint bar is rendered, so every page's hints come
// from the same keymap-derived source and cannot drift from what Match can
// actually dispatch.
type StatusBar struct {
	theme theme.Theme
	left  string
	right string
	width int
}

// NewStatusBar builds a StatusBar over theme t.
func NewStatusBar(t theme.Theme) StatusBar {
	return StatusBar{theme: t, width: StatusBarMinSize.Width}
}

// WithStatus returns a copy of s with the left status message set.
func (s StatusBar) WithStatus(left string) StatusBar {
	s.left = left
	return s
}

// WithRight returns a copy of s with the right-aligned segment set.
func (s StatusBar) WithRight(right string) StatusBar {
	s.right = right
	return s
}

// SetSize sets the row width. Height is always one row.
func (s *StatusBar) SetSize(width, _ int) {
	if width < 0 {
		width = 0
	}
	s.width = width
}

var _ Sizeable = (*StatusBar)(nil)

// View renders the status row for the currently-focused component: the left
// status, then the footer hints derived from km restricted to avail's
// available actions (via keymap.FooterView), then any right segment. avail may
// be nil, in which case no hints are shown. The whole row fits exactly width
// cells. Only the PLAIN left status is ever truncated - the already-styled
// hint bar and right segment are kept whole (dropped entirely rather than cut
// mid-escape) so a clip can never sever an ANSI sequence.
func (s StatusBar) View(km keymap.Keymap, avail keymap.Availability) string {
	styles := s.theme.Styles()
	if !StatusBarMinSize.fitsWithin(s.width, 1) {
		return styles.Muted.Render(truncateLine(s.left, s.width))
	}

	var hints string
	if avail != nil {
		hints = keymap.FooterView(s.theme, km, avail)
	}
	right := ""
	if s.right != "" {
		right = styles.Muted.Render(s.right)
	}

	// Reserve space for the right segment first, then hints, then fit the
	// plain left status into whatever remains.
	rightW := lipgloss.Width(right)
	hintsW := lipgloss.Width(hints)
	if rightW >= s.width {
		return styles.Muted.Render(truncateLine(s.right, s.width))
	}
	remaining := s.width - rightW
	// A gap of at least one cell before the right segment when it is present.
	gapBeforeRight := 0
	if right != "" {
		gapBeforeRight = 1
		remaining -= 1
	}
	if hintsW+2 > remaining {
		// No room for the hint bar; drop it whole.
		hints, hintsW = "", 0
	}
	sep := ""
	sepW := 0
	if hints != "" {
		sep = styles.Muted.Render("  ")
		sepW = 2
	}
	leftBudget := remaining - hintsW - sepW
	if leftBudget < 0 {
		leftBudget = 0
	}
	left := styles.Base.Render(truncateLine(s.left, leftBudget))
	leftW := lipgloss.Width(left)

	// Fill between the left cluster and the right segment.
	fill := s.width - leftW - sepW - hintsW - gapBeforeRight - rightW
	if fill < 0 {
		fill = 0
	}
	return left + sep + hints + styles.Base.Render(spaces(fill+gapBeforeRight)) + right
}
