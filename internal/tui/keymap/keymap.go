// Package keymap is the single source of truth for every keybinding a kit
// component or the settings Flow can dispatch: one closed [ActionID] enum,
// one [Default] [Keymap] built once from it, and one [Match] function that
// resolves a key press into an action for whatever subset of actions the
// current screen exposes via [Availability].
//
// The audit that motivated this package found the same class of bug
// repeated across internal/tui and internal/push/wizard.go: raw
// msg.String() == comparisons and raw key-string switches on
// tea.KeyPressMsg, defined ad hoc per screen, with no single place that
// could say what a screen's dispatchable actions actually were - including
// a real user-visible divergence where one wizard accepted "tab" to confirm
// while every other page used "enter" (see docs/TUI.md's key reference and
// internal/tui/ftue/keymap.go's TreeKeyMap.ConfirmSelection vs
// PageKeyMap.Confirm). [Default] defines exactly ONE confirm key ("enter")
// app-wide, and [Match]/[FooterView]/[HelpEntries] all derive from the same
// [Availability], so dispatch, the footer hint bar, and the help overlay
// cannot drift apart the way the audited pages did. The internal/tui/gates
// key grep gate enforces the boundary going forward: no new msg.String()
// comparison or raw key-string switch may appear in internal/tui/... or
// internal/push/wizard.go outside this package.
package keymap

