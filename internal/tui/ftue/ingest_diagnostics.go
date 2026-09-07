package ftue

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/mdrender"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// IngestDiagnosticsText preserves the complete reason and remedy of each
// nonfatal warning, stripping terminal controls from source-derived evidence.
func IngestDiagnosticsText(diagnostics []ingest.DiagnosticEntry) string {
	if len(diagnostics) == 0 {
		return ""
	}
	var body strings.Builder
	fmt.Fprintf(&body, "warnings (%d)\n", len(diagnostics))
	for index, diagnostic := range diagnostics {
		fmt.Fprintf(&body, "\nwarning %d", index+1)
		if diagnostic.Location != "" {
			fmt.Fprintf(&body, " — %s", mdrender.Sanitize(diagnostic.Location))
		}
		if diagnostic.ErrorType != "" {
			fmt.Fprintf(&body, " (%s)", mdrender.Sanitize(diagnostic.ErrorType))
		}
		fmt.Fprintf(&body, "\n%s\n", mdrender.Sanitize(diagnostic.Message))
		if diagnostic.Remediation != "" {
			fmt.Fprintf(&body, "fix: %s\n", mdrender.Sanitize(diagnostic.Remediation))
		}
	}
	return body.String()
}

// IngestCompletion keeps warning-bearing setup results readable within the
// mounted terminal. The callers retain their existing success/error actions;
// this presentation helper only wraps and scrolls their completion content.
type IngestCompletion struct {
	frame kit.Frame
	panel kit.Panel
	lines []string
	title string
}

// NewIngestCompletion composes the shared kit frame over trusted presentation
// content. Source-derived diagnostic values must pass IngestDiagnosticsText.
func NewIngestCompletion(th theme.Theme, title, content, footer string, width, height int) IngestCompletion {
	frame := kit.NewFrame(th).WithTitle(title).WithFooter(footer)
	frame.SetSize(width, height)
	panel := kit.NewPanel(th)
	panel.SetSize(frame.InnerWidth(), frame.InnerHeight())
	return IngestCompletion{frame: frame, panel: panel, title: title, lines: strings.Split(ansi.Wrap(content, max(1, frame.InnerWidth()), ""), "\n")}
}

type completionScrollActions struct{}

var _ keymap.Availability = completionScrollActions{}

func (completionScrollActions) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionUp, keymap.ActionDown, keymap.ActionPageUp, keymap.ActionPageDown, keymap.ActionTop, keymap.ActionBottom}
}

// Scroll dispatches the canonical navigation keys without consuming completion
// confirmation, retry, or exit. Offset is clamped again after terminal resize.
func (c IngestCompletion) Scroll(msg tea.Msg, offset int) (int, bool) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return offset, false
	}
	action, ok := keymap.Match(keymap.Default(), key, completionScrollActions{})
	if !ok {
		return offset, false
	}
	limit := max(0, len(c.lines)-c.frame.InnerHeight())
	offset = min(max(0, offset), limit)
	switch action {
	case keymap.ActionUp:
		offset--
	case keymap.ActionDown:
		offset++
	case keymap.ActionPageUp:
		offset -= max(1, c.frame.InnerHeight()-1)
	case keymap.ActionPageDown:
		offset += max(1, c.frame.InnerHeight()-1)
	case keymap.ActionTop:
		offset = 0
	case keymap.ActionBottom:
		offset = limit
	}
	return min(max(0, offset), limit), true
}

// View retains the footer while all warning and next-step lines can be paged.
func (c IngestCompletion) View(offset int) string {
	height := c.frame.InnerHeight()
	offset = min(max(0, offset), max(0, len(c.lines)-height))
	if height > 0 && len(c.lines) > height {
		c.frame = c.frame.WithTitle(fmt.Sprintf("%s · lines %d–%d of %d", c.title, offset+1, min(len(c.lines), offset+height), len(c.lines)))
	}
	for _, line := range c.lines[offset:min(len(c.lines), offset+height)] {
		c.panel.Rendered(line)
	}
	c.frame.SetContent(c.panel.View())
	return c.frame.View()
}
