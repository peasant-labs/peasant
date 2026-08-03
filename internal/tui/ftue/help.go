package ftue

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
)

// helpCategory groups related key bindings for display in the help overlay.
type helpCategory struct {
	name     string
	bindings []key.Binding
}

// pageHelpCategories returns the full help categories for PageKeyMap (overlay).
func pageHelpCategories(km PageKeyMap) []helpCategory {
	return []helpCategory{
		{
			name:     "Navigation",
			bindings: []key.Binding{km.Up, km.Down},
		},
		{
			name: "Actions",
			bindings: []key.Binding{
				km.Select, km.Confirm, km.Help,
				key.NewBinding(key.WithHelp("b", "back")),
				key.NewBinding(key.WithHelp("q", "quit")),
				key.NewBinding(key.WithHelp("j/k", "up/down")),
				key.NewBinding(key.WithHelp("1-9", "instant select")),
			},
		},
	}
}

// treeHelpCategories returns the full help categories for TreeKeyMap (overlay).
func treeHelpCategories(km TreeKeyMap) []helpCategory {
	return []helpCategory{
		{
			name: "Navigation",
			bindings: []key.Binding{
				km.Up, km.Down, km.Expand, km.Collapse,
			},
		},
		{
			name: "Actions",
			bindings: []key.Binding{
				km.Select, km.Confirm,
				key.NewBinding(key.WithHelp("b", "back")),
				key.NewBinding(key.WithHelp("q", "quit")),
				km.Help,
				key.NewBinding(key.WithHelp("j/k", "up/down")),
				key.NewBinding(key.WithHelp("h/l", "collapse/expand")),
				key.NewBinding(key.WithHelp("J/K", "page down/up")),
				key.NewBinding(key.WithHelp("H/L", "jump to top/bottom")),
				key.NewBinding(key.WithHelp("[/]", "prev/next section")),
				km.Search,
			},
		},
	}
}

// pageStatusBar returns a compact single-line help bar for simple pages.
func pageStatusBar() string {
	return HelpBar.Render("↑/↓: navigate  space: select  enter: confirm  ?: all shortcuts")
}

// treeStatusBar returns a compact single-line help bar for the tree page.
func treeStatusBar() string {
	return HelpBar.Render("↑/↓: move  space: toggle  →/←: expand/collapse  enter: confirm  ?: all shortcuts")
}

// renderHelpOverlay renders bindings grouped by category as an indented list.
func renderHelpOverlay(categories []helpCategory) string {
	var b strings.Builder

	b.WriteString(TextBg.Render("\n"))
	b.WriteString(OptionSelected.Render("All Shortcuts"))
	b.WriteString(TextBg.Render("\n"))

	for _, cat := range categories {
		b.WriteString(TextBg.Render("\n"))
		b.WriteString(DescriptionStyle.Render("  " + cat.name))
		b.WriteString(TextBg.Render("\n"))
		for _, bind := range cat.bindings {
			h := bind.Help()
			b.WriteString(HelpBar.Render(fmt.Sprintf("    %-16s %s", h.Key, h.Desc)))
			b.WriteString(TextBg.Render("\n"))
		}
	}

	b.WriteString(TextBg.Render("\n"))
	b.WriteString(HelpBar.Render("  Press ? or Esc to close"))
	b.WriteString(TextBg.Render("\n"))

	return b.String()
}