import (
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// ActionID is the closed set of user actions a kit component or the
// settings Flow can dispatch. It is intentionally NOT stringly-typed: every
// caller compares against a named constant, never a raw key string, which
// is the property the internal/tui/gates key grep gate exists to enforce
// outside this package.
type ActionID int

// The full ActionID enum. ActionUnknown is the zero value and is never a
// valid dispatch target - it exists so a zero-valued ActionID (e.g. a
// forgotten map entry) fails IsValid rather than silently aliasing
// ActionQuit, mirroring the leading-Unknown-sentinel convention already used
// by internal/config.RedactionLevelDispositionUnknown and
// internal/ingest.TranscriptSourceKindUnknown.
const (
	ActionUnknown ActionID = iota

	// ActionQuit exits the current TUI program.
	ActionQuit
	// ActionConfirm is the ONE confirm action app-wide - the ratified fix
	// for the tab-vs-enter divergence the audit found inside a single
	// wizard (docs/TUI.md's TreeKeyMap.ConfirmSelection bound "tab" while
	// every other page's PageKeyMap.Confirm bound "enter"). Every kit
	// component and the settings Flow confirm with the same key.
	ActionConfirm
	// ActionBack backs out of the current page/step without confirming.
	ActionBack

	// ActionUp moves the cursor/selection up.
	ActionUp
	// ActionDown moves the cursor/selection down.
	ActionDown
	// ActionLeft moves horizontally left (a component with tree semantics
	// uses ActionCollapse instead; see that constant's doc).
	ActionLeft
	// ActionRight moves horizontally right (a component with tree semantics
	// uses ActionExpand instead; see that constant's doc).
	ActionRight
	// ActionPageUp jumps a page's worth of rows up.
	ActionPageUp
	// ActionPageDown jumps a page's worth of rows down.
	ActionPageDown
	// ActionTop jumps the cursor to the first row.
	ActionTop
	// ActionBottom jumps the cursor to the last row.
	ActionBottom
	// ActionNextProject moves the cursor to the next top-level project.
	ActionNextProject
	// ActionPrevProject moves the cursor to the previous top-level project.
	ActionPrevProject
	// ActionSearch starts one hierarchy-wide text search. Components that expose
	// it search every label they own rather than requiring a separate scope.
	ActionSearch

	// ActionFocusPaneLeft moves focus to the LEFT pane of a split surface.
	// It is pane navigation, not field navigation: a split is one field, and
	// the flow's tab/shift+tab still step between fields while the cursor is
	// in either pane.
	ActionFocusPaneLeft
	// ActionFocusPaneRight moves focus to the RIGHT pane of a split surface.
	// See ActionFocusPaneLeft.
	ActionFocusPaneRight

	// ActionNextField moves focus to the next form field.
	ActionNextField
	// ActionPrevField moves focus to the previous form field.
	ActionPrevField
	// ActionToggle toggles/selects the focused item.
	ActionToggle
	// ActionSelectAll toggles every selectable item represented by the current
	// visible projection.
	ActionSelectAll
	// ActionSelectUnderProject selects every selectable node represented under
	// the current projected project containing the cursor. It is an operation,
	// not navigation.
	ActionSelectUnderProject

	// ActionExpand expands a collapsed tree node. Deliberately shares
	// Default's physical keys with ActionRight ("right", "l") - a tree
	// component's Availability exposes ActionExpand, a horizontal-list
	// component exposes ActionRight; the two are never both available on
	// the same screen, so Match's first-match-in-Availability-order
	// resolution never has to arbitrate between them in practice.
	ActionExpand
	// ActionCollapse collapses an expanded tree node. See ActionExpand's
	// doc for why it shares physical keys with ActionLeft.
	ActionCollapse
	// ActionExpandLevel expands every controllable branch in the current
	// projected project, not just the hovered node.
	ActionExpandLevel
	// ActionCollapseLevel collapses every controllable branch in the current
	// projected project.
	ActionCollapseLevel
	// ActionExpandAll expands every controllable node represented by the current
	// visible projection.
	ActionExpandAll
	// ActionCollapseAll collapses every controllable node represented by the
	// current visible projection.
	ActionCollapseAll

	// ActionSave persists the current state (e.g. a settings Flow page).
	ActionSave
	// ActionHelp opens/closes the full keybinding help overlay.
	ActionHelp
	// ActionFilter cycles the harness facet. Hierarchy-wide text search belongs
	// to ActionSearch.
	ActionFilter
	// ActionDeleteFilter removes the previous character while filter text is
	// being edited.
	ActionDeleteFilter
	// ActionKeepFilter exits filter editing while retaining the current query.
	ActionKeepFilter
	// ActionClearFilter exits filter editing and clears the current query. It
	// remains available while a kept query is active, so users always have a
	// visible way back to the unfiltered forest.
	ActionClearFilter
)

// AllActions returns the full closed set of real (non-ActionUnknown)
// ActionID values, in declaration order. Fixture-driven tests use this to
// guard that every ActionID has row coverage - a new ActionID added here
// without matching fixture rows fails those guards rather than silently
// shipping untested.
func AllActions() []ActionID {
	return []ActionID{
		ActionQuit,
		ActionConfirm,
		ActionBack,
		ActionUp,
		ActionDown,
		ActionLeft,
		ActionRight,
		ActionPageUp,
		ActionPageDown,
		ActionTop,
		ActionBottom,
		ActionNextProject,
		ActionPrevProject,
		ActionSearch,
		ActionFocusPaneLeft,
		ActionFocusPaneRight,
		ActionNextField,
		ActionPrevField,
		ActionToggle,
		ActionSelectAll,
		ActionSelectUnderProject,
		ActionExpand,
		ActionCollapse,
		ActionExpandLevel,
		ActionCollapseLevel,
		ActionExpandAll,
		ActionCollapseAll,
		ActionSave,
		ActionHelp,
		ActionFilter,
		ActionDeleteFilter,
		ActionKeepFilter,
		ActionClearFilter,
	}
}

// IsValid reports whether a is a known, non-ActionUnknown ActionID.
func (a ActionID) IsValid() bool {
	switch a {
	case ActionQuit, ActionConfirm, ActionBack,
		ActionUp, ActionDown, ActionLeft, ActionRight, ActionPageUp, ActionPageDown,
		ActionTop, ActionBottom, ActionNextProject, ActionPrevProject,
		ActionSearch,
		ActionFocusPaneLeft, ActionFocusPaneRight,
		ActionNextField, ActionPrevField, ActionToggle, ActionSelectAll, ActionSelectUnderProject,
		ActionExpand, ActionCollapse, ActionExpandLevel, ActionCollapseLevel, ActionExpandAll, ActionCollapseAll,
		ActionSave, ActionHelp, ActionFilter,
		ActionDeleteFilter, ActionKeepFilter, ActionClearFilter:
		return true
	default:
		return false
	}
}

// IsNavigation reports whether a is a movement/navigation action (cursor,
// paging, tree expand/collapse, field traversal) as opposed to an operation
// (confirm, back, toggle, save, help, ...). It is the single classifier the
// help overlay groups its rows by, mirroring the FTUE help overlay's
// "Navigation" vs "Actions" categories without re-listing bindings.
func (a ActionID) IsNavigation() bool {
	switch a {
	case ActionUp, ActionDown, ActionLeft, ActionRight,
		ActionPageUp, ActionPageDown, ActionTop, ActionBottom,
		ActionNextProject, ActionPrevProject,
		ActionExpand, ActionCollapse,
		ActionExpandLevel, ActionCollapseLevel, ActionExpandAll, ActionCollapseAll,
		ActionFocusPaneLeft, ActionFocusPaneRight,
		ActionNextField, ActionPrevField:
		return true
	default:
		return false
	}
}

// String returns a human-readable, kebab-case name for a, or "unknown" for
// ActionUnknown or any out-of-range value - mirroring
// internal/ingest.DiffStatus.String's default-case convention.
func (a ActionID) String() string {
	switch a {
	case ActionQuit:
		return "quit"
	case ActionConfirm:
		return "confirm"
	case ActionBack:
		return "back"
	case ActionUp:
		return "up"
	case ActionDown:
		return "down"
	case ActionLeft:
		return "left"
	case ActionRight:
		return "right"
	case ActionPageUp:
		return "page-up"
	case ActionPageDown:
		return "page-down"
	case ActionTop:
		return "top"
	case ActionBottom:
		return "bottom"
	case ActionNextProject:
		return "next-project"
	case ActionPrevProject:
		return "prev-project"
	case ActionSearch:
		return "search"
	case ActionFocusPaneLeft:
		return "focus-pane-left"
	case ActionFocusPaneRight:
		return "focus-pane-right"
	case ActionNextField:
		return "next-field"
	case ActionPrevField:
		return "prev-field"
	case ActionToggle:
		return "toggle"
	case ActionSelectAll:
		return "select-all"
	case ActionSelectUnderProject:
		return "select-under-project"
	case ActionExpand:
		return "expand"
	case ActionCollapse:
		return "collapse"
	case ActionExpandLevel:
		return "expand-level"
	case ActionCollapseLevel:
		return "collapse-level"
	case ActionExpandAll:
		return "expand-all"
	case ActionCollapseAll:
		return "collapse-all"
	case ActionSave:
		return "save"
	case ActionHelp:
		return "help"
	case ActionFilter:
		return "filter"
	case ActionDeleteFilter:
		return "delete-filter"
	case ActionKeepFilter:
		return "keep-filter"
	case ActionClearFilter:
		return "clear-filter"
	default:
		return "unknown"
	}
}

// Keymap maps each dispatchable ActionID to the key.Binding a screen
// matches key presses against. A Keymap built by anything other than
// [Default] is for tests only (see keymap_test.go) - production code has
// exactly one Keymap, [Default]'s.
type Keymap map[ActionID]key.Binding

// Availability is implemented by whatever the caller is currently rendering
// (a kit component, a settings Flow page) to report which ActionIDs are
// dispatchable RIGHT NOW. [Match], [FooterView], and [HelpEntries] all take
// an Availability and derive their answer from the SAME
// AvailableActions() call, which is what keeps dispatch, the footer hint
// bar, and the help overlay from drifting apart - the class of bug this
// package's audit found repeatedly (e.g. internal/tui/ftue/help.go's
// pageHelpCategories listing "b: back" and "q: quit" help text with no
// underlying binding that could ever produce a match).
type Availability interface {
	// AvailableActions returns the ActionIDs dispatchable in the caller's
	// current state, in priority order: if more than one available
	// action's binding matches a given key press, [Match] returns the
	// first one in this order.
	AvailableActions() []ActionID
}

// Default returns the ONE production Keymap every kit component and the
// settings Flow consume. It is built fresh on each call (a Keymap is a
// plain map, mutable by a caller that disables a binding for one render via
// key.Binding.SetEnabled) but every call returns the identical set of keys
// and help text - there is exactly one definition, here, and no page may
// define a second one (the internal/tui/gates key grep gate is what makes
// that a checkable claim rather than a convention nobody re-verifies).
func Default() Keymap {
	return Keymap{
		ActionQuit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		ActionConfirm: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "confirm"),
		),
		ActionBack: key.NewBinding(
			key.WithKeys("esc", "b"),
			key.WithHelp("esc", "back"),
		),
		ActionUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		ActionDown: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		ActionLeft: key.NewBinding(
			key.WithKeys("left", "h"),
			key.WithHelp("←/h", "left"),
		),
		ActionRight: key.NewBinding(
			key.WithKeys("right", "l"),
			key.WithHelp("→/l", "right"),
		),
		ActionPageUp: key.NewBinding(
			key.WithKeys("pgup", "ctrl+u"),
			key.WithHelp("PgUp", "page up"),
		),
		ActionPageDown: key.NewBinding(
			key.WithKeys("pgdown", "ctrl+d"),
			key.WithHelp("PgDn", "page down"),
		),
		ActionTop: key.NewBinding(
			key.WithKeys("g"),
			key.WithHelp("g", "go to top"),
		),
		// A shifted letter arrives as its uppercase text on most terminals and
		// as the "shift+" keystroke under the kitty protocol, so both forms are
		// bound.
		ActionBottom: key.NewBinding(
			key.WithKeys("shift+g", "G"),
			key.WithHelp("shift+g", "go to bottom"),
		),
		ActionNextProject: key.NewBinding(
			key.WithKeys("shift+j", "J"),
			key.WithHelp("shift+j", "next project"),
		),
		ActionPrevProject: key.NewBinding(
			key.WithKeys("shift+k", "K"),
			key.WithHelp("shift+k", "prev project"),
		),
		ActionSearch: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "search"),
		),
		// Vim window-navigation keys for the two panes of a split surface.
		//
		// ctrl+h is worth a note, because it is famously ambiguous: the keystroke
		// sends the single byte 0x08, which some terminals also send for
		// backspace. charm.land/bubbletea decodes 0x08 as ctrl+h and 0x7f as
		// backspace, so on a terminal that sends 0x7f for backspace - the common
		// case, and what kitty's and xterm's extended protocols encode
		// unambiguously - the two are distinct here and this binding is live. On
		// a terminal configured to send 0x08 for backspace they are one keystroke
		// no software can separate. PreviewSplit therefore treats 0x08 as filter
		// deletion only while its already-focused tree is editing search text; from
		// the preview it retains the focus-left meaning.
		ActionFocusPaneLeft: key.NewBinding(
			key.WithKeys("ctrl+h"),
			key.WithHelp("ctrl+h", "focus left pane"),
		),
		ActionFocusPaneRight: key.NewBinding(
			key.WithKeys("ctrl+l"),
			key.WithHelp("ctrl+l", "focus right pane"),
		),
		ActionNextField: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next field"),
		),
		ActionPrevField: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("shift+tab", "prev field"),
		),
		ActionToggle: key.NewBinding(
			key.WithKeys("space"),
			key.WithHelp("space", "select"),
		),
		ActionSelectAll: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "select all"),
		),
		ActionSelectUnderProject: key.NewBinding(
			key.WithKeys("shift+a", "A"),
			key.WithHelp("shift+a", "select project"),
		),
		ActionExpand: key.NewBinding(
			key.WithKeys("right", "l"),
			key.WithHelp("→/l", "expand"),
		),
		ActionCollapse: key.NewBinding(
			key.WithKeys("left", "h"),
			key.WithHelp("←/h", "collapse"),
		),
		ActionExpandLevel: key.NewBinding(
			key.WithKeys("shift+l", "L"),
			key.WithHelp("shift+l", "expand branches"),
		),
		ActionCollapseLevel: key.NewBinding(
			key.WithKeys("shift+h", "H"),
			key.WithHelp("shift+h", "collapse branches"),
		),
		ActionExpandAll: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("ctrl+p", "expand all"),
		),
		ActionCollapseAll: key.NewBinding(
			key.WithKeys("ctrl+o"),
			key.WithHelp("ctrl+o", "collapse all"),
		),
		ActionSave: key.NewBinding(
			key.WithKeys("ctrl+s"),
			key.WithHelp("ctrl+s", "save"),
		),
		ActionHelp: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		ActionFilter: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "filter"),
		),
		ActionDeleteFilter: key.NewBinding(
			key.WithKeys("backspace", "ctrl+h"),
			key.WithHelp("backspace", "delete"),
		),
		ActionKeepFilter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "keep filter"),
		),
		ActionClearFilter: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "clear filter"),
		),
	}
}

