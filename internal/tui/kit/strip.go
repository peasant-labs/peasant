package kit

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Strip overflow markers. A clipped strip shows the marker on the clipped
// side, so a cut reads as "more items this way" instead of a random
// truncation.
const (
	// StripMarkerLeft marks items hidden before the visible window.
	StripMarkerLeft = "‹"
	// StripMarkerRight marks items hidden after the visible window.
	StripMarkerRight = "›"
)

// StripItem is one item of a horizontal strip, such as one tab of a step tab
// strip. Style is applied to Text alone; the strip paints the gaps, the
// markers, and the trailing pad with the caller's base style.
type StripItem struct {
	Text  string
	Style lipgloss.Style
}

// StripView is the visible window of a horizontal strip: the half-open item
// range [Start, End) to draw, and whether items are hidden on each side.
type StripView struct {
	Start         int
	End           int
	LeftOverflow  bool
	RightOverflow bool
}

// Empty reports whether the window contains no items.
func (v StripView) Empty() bool { return v.End <= v.Start }

// Contains reports whether item index i is inside the window.
func (v StripView) Contains(i int) bool { return i >= v.Start && i < v.End }

// StripWindow computes the window of a horizontal strip that keeps the active
// item FULLY visible.
//
// itemWidths holds each item's display width in order. gapWidth is the width
// of the separator drawn between two neighbouring items, and markerWidth is
// the width of one overflow marker plus its own gap. width is the total cell
// budget for the whole strip.
//
// The window is the LEFTMOST one that still contains the active item, so the
// steps already passed stay in view for as long as they fit, and the window
// then grows to the right while there is room. When even the active item
// alone does not fit, the window holds that item only and both overflow flags
// report the truth for the caller to draw.
func StripWindow(itemWidths []int, gapWidth, markerWidth, active, width int) StripView {
	n := len(itemWidths)
	if n == 0 || width <= 0 {
		return StripView{}
	}
	if active < 0 {
		active = 0
	}
	if active >= n {
		active = n - 1
	}

	fits := func(start, end int) bool {
		if end <= start {
			return false
		}
		total := 0
		for i := start; i < end; i++ {
			total += itemWidths[i]
		}
		total += gapWidth * (end - start - 1)
		if start > 0 {
			total += markerWidth
		}
		if end < n {
			total += markerWidth
		}
		return total <= width
	}

	for start := 0; start <= active; start++ {
		if !fits(start, active+1) {
			continue
		}
		end := active + 1
		for end < n && fits(start, end+1) {
			end++
		}
		return StripView{
			Start:         start,
			End:           end,
			LeftOverflow:  start > 0,
			RightOverflow: end < n,
		}
	}
	// The active item alone is wider than the budget. Show it and let the
	// caller truncate, rather than showing a window without it.
	return StripView{
		Start:         active,
		End:           active + 1,
		LeftOverflow:  active > 0,
		RightOverflow: active+1 < n,
	}
}

// ScrollStrip renders items into EXACTLY width cells, scrolled so the item at
// active stays fully visible, with an overflow marker on each clipped side.
//
// base paints the gaps, the markers, and the trailing pad, so every cell of
// the strip carries the surface background. Derive base from a [Panel] with
// [Panel.Style] rather than painting a background at the surface.
func ScrollStrip(base lipgloss.Style, items []StripItem, active, width int, gap string) string {
	if width <= 0 {
		return ""
	}
	if len(items) == 0 {
		return FitLine(base, "", width)
	}
	widths := make([]int, len(items))
	for i, item := range items {
		widths[i] = lipgloss.Width(item.Text)
	}
	gapWidth := lipgloss.Width(gap)
	markerWidth := lipgloss.Width(StripMarkerLeft) + gapWidth
	view := StripWindow(widths, gapWidth, markerWidth, active, width)
	if view.Empty() {
		return FitLine(base, "", width)
	}

	var b strings.Builder
	if view.LeftOverflow {
		b.WriteString(base.Render(StripMarkerLeft + gap))
	}
	for i := view.Start; i < view.End; i++ {
		if i > view.Start {
			b.WriteString(base.Render(gap))
		}
		b.WriteString(items[i].Style.Render(items[i].Text))
	}
	if view.RightOverflow {
		b.WriteString(base.Render(gap + StripMarkerRight))
	}
	return fitRenderedLine(base, b.String(), width)
}
