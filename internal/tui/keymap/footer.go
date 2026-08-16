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
	entries := dispatchableEntries(km, footerAvailability(avail))
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

// FooterActionsProvider is an optional refinement an Availability may implement
// to show FEWER actions in the footer hint bar than the full set it makes
// dispatchable, while the help overlay (built straight from AvailableActions)
// still lists them all. A step whose field advertises a large action set can
// use this to keep the always-visible footer to its few primary keys and move
// the rest behind help, without ever advertising a footer hint no key could
// match: FooterView intersects the returned list with AvailableActions, so the
// footer can only ever be a SUBSET of what is dispatchable.
type FooterActionsProvider interface {
	FooterActions() []ActionID
}

// footerAvailability narrows an Availability to its footer-preferred actions
// when it implements [FooterActionsProvider], intersected with (and ordered by)
// what is actually available so the footer never lists an undispatchable hint.
// An Availability that does not opt in footers exactly the actions it makes
// available, unchanged.
func footerAvailability(avail Availability) Availability {
	provider, ok := avail.(FooterActionsProvider)
	if !ok {
		return avail
	}
	available := map[ActionID]bool{}
	for _, action := range avail.AvailableActions() {
		available[action] = true
	}
	seen := map[ActionID]bool{}
	var out []ActionID
	for _, action := range provider.FooterActions() {
		if available[action] && !seen[action] {
			out = append(out, action)
			seen[action] = true
		}
	}
	return staticAvailability(out)
}

// staticAvailability adapts a fixed action list to the Availability interface.
type staticAvailability []ActionID

func (s staticAvailability) AvailableActions() []ActionID { return []ActionID(s) }
