package ftue

import "github.com/charmbracelet/bubbles/key"

// PageKeyMap contains keybindings common to all wizard pages.
type PageKeyMap struct {
	Up      key.Binding
	Down    key.Binding
	Select  key.Binding // space — toggle/select
	Confirm key.Binding // enter
	Cancel  key.Binding // escape
	Help    key.Binding // ? — show help overlay
}

// DefaultPageKeyMap is the shared base keybinding set for all wizard pages.
var DefaultPageKeyMap = PageKeyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓", "down"),
	),
	Select: key.NewBinding(
		key.WithKeys(" "),
		key.WithHelp("space", "select"),
	),
	Confirm: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "confirm"),
	),
	Cancel: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "cancel"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "all shortcuts"),
	),
}

// TreeKeyMap extends PageKeyMap with tree-specific navigation.
type TreeKeyMap struct {
	PageKeyMap
	PageUp           key.Binding
	PageDown         key.Binding
	JumpTop          key.Binding
	JumpBottom       key.Binding
	Expand           key.Binding
	Collapse         key.Binding
	PrevSibling      key.Binding
	NextSibling      key.Binding
	Search           key.Binding
	ConfirmSelection key.Binding
}

// DefaultTreeKeyMap is the default key binding set for TreeSelectPage.
var DefaultTreeKeyMap = TreeKeyMap{
	PageKeyMap: DefaultPageKeyMap,
	PageUp: key.NewBinding(
		key.WithKeys("K", "pgup", "ctrl+u"),
		key.WithHelp("PgUp", "page up"),
	),
	PageDown: key.NewBinding(
		key.WithKeys("J", "pgdown", "ctrl+d"),
		key.WithHelp("PgDn", "page down"),
	),
	JumpTop: key.NewBinding(
		key.WithKeys("H"),
		key.WithHelp("Home/Shift+h", "jump to top"),
	),
	JumpBottom: key.NewBinding(
		key.WithKeys("L"),
		key.WithHelp("End/Shift+l", "jump to bottom"),
	),
	Expand: key.NewBinding(
		key.WithKeys("right", "l"),
		key.WithHelp("→", "expand"),
	),
	Collapse: key.NewBinding(
		key.WithKeys("left", "h"),
		key.WithHelp("←", "collapse"),
	),
	PrevSibling: key.NewBinding(
		key.WithKeys("[", "alt+k", "alt+up"),
		key.WithHelp("[", "prev section"),
	),
	NextSibling: key.NewBinding(
		key.WithKeys("]", "alt+j", "alt+down"),
		key.WithHelp("]", "next section"),
	),
	Search: key.NewBinding(
		key.WithKeys("f"),
		key.WithHelp("f", "search"),
	),
	ConfirmSelection: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "confirm"),
	),
}