// Match resolves msg against km, restricted to the actions avail reports as
// currently dispatchable, and returns the first matching ActionID in
// avail.AvailableActions() order (see [Availability]'s doc for why order
// matters). ok is false if msg matches no available action's binding, or if
// an available ActionID has no entry in km at all (a caller-constructed
// Keymap missing an action it advertised as available - a caller bug, not
// a user input, so Match fails closed rather than panicking mid-render).
func Match(km Keymap, msg tea.KeyPressMsg, avail Availability) (ActionID, bool) {
	for _, action := range avail.AvailableActions() {
		binding, ok := km[action]
		if !ok {
			continue
		}
		if key.Matches(msg, binding) {
			return action, true
		}
	}
	return ActionUnknown, false
}

// HasPrintableBinding reports whether action has a key that a focused text
// editor receives as printable input. It lets a presentation remove shadowed
// actions from its effective Availability while editing, so dispatch, footer,
// and help all describe the same keys. "space" is the one printable key whose
// binding name is not itself a single rune.
func HasPrintableBinding(km Keymap, action ActionID) bool {
	binding, ok := km[action]
	if !ok {
		return false
	}
	for _, name := range binding.Keys() {
		if name == "space" {
			return true
		}
		runes := []rune(name)
		if len(runes) == 1 && unicode.IsPrint(runes[0]) {
			return true
		}
	}
	return false
}

