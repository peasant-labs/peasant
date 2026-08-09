# TUI Keyboard Shortcuts

Peasant's kit-based TUI screens derive dispatch, footer hints, and help from one
typed action catalog in `internal/tui/keymap`, built on
[`charmbracelet/bubbles/key`](https://pkg.go.dev/github.com/charmbracelet/bubbles/key).
The legacy wizard pages still use the older layered keymaps documented later in
this file.

## Config screen

Run `peasant config` to open the dense settings editor. `peasant settings` is an
alias for the same command and mounts the same screen. The editor uses the same
settings registry as kickstart, but presents it as a section list rather than a
guided sequence.

Edits stay in a buffered draft until an explicit save. The section list and each
edited field show a modified marker. Only currently visible fields contribute to
those markers. If an earlier answer hides a section or field, its buffered edit
is dropped before validation and save. The screen's own status line names
`ctrl+s` as the save action. Once a save commits, field and discard input pause
until the matching completion is handled, so post-save effects observe exactly
the values that were committed.

| Action | Keys | Description |
|--------|------|-------------|
| Choose section | `↑` `k` `↓` `j` | Move through the section list |
| Enter section | `enter` or `tab` | Focus the selected section's first field |
| Move between fields | `tab` `shift+tab` | Move field focus or return to the section list |
| Save | `ctrl+s` | Validate visible fields and persist applicable changes |
| Leave without saving | `esc` `b` `q` | Open the discard confirmation |
| Help | `?` | Show the bindings available for the current focus |

After the exact-byte drift check, saving an existing semantically clean config
is a successful byte-preserving no-op, so comments, ordering, omitted defaults,
and formatting remain intact. A config path that was missing when the editor
opened is still created. A transient-only retention change also completes this
config step and emits the normal save completion so its external write can run.

Peasant opens the resolved Claude settings path once before mounting the editor.
A missing file or retention key uses the recommended value; unreadable,
malformed, non-object, or invalid settings stop before editing. When retention
changed, the config step completes first. Peasant then strictly rereads the same
bound Claude path, preserves valid unrelated values added while the TUI was
open, and atomically replaces the document once. A late malformed or unreadable
document, or a changed cleanup value, fails before rename and leaves the current
destination unchanged. A save with no visible changes, discard, validation
failure, or config drift failure performs no retention write. Post-config
retention failures report an honest partial save with both paths and repair
guidance. The config command edits settings only; it does not import or share
transcripts.

The config screen and other kit surfaces derive dispatch, footer hints, and help
from the shared `internal/tui/keymap` action catalog. The older page-specific
reference below remains for legacy wizard pages that have not yet moved to the
kit.

## Kickstart transcript selection

The guided kickstart selection step uses a project → branch → session tree. Its
status rows distinguish three independent facts:

- `tracked` means the row was included by the previous saved selection;
- `already imported` means transcript data is already in the local store; and
- the checkbox is the current buffered choice for this run.

Long trees show `↑`, `↓`, or `↕` in the row margin when more rows exist above,
below, or in both directions. The selected-session summary also reports how many
selected sessions are hidden by the current text or harness view.

| Action | Keys | Description |
|--------|------|-------------|
| Move | `↑` `k` `↓` `j` | Move through visible rows |
| Page | `PgUp` `Ctrl+u` `PgDn` `Ctrl+d` | Move one visible page |
| First / last row | `g` `Shift+g` | Jump to the start or end |
| Previous / next scope | `[` `]` | Move among project, branch, and session scope |
| Search current scope | `/` | Start a text filter at the named scope |
| Type filter | any printable key | Append text while search is editing |
| Delete search text | `backspace` | Remove the previous character |
| Keep filter | `enter` | Leave search editing while keeping the filter |
| Clear filter | `esc` | Restore the complete tree |
| Harness view | `f` | Cycle all harnesses, each harness, and hidden gutter |
| Select row | `space` | Toggle the visible row and its visible descendants |
| Select visible tree | `a` | Select or clear the current projected tree |
| Select current project | `Shift+a` | Select visible sessions under the current project |
| Collapse / expand | `←` `h` / `→` `l` | Change tree expansion without changing scope |
| Focus tree / preview | `Ctrl+h` / `Ctrl+l` | Move input across the preview divider |
| Help | `?` | Show actions available for the current focus |

Search keeps ancestor rows for context and shares session nodes with the full
forest, so selecting a filtered row changes the real buffered choice exactly
once. Clearing restores the complete forest, expansion state, and the current
row when it still exists. While a filter remains active, the status line keeps
the clear path visible, including when preview focus requires returning to the
tree first. Footer and help entries describe only actions that can run in the
current state: a leaf does not advertise expand or collapse, edge navigation is
omitted when it cannot move, and the harness key appears only after its values
load while the tree pane has focus.

The harness control is view-only: rows found only under an excluded harness are
hidden, mixed-harness projects remain through included data, and hidden
selections remain selected and counted. Changing the harness view does not
rewrite saved selection intent. A highlighted canonical row keeps its viewport
anchor when it remains in the projected view; returning to the complete view
restores that anchor even if an intermediate harness view hid it.

## Legacy wizard keymap architecture

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

### Split preview pane (rebuilt "choose transcripts to import" step)

The rebuilt selection step shows the session tree on the left and a preview of the
highlighted session on the right. The divider between them carries a focus marker
(`<` while the tree takes input, `>` while the preview does), and the hint bar at the
bottom always lists the keys the focused pane actually dispatches.

| Action | Keys | Description |
|--------|------|-------------|
| Focus left pane | `Ctrl+h` | Send input to the session tree |
| Focus right pane | `Ctrl+l` | Send input to the preview |
| Scroll preview | `↑` `k` `↓` `j` | Move the preview one line (when the preview is focused) |
| Page preview | `PgUp` `PgDn` `Ctrl+u` `Ctrl+d` | Move the preview one pane |
| Preview to top/bottom | `g` `Shift+g` | Jump the preview to its first/last line |

`Ctrl+h` is famously ambiguous, so it is worth stating what happens here: the keystroke
sends the byte `0x08`, which some terminals also send for backspace. Bubble Tea decodes
`0x08` as `Ctrl+h` and `0x7f` as backspace. On a terminal that sends `0x7f` for backspace,
the two are distinct and this binding is live; that is the common case, and it is what
kitty's and xterm's extended protocols encode unambiguously. On a terminal configured to
send `0x08` for backspace they are one keystroke that no software can separate. While the
already-focused tree is editing search text, the selection split treats that byte as
delete; from the preview pane it retains the focus-left meaning.

The preview renders the recorded first user message as markdown, with fenced code
syntax-highlighted in the app's own palette (`internal/tui/mdrender`).

### Legacy search mode (inside the legacy session tree)

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
