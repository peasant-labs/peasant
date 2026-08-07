package keymap_test

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
)

// actionsByName maps every keymap.ActionID's String() name back to its
// ActionID, built fresh from keymap.AllActions() so it can never drift from
// the production enum (as opposed to a hand-maintained duplicate map).
func actionsByName(t *testing.T) map[string]keymap.ActionID {
	t.Helper()
	byName := make(map[string]keymap.ActionID, len(keymap.AllActions()))
	for _, a := range keymap.AllActions() {
		byName[a.String()] = a
	}
	return byName
}

// actionByName looks up name in keymap.AllActions(), failing the test
// immediately (not returning a zero value) if a fixture names an action the
// production enum does not recognize - a typo'd fixture row should never
// silently test ActionUnknown instead of the action it meant to name.
func actionByName(t *testing.T, name string) keymap.ActionID {
	t.Helper()
	a, ok := actionsByName(t)[name]
	if !ok {
		t.Fatalf("actionByName: fixture names action %q, which is not in keymap.AllActions(); fix the fixture or add "+
			"the ActionID to keymap.AllActions()", name)
	}
	return a
}

// availabilityList is a keymap.Availability over a fixed, ordered slice of
// ActionIDs - the test-side implementation fixture-driven Match/FooterView/
// HelpEntries tests use to simulate "the screen currently reports these
// actions as available, in this priority order".
type availabilityList []keymap.ActionID

func (a availabilityList) AvailableActions() []keymap.ActionID { return a }

// availabilityFromNames resolves a fixture's list of action-name strings
// into an availabilityList, preserving order (Match resolves ties in
// AvailableActions order, so fixture case order is significant).
func availabilityFromNames(t *testing.T, names []string) availabilityList {
	t.Helper()
	avail := make(availabilityList, 0, len(names))
	for _, name := range names {
		avail = append(avail, actionByName(t, name))
	}
	return avail
}

// msgForKeyString builds the tea.KeyPressMsg a real terminal would produce
// for the keystroke label s uses in keymap.Default()'s key.WithKeys calls
// and this package's fixtures. It is deliberately narrow - only the
// keystrokes Default() actually binds, plus a couple of guaranteed-unbound
// single characters fixtures use to prove a press matches nothing - not a
// general keystroke parser.
func msgForKeyString(t *testing.T, s string) tea.KeyPressMsg {
	t.Helper()
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	case "ctrl+h":
		return tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl}
	case "ctrl+l":
		return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}
	case "ctrl+p":
		return tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}
	case "ctrl+o":
		return tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "ctrl+shift+l":
		return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl | tea.ModShift}
	case "ctrl+shift+h":
		return tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl | tea.ModShift}
	case "?":
		return tea.KeyPressMsg{Code: '?', Text: "?"}
	default:
		if len(s) == 1 {
			return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
		}
		t.Fatalf("msgForKeyString: no mapping for key string %q - add one, or fix the fixture row", s)
		return tea.KeyPressMsg{}
	}
}
