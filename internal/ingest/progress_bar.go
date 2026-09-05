package ingest

import (
	"fmt"
	"strings"
)

// Progress bar rendering shared by the harvest TTY renderer and the kickstart
// local-import screen, so both surfaces report one stage exactly the same way.
// The name renders exactly as given: harvest passes Stage.String, kickstart
// passes the lowercased name per the lowercase-chrome rule.
const (
	progressBarWidth     = 24
	progressBarNameWidth = 13
	progressBarFill      = '█'
	progressBarEmpty     = '░'
)

// RenderProgressBar formats a single stage bar as a fixed-width string:
// status icon, padded name, bar, and count.
func RenderProgressBar(name string, sp StageProgress) string {
	// Status icon.
	icon := "○" // not started
	switch {
	case sp.HasErr:
		icon = "✗"
	case sp.Ended:
		icon = "✓"
	case sp.Done > 0 || sp.Total > 0:
		icon = "●"
	}

	// Fill fraction.
	var filled int
	if sp.Total > 0 {
		filled = sp.Done * progressBarWidth / sp.Total
		if sp.Done > 0 && filled == 0 {
			filled = 1
		}
		if filled > progressBarWidth {
			filled = progressBarWidth
		}
	} else if sp.Ended {
		filled = progressBarWidth
	}
	barStr := strings.Repeat(string(progressBarFill), filled) +
		strings.Repeat(string(progressBarEmpty), progressBarWidth-filled)

	// Count label.
	var count string
	if sp.Total > 0 {
		count = fmt.Sprintf("%d/%d", sp.Done, sp.Total)
	} else if sp.Ended {
		count = "done"
	}

	// Stage name padded to a fixed width.
	if len(name) < progressBarNameWidth {
		name = name + strings.Repeat(" ", progressBarNameWidth-len(name))
	}

	return fmt.Sprintf(" %s %s  %s  %s", icon, name, barStr, count)
}
