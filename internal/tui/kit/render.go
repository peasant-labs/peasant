package kit

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// truncateLine clips a single (already-styled or plain) line to at most
// width display cells using the ansi-aware lipgloss.Width, NEVER len().
// When the content is wider than width it is cut and, if there is room, an
// ellipsis rune is placed in the last cell so truncation is visible rather
// than silent. A non-positive width yields the empty string. It operates on
// the raw runes of a plain string; callers style the result after clipping
// so the ellipsis inherits the surrounding style.
func truncateLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	// Grow the prefix until adding one more rune would exceed the budget,
	// reserving one cell for the ellipsis when the string is being cut.
	budget := width
	ellipsis := "…"
	if width >= 1 {
		budget = width - 1
	}
	var out []rune
	for _, r := range runes {
		if lipgloss.Width(string(append(out, r))) > budget {
			break
		}
		out = append(out, r)
	}
	clipped := string(out) + ellipsis
	// Defensive: if the ellipsis pushed us over (wide runes), fall back to a
	// hard cut with no ellipsis rather than overflow.
	if lipgloss.Width(clipped) > width {
		return string(out)
	}
	return clipped
}

// padLine right-pads a line with spaces to exactly width display cells,
// measuring with lipgloss.Width so styled/wide content is accounted for. A
// line already at or over width is returned unchanged (callers truncate
// first when they need a hard cap).
func padLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + spaces(width-w)
}

// FitLine truncates then pads one PLAIN-TEXT line to exactly width terminal
// cells and applies style exactly once, after fitting. Applying the style last
// makes background-bearing rows paint through their final cell instead of
// stopping at the final glyph. Measurement and truncation are ANSI-aware and
// grapheme-aware, but callers must not pass an already-rendered ANSI string:
// composed output needs fitRenderedLine so nested style resets can restore the
// parent's background correctly.
func FitLine(style lipgloss.Style, s string, width int) string {
	if width <= 0 {
		return ""
	}
	clipped := s
	if ansi.StringWidth(clipped) > width {
		clipped = ansi.Truncate(clipped, width, "…")
	}
	return style.Render(padLine(clipped, width))
}

// FitLineTail truncates then pads one PLAIN-TEXT line to exactly width terminal
// cells while retaining its latest complete grapheme clusters. When clipping
// leaves room, a leading ellipsis makes the omitted prefix visible; a one-cell
// region keeps the final cluster itself, which lets an editing row retain its
// caret. Like [FitLine], it applies style exactly once after fitting.
func FitLineTail(style lipgloss.Style, s string, width int) string {
	if width <= 0 {
		return ""
	}
	clipped := s
	stringWidth := ansi.StringWidth(s)
	if stringWidth > width {
		prefix := "…"
		prefixWidth := ansi.StringWidth(prefix)
		if width <= prefixWidth {
			prefix = ""
			prefixWidth = 0
		}
		clipped = completeTailCandidate(s, stringWidth, width, prefix)
		// Reserving the ellipsis can put the requested cut inside a wide
		// grapheme. Keeping that grapheme whole then drops it, which can leave
		// the candidate under-filled. In that boundary case, prefer the maximal
		// complete suffix without an ellipsis when it retains more trailing
		// content cells.
		if prefix != "" && ansi.StringWidth(clipped) < width {
			suffix := completeTailCandidate(s, stringWidth, width, "")
			trailingWidth := ansi.StringWidth(clipped) - prefixWidth
			if ansi.StringWidth(suffix) > trailingWidth {
				clipped = suffix
			}
		}
	}
	return style.Render(padLine(clipped, width))
}

// completeTailCandidate returns the latest complete grapheme clusters that fit
// in width after prefix. TruncateLeft keeps a cluster whole when the requested
// cut lands inside it; advancing the cut prevents that preservation from
// overflowing the requested cell budget.
func completeTailCandidate(s string, stringWidth, width int, prefix string) string {
	prefixWidth := ansi.StringWidth(prefix)
	remove := stringWidth - (width - prefixWidth)
	clipped := ansi.TruncateLeft(s, remove, prefix)
	for ansi.StringWidth(clipped) > width && remove < stringWidth {
		remove++
		clipped = ansi.TruncateLeft(s, remove, prefix)
	}
	return clipped
}

// fitLine preserves the private helper used throughout the component
// implementations while exposing FitLine to presentation packages that need
// the same fixed-row contract.
func fitLine(style lipgloss.Style, s string, width int) string {
	return FitLine(style, s, width)
}

