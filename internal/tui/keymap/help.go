package keymap

// HelpEntry is one row of the help overlay: one dispatchable action for the
// caller's current Availability, paired with the key hint and description
// key.Binding.Help carries in km.
type HelpEntry struct {
	Action ActionID
	Key    string
	Desc   string
}

// HelpEntries returns the help-overlay rows for km restricted to avail's
// currently available actions, in avail.AvailableActions() order.
//
// This calls the exact same [dispatchableEntries] derivation [Match] and
// [FooterView] use, so the help overlay can never list an action Match
// could not also resolve, and can never omit one Match can dispatch -
// TestHelpEntries_EqualsDispatchableSet (help_test.go) is the fixture-driven
// proof. This is the architectural fix for the drift the audit found in
// internal/tui/ftue/help.go's pageHelpCategories/treeHelpCategories, which
// listed cosmetic "b: back" / "q: quit" help rows built from
// key.NewBinding(key.WithHelp(...)) calls that carried no WithKeys at all -
// text a user could read in the help overlay that no keypress could ever
// actually trigger.
func HelpEntries(km Keymap, avail Availability) []HelpEntry {
	dispatchable := dispatchableEntries(km, avail)
	entries := make([]HelpEntry, 0, len(dispatchable))
	for _, d := range dispatchable {
		entries = append(entries, HelpEntry{Action: d.Action, Key: d.Help.Key, Desc: d.Help.Desc})
	}
	return entries
}
