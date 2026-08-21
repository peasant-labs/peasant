package testutil

import (
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// sgrPattern matches one ANSI select-graphic-rendition escape.
var sgrPattern = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// UnpaintedCells counts the printable cells of one rendered terminal line that
// no background color covers.
//
// A theme text role carries a foreground only, so a line rendered with it
// paints no background cells. Those cells then show the terminal's own color
// instead of the theme token, and a block of lines with different lengths
// reads as a ragged staircase. A line composed through the layout primitives
// in internal/tui/kit must return zero here.
func UnpaintedCells(line string) int {
	unpainted := 0
	painted := false
	index := 0
	for _, loc := range sgrPattern.FindAllStringSubmatchIndex(line, -1) {
		if text := line[index:loc[0]]; text != "" && !painted {
			unpainted += lipgloss.Width(text)
		}
		painted = applySGR(painted, line[loc[2]:loc[3]])
		index = loc[1]
	}
	if text := line[index:]; text != "" && !painted {
		unpainted += lipgloss.Width(text)
	}
	return unpainted
}

// applySGR reports the background state after one escape's parameters. An
// empty parameter list and a "0" parameter are both a full reset. The extended
// forms 48;2;r;g;b and 48;5;n consume their own arguments, so a color
// component is never read as a separate code.
func applySGR(painted bool, params string) bool {
	if params == "" {
		return false
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		switch code := fields[i]; code {
		case "0", "49":
			painted = false
		case "38", "48":
			step := 1
			if i+1 < len(fields) {
				switch fields[i+1] {
				case "2":
					step = 4
				case "5":
					step = 2
				}
			}
			if code == "48" {
				painted = true
			}
			i += step
		default:
			if isBasicBackgroundCode(code) {
				painted = true
			}
		}
	}
	return painted
}

// isBasicBackgroundCode reports whether code is one of the basic or bright
// background colors.
func isBasicBackgroundCode(code string) bool {
	value, err := strconv.Atoi(code)
	if err != nil {
		return false
	}
	return (value >= 40 && value <= 47) || (value >= 100 && value <= 107)
}