// mustBinding returns km's binding for action and whether one exists. It
// does NOT panic: a missing entry (a Keymap built by hand instead of
// keymap.Default, or a new ActionID added without a binding for it) is
// reported via ok=false so the caller can fail closed - dispatchableEntries
// (the only caller) skips any action mustBinding reports missing, mirroring
// Match's own fail-closed handling of the same situation, rather than
// panicking mid-render over what fixture-driven tests should have already
// caught long before this ever runs. Shared by footer.go and help.go, which
// both derive their output from exactly the actions avail reports
// available.
func mustBinding(km Keymap, action ActionID) (key.Binding, bool) {
	b, ok := km[action]
	if !ok {
		return key.Binding{}, false
	}
	return b, true
}

// dispatchableEntry is one action from an Availability that km can actually
// dispatch (bound and enabled) - the shared derivation [FooterView] and
// [HelpEntries] both build on, so the footer hint bar and the help overlay
// can never list an action Match could not also resolve.
type dispatchableEntry struct {
	Action ActionID
	Help   key.Help
}

// dispatchableEntries derives, in avail's order, every action that is both
// dispatchable (bound, enabled) in km. This is the single computation
// FooterView and HelpEntries both call - see the package doc for why one
// derivation feeding all three consumers is the point.
func dispatchableEntries(km Keymap, avail Availability) []dispatchableEntry {
	actions := avail.AvailableActions()
	entries := make([]dispatchableEntry, 0, len(actions))
	for _, action := range actions {
		binding, ok := mustBinding(km, action)
		if !ok || !binding.Enabled() {
			continue
		}
		entries = append(entries, dispatchableEntry{Action: action, Help: binding.Help()})
	}
	return entries
}
