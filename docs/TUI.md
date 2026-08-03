# TUI Keyboard Shortcuts

Peasant's TUI pages use a layered keymap system built on [`charmbracelet/bubbles/key`](https://pkg.go.dev/github.com/charmbracelet/bubbles/key). A shared base keymap (`PageKeyMap`) provides common bindings; page-specific keymaps embed it and add their own.

## Architecture

```
PageKeyMap (shared base)
├── Up         ↑/k
├── Down       ↓/j
├── Select     space
├── Confirm    enter
├── Cancel     esc
└── Help       ?

TreeKeyMap (embeds PageKeyMap)
├── PageKeyMap (all base bindings)
├── PageUp           Shift+k/PgUp/Ctrl+u
├── PageDown         Shift+j/PgDn/Ctrl+d
├── JumpTop          Shift+h
├── JumpBottom       Shift+l
├── Expand           l/→
├── Collapse         h/←
├── PrevSibling      [/⌥k/⌥↑
├── NextSibling      ]/⌥j/⌥↓
├── Search           f
└── ConfirmSelection tab
```

Each binding is a `key.Binding` with multiple key aliases and help metadata:

```go
key.NewBinding(
    key.WithKeys("up", "k"),       // accepted keys
    key.WithHelp("↑/k", "up"),     // displayed in help bar
)
```

Matching uses `key.Matches(msg, binding)` instead of raw string comparison, so adding a new alias is a one-line change in the binding definition.

## Keybind Reference

### All wizard pages (`PageKeyMap`)

| Action | Keys | Description |
|--------|------|-------------|
| Up | `↑` `k` | Move cursor up |
| Down | `↓` `j` | Move cursor down |
| Select | `space` | Toggle / select item |
| Confirm | `enter` | Confirm selection |
| Cancel | `esc` | Cancel / clear filter |
| Help | `?` | Show/hide help overlay |

### Session tree (`TreeKeyMap`)

Includes all `PageKeyMap` bindings plus:

| Action | Keys | Description |
|--------|------|-------------|
| Page down | `Shift+j` `PgDn` `Ctrl+d` | Jump cursor down by one page |
| Page up | `Shift+k` `PgUp` `Ctrl+u` | Jump cursor up by one page |
| Jump to top | `Shift+h` | Jump cursor to first item |
| Jump to bottom | `Shift+l` | Jump cursor to last item |
| Expand | `l` `→` | Expand provider, remote, or worktree |
| Collapse | `h` `←` | Collapse provider, remote, or worktree |
| Prev section | `[` `⌥k` `⌥↑` | Jump to previous sibling section |
| Next section | `]` `⌥j` `⌥↓` | Jump to next sibling section |
| Search | `f` | Enter search/filter mode |
| Confirm selection | `tab` | Open confirm overlay (requires at least one session selected) |

### Search mode (inside session tree)

When search is active (`f`), normal navigation keys are intercepted as text input.

| Action | Keys | Description |
|--------|------|-------------|
| Type | any printable key | Append to filter text |
| Delete | `backspace` | Remove last character |
| Keep filter | `enter` | Exit search, keep filter active |
| Clear filter | `esc` | Exit search, clear filter |

### Help overlay

Pressing `?` on any page that supports it opens a centered box showing all
bindings grouped by category. Press `?` or `Esc` to close.

For simple pages (PageKeyMap):

```
┌──────── Help ────────┐
│                      │
│  Navigation          │
│    ↑/k       up      │
│    ↓/j       down    │
│                      │
│  Actions             │
│    space     select  │
│    enter     confirm │
│    ?         help    │
│                      │
│  Press ? or Esc to close │
└──────────────────────┘
```

For the session tree (TreeKeyMap):

```
┌─────────────── Help ───────────────┐
│                                    │
│  Navigation                                   │
│    ↑/k                up                       │
│    ↓/j                down                     │
│    Shift+k/PgUp/Ctrl+u  page up                │
│    Shift+j/PgDn/Ctrl+d  page down              │
│    Shift+h             jump to top              │
│    Shift+l             jump to bottom           │
│                                    │
│  Selection                         │
│    space     select                │
│    enter     confirm               │
│    tab       confirm               │
│                                    │
│  Tree                              │
│    l/→       expand                │
│    h/←       collapse              │
│    [/Alt+k   prev section          │
│    ]/Alt+j   next section          │
│    f         search                │
│    ?         help                  │
│                                    │
│  Press ? or Esc to close           │
└────────────────────────────────────┘
```

### Grouped help bar

Pages display a multi-line help bar at the bottom. Each category's bindings
appear on a separate line. The format is `key: description` pairs separated by
two spaces.

For simple pages (two lines):

```
↑/k: up  ↓/j: down
space: select  enter: confirm  ?: help
```

For the session tree (three lines):

```
↑/k: up  ↓/j: down  Shift+k/PgUp/Ctrl+u: page up  Shift+j/PgDn/Ctrl+d: page down  Shift+h: jump to top  Shift+l: jump to bottom
space: select  enter: confirm  tab: confirm
l/→: expand  h/←: collapse  [/Alt+k: prev section  ]/Alt+j: next section  f: search  ?: help
```

### Wizard-level (global)

These are handled by `WizardModel`, not individual pages:

| Action | Keys | Description |
|--------|------|-------------|
| Back | `b` | Go to previous page |
| Restart | `r` | Restart wizard from first page |
| Quit | `q` `ctrl+c` | Exit the wizard |

## Adding keybinds to a new page

1. Define a page-specific keymap that embeds `PageKeyMap`:

```go
type MyPageKeyMap struct {
    ftue.PageKeyMap
    CustomAction key.Binding
}

var DefaultMyPageKeyMap = MyPageKeyMap{
    PageKeyMap: ftue.DefaultPageKeyMap,
    CustomAction: key.NewBinding(
        key.WithKeys("x"),
        key.WithHelp("x", "do thing"),
    ),
}
```

2. Add a `keymap` field to your page struct, initialized in the constructor.

3. Use `key.Matches(msg, p.keymap.Up)` in your `Update()` method.

4. Build the help bar from binding metadata via `binding.Help()`.

## Source files

| File | Contents |
|------|----------|
| `internal/tui/ftue/keymap.go` | `PageKeyMap`, `TreeKeyMap`, and their defaults |
| `internal/tui/ftue/page.go` | `TreeSelectPage.Update()` — uses `key.Matches()` |
| `internal/tui/ftue/help.go` | Help overlay (`renderHelpOverlay`), grouped help bar (`helpBarGrouped`), category definitions |
| `internal/defaults/tui.go` | Legacy `defaults.Key` constants (used by non-tree pages) |