// fitRenderedLine fits a line that already contains ANSI styling inside a
// parent style. Unlike FitLine, this function never applies a style around
// already-rendered bytes: ANSI reset sequences are not nestable and would
// otherwise cancel the parent's background before trailing padding. It starts
// in the parent style, restores that style after each reset, and pads before
// the final reset so every otherwise-transparent cell inherits the parent.
func fitRenderedLine(style lipgloss.Style, rendered string, width int) string {
	if width <= 0 {
		return ""
	}
	clipped := rendered
	if ansi.StringWidth(clipped) > width {
		clipped = ansi.Truncate(clipped, width, "…")
	}
	padding := spaces(width - ansi.StringWidth(clipped))
	if clipped == "" {
		return FitLine(style, "", width)
	}

	styledProbe := style.Render("x")
	prefixAt := strings.Index(styledProbe, "x")
	if prefixAt < 0 {
		return clipped + padding
	}
	prefix := styledProbe[:prefixAt]
	suffix := styledProbe[prefixAt+1:]
	if prefix == "" || suffix == "" {
		return clipped + padding
	}
	// The content's OWN trailing reset (its last styled segment's close) is
	// stripped so the wrapping prefix/suffix below owns the true end of the
	// line - interior resets are converted to "restore" (reset + reopen
	// parent) instead, so a run of unstyled text between two styled cells
	// still inherits the parent's background rather than falling back to
	// the terminal default.
	trailingOpen := ""
	for strings.HasSuffix(clipped, suffix) {
		trimmed := strings.TrimSuffix(clipped, suffix)
		if trailingOpen == "" {
			trailingOpen = lastEscapeSequence(trimmed)
		}
		clipped = trimmed
	}
	restore := suffix + prefix
	clipped = strings.ReplaceAll(clipped, "\x1b[m", restore)
	clipped = strings.ReplaceAll(clipped, "\x1b[0m", restore)
	// A trailing segment that only set its OWN foreground (the common case -
	// e.g. a Muted label) leaves the parent's background untouched once its
	// closing reset is stripped, so padding can follow directly: byte-for-
	// byte identical to before. But a trailing segment that also painted its
	// OWN background (e.g. the amber-filled active tab, when it happens to
	// be the LAST cell on the line) leaves that background open with nothing
	// left to close it - every padding cell that follows would inherit the
	// highlight instead of the parent's canvas. Restore the parent style
	// before padding only in that case.
	if setsBackground(trailingOpen) {
		return prefix + clipped + restore + padding + suffix
	}
	return prefix + clipped + padding + suffix
}

// lastEscapeSequence returns the final ANSI SGR escape sequence in s, or ""
// if s contains none.
func lastEscapeSequence(s string) string {
	idx := strings.LastIndex(s, "\x1b[")
	if idx < 0 {
		return ""
	}
	end := strings.IndexByte(s[idx:], 'm')
	if end < 0 {
		return ""
	}
	return s[idx : idx+end+1]
}

// setsBackground reports whether an SGR escape sequence sets a background
// color: the extended code 48 (truecolor "48;2;r;g;b" or 256-color
// "48;5;n"), or a standard/bright background code in the 40-47 / 100-107
// range. Fields are walked as SGR CODES, consuming an extended code's
// trailing color-component fields as it goes, rather than matched by
// substring or by field position alone - a truecolor component's own 0-255
// value (e.g. "47" or "148" inside "38;2;R;G;Bm") can coincide with a real
// background code number, so only a field actually occupying a code
// position counts.
func setsBackground(seq string) bool {
	if seq == "" {
		return false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m")
	fields := strings.Split(body, ";")
	for i := 0; i < len(fields); i++ {
		code, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case code == 38 || code == 48:
			isBackground := code == 48
			// Extended color: "38/48;2;r;g;b" (truecolor) or "38/48;5;n"
			// (256-color). Consume the mode selector plus its color
			// components so they are never mistaken for standalone codes.
			if i+1 < len(fields) {
				switch fields[i+1] {
				case "2":
					i += 4 // mode + r + g + b
				case "5":
					i += 2 // mode + palette index
				}
			}
			if isBackground {
				return true
			}
		case code >= 40 && code <= 47:
			return true
		case code >= 100 && code <= 107:
			return true
		}
	}
	return false
}

// trimLastGraphemeCluster removes exactly one user-perceived character from s.
// Search input is plain text, but FirstGraphemeCluster also gives the same
// width vocabulary used by the canonical fitting helpers above.
func trimLastGraphemeCluster(s string) string {
	if s == "" {
		return ""
	}
	lastStart := 0
	for offset := 0; offset < len(s); {
		cluster, _ := ansi.FirstGraphemeCluster(s[offset:], ansi.GraphemeWidth)
		if cluster == "" {
			return s[:lastStart]
		}
		lastStart = offset
		offset += len(cluster)
	}
	return s[:lastStart]
}

// spaces returns n space characters (n<=0 yields "").
func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// cursorGlyph is the shared selection-cursor vocabulary hoisted from the
// legacy FTUE project picker: the row the cursor is on leads with "▸ ", every
// other row with two spaces, so rows stay column-aligned.
const (
	cursorGlyph   = "▸ "
	noCursorGlyph = "  "
	checkedBox    = "[✓]"
	uncheckedBox  = "[ ]"
	radioOn       = "(•)"
	radioOff      = "( )"
)

// styledCursor returns the cursor prefix for a row, styled from t: the
// selected/amber style on the active row, muted otherwise.
func styledCursor(styles theme.Styles, active bool) string {
	if active {
		return styles.Selected.Render(cursorGlyph)
	}
	return styles.Base.Render(noCursorGlyph)
}
