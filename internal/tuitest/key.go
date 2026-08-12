// Package tuitest holds small helpers shared by the scripted, program-level
// smoke tests that drive a bubbletea v2 program's real Update() loop
// (internal/push, internal/tui, internal/tui/ftue). It is deliberately scoped
// to those callers rather than folded into the general-purpose internal/testutil
// package: internal/testutil is imported broadly by non-TUI test packages that
// have no relationship to a terminal UI framework, and pulling bubbletea into
// it would couple all of those packages to a dependency they don't need. Every
// package that imports tuitest already imports charm.land/bubbletea/v2 in its
// own production code, so this package adds no new dependency edge.
package tuitest

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// Key maps a fixture key token to the key event a mounted bubbletea v2 program
// reads. Special keys carry their v2 Code; a single printable rune carries both
// Code and Text so its String() matches the shortcut the program compares
// against, and so a regular-character textinput.Model reads a non-empty Text.
func Key(token string) tea.KeyPressMsg {
	switch token {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEsc}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	default:
		r := []rune(token)
		if len(r) != 1 {
			panic(fmt.Sprintf("tuitest.Key: unsupported multi-rune token %q; add a named case above for it", token))
		}
		return tea.KeyPressMsg{Code: r[0], Text: token}
	}
}
