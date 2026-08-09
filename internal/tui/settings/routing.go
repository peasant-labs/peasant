package settings

import (
	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
)

// asyncField is the private capability for a field with component-owned
// asynchronous work. Presentations route non-key messages only through this
// seam, so ordinary synchronous fields can never rewrite Draft state in
// response to an unrelated completion.
type asyncField interface {
	handleAsync(*Draft, tea.Msg) tea.Cmd
}

// fieldAsyncCommands forwards msg only to mounted fields that explicitly own
// asynchronous work. It is deliberately package-private and field-specific,
// not a generic event bus.
func fieldAsyncCommands(reg Registry, draft *Draft, msg tea.Msg) []tea.Cmd {
	var commands []tea.Cmd
	for _, section := range reg.Sections {
		for _, field := range section.Fields {
			owner, ok := field.(asyncField)
			if !ok {
				continue
			}
			if cmd := owner.handleAsync(draft, msg); cmd != nil {
				commands = append(commands, cmd)
			}
		}
	}
	return commands
}

// effectiveAvailability removes every printable binding while the focused
// field captures text. Dispatch, footer, and help must all consume the returned
// set so printable q, b, ?, f, and future bindings cannot shadow query text.
func effectiveAvailability(actions []keymap.ActionID, capturesPrintable bool) []keymap.ActionID {
	if !capturesPrintable {
		return actions
	}
	km := keymap.Default()
	effective := make([]keymap.ActionID, 0, len(actions))
	for _, action := range actions {
		if !keymap.HasPrintableBinding(km, action) {
			effective = append(effective, action)
		}
	}
	return effective
}
