package kit

import (
	"fmt"
	"strings"
)

// Progress bar rows shared by every surface that reports staged work with
// counts: the harvest TTY renderer and the kickstart local-import screen.
// One implementation keeps both surfaces byte-identical. Rows carry no style;
// callers paint them with a theme role. Labels render exactly as given:
// harvest passes raw stage names, kickstart passes lowercased names per the
// lowercase-chrome rule.
const (
	progressBarWidth     = 24
	progressBarNameWidth = 13
	progressBarFill      = '█'
	progressBarEmpty     = '░'
)

// ProgressBar formats a single progress row as a fixed-width string: status
// icon, padded label, bar, and count.
func ProgressBar(label string, done, total int, ended, hasErr bool) string {
	return renderProgressBar(label, done, total, ended, hasErr, "", 0)
}

// ProgressRow is one matrix row: the stage progress plus an optional trailing
// elapsed duration. An empty Elapsed omits the duration cleanly.
type ProgressRow struct {
	Label         string
	Done, Total   int
	Ended, HasErr bool
	Elapsed       string
}

// ProgressMatrix renders one aligned progress row per entry: status icon,
// padded label, bar, count, and, when set, the elapsed duration in a column
// aligned across the matrix by padding every count cell to the widest count
// in the set.
func ProgressMatrix(rows []ProgressRow) []string {
	width := 0
	for _, row := range rows {
		if w := len(progressCount(row.Done, row.Total, row.Ended)); w > width {
			width = w
		}
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, renderProgressBar(row.Label, row.Done, row.Total, row.Ended, row.HasErr, row.Elapsed, width))
	}
	return lines
}

// progressCount formats the count cell shared by the single and matrix
// renderers. Counts are ASCII digits, slashes, and words, so length equals
// display width.
func progressCount(done, total int, ended bool) string {
	if total > 0 {
		return fmt.Sprintf("%d/%d", done, total)
	}
	if ended {
		return "done"
	}
	return ""
}

func renderProgressBar(label string, done, total int, ended, hasErr bool, elapsed string, countWidth int) string {
	// Status icon.
	icon := "○" // not started
	switch {
	case hasErr:
		icon = "✗"
	case ended:
		icon = "✓"
	case done > 0 || total > 0:
		icon = "●"
	}

	// Fill fraction.
	var filled int
	if total > 0 {
		filled = done * progressBarWidth / total
		if done > 0 && filled == 0 {
			filled = 1
		}
		if filled > progressBarWidth {
			filled = progressBarWidth
		}
	} else if ended {
		filled = progressBarWidth
	}
	barStr := strings.Repeat(string(progressBarFill), filled) +
		strings.Repeat(string(progressBarEmpty), progressBarWidth-filled)

	count := progressCount(done, total, ended)

	// Label padded to a fixed width.
	if len(label) < progressBarNameWidth {
		label = label + strings.Repeat(" ", progressBarNameWidth-len(label))
	}

	row := fmt.Sprintf(" %s %s  %s  %s", icon, label, barStr, count)
	if elapsed == "" {
		return row
	}
	if len(count) < countWidth {
		row += strings.Repeat(" ", countWidth-len(count))
	}
	return row + "  " + elapsed
}
