package settings

import "github.com/peasant-labs/peasant/internal/tui/keymap"

func actionSet(actions []keymap.ActionID) map[keymap.ActionID]bool {
	set := make(map[keymap.ActionID]bool, len(actions))
	for _, action := range actions {
		set[action] = true
	}
	return set
}
