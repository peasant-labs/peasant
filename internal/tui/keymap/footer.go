package keymap

import (
	"strings"

	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// FooterView renders the bottom hint bar for km restricted to avail's
// currently available actions, styled from t - "key: description" pairs,
// in avail.AvailableActions() order, separated by two spaces (matching the
// single-line grouped-help-bar format docs/TUI.md already documents for the
// legacy pages this package supersedes).
//
// It calls the exact same [dispatchableEntries] derivation [Match] and
// [HelpEntries] use, so the footer can never show a hint for an action
// Match could not also resolve. Colors come from t.Styles() ONLY - never a
// raw lipgloss.Color or hex literal - which is what lets the
// internal/tui/gates color grep gate treat this file the same as every
// other kit component.
func FooterView(t theme.Theme, km Keymap, avail Availability) string {
	entries := dispatchableEntries(km, avail)
	if len(entries) == 0 {
		return ""
	}
	styles := t.Styles()
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, styles.Base.Render(e.Help.Key)+styles.Muted.Render(": "+e.Help.Desc))
	}
	return strings.Join(parts, styles.Muted.Render("  "))
}
