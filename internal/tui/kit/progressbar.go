package kit

import (
	"fmt"
	"strings"
)

// Progress bar row shared by every surface that reports staged work with
// counts: the harvest TTY renderer and the kickstart local-import screen.
// One implementation keeps both surfaces byte-identical. The row carries no
// style; callers paint it with a theme role. The label renders exactly as
// given: harvest passes the raw stage name, kickstart passes the lowercased
// name per the lowercase-chrome rule.
const (
	progressBarWidth     = 24
	progressBarNameWidth = 13
	progressBarFill      = '█'
	progressBarEmpty     = '░'
)

// ProgressBar formats a single progress row as a fixed-width string: status
// icon, padded label, bar, and count.
func ProgressBar(label string, done, total int, ended, hasErr bool) string {
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

	// Count label.
	var count string
	if total > 0 {
		count = fmt.Sprintf("%d/%d", done, total)
	} else if ended {
		count = "done"
	}

	// Label padded to a fixed width.
	if len(label) < progressBarNameWidth {
		label = label + strings.Repeat(" ", progressBarNameWidth-len(label))
	}

	return fmt.Sprintf(" %s %s  %s  %s", icon, label, barStr, count)
}
