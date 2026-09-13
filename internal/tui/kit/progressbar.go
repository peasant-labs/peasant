package kit

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"
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
	bar, _ := renderProgressBar(label, done, total, ended, hasErr, "", 0)
	return bar
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
// in the set. Rows split into Bar and Elapsed so callers can paint the
// duration in a dimmer role than the row.
func ProgressMatrix(rows []ProgressRow) []ProgressLine {
	width := 0
	for _, row := range rows {
		if w := len(progressCount(row.Done, row.Total, row.Ended)); w > width {
			width = w
		}
	}
	lines := make([]ProgressLine, 0, len(rows))
	for _, row := range rows {
		bar, elapsed := renderProgressBar(row.Label, row.Done, row.Total, row.Ended, row.HasErr, row.Elapsed, width)
		lines = append(lines, ProgressLine{Bar: bar, Elapsed: elapsed})
	}
	return lines
}

// ProgressLine is one rendered matrix row. Bar holds the icon, padded label,
// bar, and count cell; Elapsed holds the trailing duration, or "" when the
// row has none.
type ProgressLine struct {
	Bar     string
	Elapsed string
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

func renderProgressBar(label string, done, total int, ended, hasErr bool, elapsed string, countWidth int) (string, string) {
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

	filled := currentProgressFilledCells(done, total, ended)
	bar := progress.New(
		progress.WithWidth(progressBarWidth),
		progress.WithoutPercentage(),
		progress.WithFillCharacters(progressBarFill, progressBarEmpty),
		progress.WithColors(lipgloss.NoColor{}),
	)
	bar.EmptyColor = lipgloss.NoColor{}
	barStr := bar.ViewAs(float64(filled) / float64(progressBarWidth))

	count := progressCount(done, total, ended)

	// Label padded to a fixed width.
	if len(label) < progressBarNameWidth {
		label = label + strings.Repeat(" ", progressBarNameWidth-len(label))
	}

	row := fmt.Sprintf(" %s %s  %s  %s", icon, label, barStr, count)
	if elapsed == "" {
		return row, ""
	}
	if len(count) < countWidth {
		row += strings.Repeat(" ", countWidth-len(count))
	}
	return row, elapsed
}

func currentProgressFilledCells(done, total int, ended bool) int {
	if total <= 0 {
		if ended {
			return progressBarWidth
		}
		return 0
	}
	filled := done * progressBarWidth / total
	if done > 0 && filled == 0 {
		filled = 1
	}
	if filled < 0 {
		return 0
	}
	if filled > progressBarWidth {
		return progressBarWidth
	}
	return filled
}
