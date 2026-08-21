# Kickstart

`peasant kickstart` opens Peasant's guided local setup flow. It discovers recorded agent sessions,
walks through the canonical settings one section at a time, saves once after review, and then imports
the selected sessions into the local Peasant store.

Kickstart never publishes a transcript. Sharing remains a separate, explicit action.

<!-- verified-example: kickstart-commands -->
```bash
peasant kickstart
peasant kickstart --reset
```

`peasant ftue` is an alias for the same command. Kickstart currently starts only when invoked; the
bare `peasant` command does not automatically open it.

## Guided flow

Discovery runs before the interactive screen. The guided flow then presents:

1. **Village connection:** connect now or continue locally. A machine with valid credentials skips
   this prompt. Connecting only authenticates the machine; publishing stays explicit and opt-in.
2. **Transcript selection:** choose all recorded sessions or narrow the project-first tree by project,
   branch, or session. The harness facet filters the same tree without redefining the selection.
3. **New branches:** for a narrowed selection, choose whether future branches in fully selected
   projects should be imported automatically. This section is hidden when all sessions are selected.
4. **Privacy:** review synthetic examples processed by the real Standard redactor used before a later
   explicit publication. Each canonical category leads a tokenized diff: `- before:` marks removed
   input and `+ after:` marks replacement output, so color is never the only distinction. Local
   imports remain original unless you explicitly run `peasant redact`. If kickstart cannot validate
   an example, it withholds that unverified output and displays an actionable error; the settings flow
   remains available.
5. **Content license:** choose the default license for a later explicit share. No license is the
   default, and saving any default does not publish anything.
6. **Sharing visibility:** when connected to a Village, choose the default visibility for later
   explicit pushes. A disconnected flow offers an optional login at this boundary before continuing
   locally without the visibility section. Login keeps the buffered selection, filters, tree position,
   and preview focus in place, and publishes nothing.
7. **Claude retention:** when Claude Code sessions were discovered, choose how long Claude Code keeps
   its local transcript files.
8. **Review and save:** review every visible buffered value and the promised local effects, then
   confirm one config commit. The consent summary states that kickstart publishes nothing.

Where a section includes narrative guidance, it shows its setting heading first, then its short guide
band and field description, then the control. Background-bearing guide and diff rows fill the complete
available line in both themes. Conditional sections and their guidance disappear together. The fields,
validation, and persisted values are the same canonical definitions used by the dense config editor.

On the transcript-selection step, the left pane is the project tree and the right pane previews the
highlighted session. An imported session is read through the same local-store path used by the
transcript viewer. A session that Peasant has not imported yet is read from the transcript its
harness wrote, which discovery found before any import. Peasant reads that file in place. It copies
no transcript, and it writes nothing to disk or to the local store to show the preview. Kickstart
keeps the turns of the last few previewed sessions in memory only, and it drops them when it exits.
A session that discovery found no transcript for is identified as not imported yet.

Changing selection controls future discovery and import lists. It does not delete sessions already in
the local store and does not remove copies shared previously.

## Save, retention, and local import

Kickstart buffers edits in a draft. Moving between sections writes nothing. The final confirmation:

1. drops edits from settings that became hidden;
2. validates every visible field;
3. atomically commits the resolved configuration file, rejecting concurrent changes;
4. applies the selected Claude retention value when that section was offered; and
5. runs the existing local ingest path over the committed selection.

The order is config first, Claude retention second, local import third. There is no push client,
publisher, publication receipt, or background sharing step in this path.

Local import preserves the original recorded transcript content. The configured privacy level protects
a later explicit publication; use `peasant redact` when you intentionally want to rewrite local data.

While local import runs, kickstart names the real import stages and shows elapsed time. It only offers
a qualified estimate when completed bounded work supports one. The completion screen remains visible
until you explicitly leave it. If import fails, the committed configuration and any applied retention
setting remain in force; retry reruns only local import and does not recommit or reapply either setting.

After a successful import, the completion screen reports that kickstart published nothing and introduces
the command list with: "these useful next steps let you modify config, open the local dashboard,
connect to a village, or explicitly publish later; kickstart runs none of them."

```text
peasant config
peasant web start
peasant village login
peasant village push
```

`peasant config` opens the interactive settings editor without importing or publishing. `peasant web
start` reports the real local address when it starts, so kickstart does not invent a localhost URL.
Village login and push remain separate, explicit actions.

Pressing `esc` or `q` opens a leave-without-saving confirmation. Confirming that exit leaves the config
and Claude settings bytes unchanged and does not run local ingest.

## Dense config editor

Use `peasant config` when setup guidance is not needed. `peasant settings` is an alias for the same
dense section-navigation screen. It mounts the same registry and fields as kickstart but intentionally
does not render the guide bands.

The config editor saves with `ctrl+s`, confirms discard on exit, and never imports or shares sessions.
If its Claude retention value changed, it commits config first and writes retention once afterward.
See [TUI keyboard shortcuts](TUI.md#config-screen) for its controls.

## Keyboard controls

| Action | Keys |
|--------|------|
| Move within a field | `arrow keys` or `j` / `k` where offered |
| Toggle or select | `space` |
| Next guided section | `tab` |
| Previous guided section | `shift+tab` |
| Confirm final review | `enter` |
| Leave without saving | `esc` or `q`, then confirm |
| Show or close help | `?` |

In the selection tree, use left/right or `h`/`l` to collapse and expand, `f` to cycle the harness view,
and `/` to start filtering. Type a query and press `enter` to keep the filter or `esc` to clear it.
The page and jump keys are shown in the footer. `ctrl+l` moves focus to the preview pane and `ctrl+h`
returns focus to the tree.

## Reset and standalone boundaries

`--reset` removes Peasant configuration, credentials, database, ingested data, and state before
starting again. Without `--reset`, kickstart loads the resolved `--config` or `--config-dir` file and
changes that configuration. It preserves unrelated settings, custom source paths, and other loaded
values. Kickstart does not rebuild the file from defaults or silently write a different path.

## Restore saved choices

On a later run, kickstart restores each available saved project, branch, and explicit session choice.
It applies these saved choices after the first successful project-tree load. If the tree refreshes
during the same run, it keeps edits that you already made on the screen.

A project that Peasant finds for the first time starts clear. This is also true after you used
**Select all projects** on an earlier run. That action saves only the projects on the current screen.
It does not select projects that Peasant finds later.

A new branch in a selected project follows the saved `autoIngestNewBranches` value:

- `true` lets discovery include a branch that is not in the saved branch list.
- `false` keeps discovery on the saved branch list. A project choice with no branch list still means
  all branches in that selected project.

An exact branch exclusion still wins when `autoIngestNewBranches` is `true`. Clearing one available
branch records a denial for that physical clone and branch. It does not disable automatic discovery
for another branch or a sibling clone.

Kickstart keeps a saved project, clone path, branch, or session when the current scan cannot offer it
for editing. Another visible edit does not remove the unavailable choice. Kickstart removes an
available choice only after you clear it and save. If an unavailable branch belongs to an available
project, clearing that project and saving removes the branch with its parent.

## Physical clone identity

Peasant identifies a local clone with a resolved absolute physical path. It resolves symbolic links
before it compares or saves the path. A symbolic link and its target identify the same clone.

One Git remote entry can contain several `clonePaths`. One shared `branches` list applies to every
path in that entry. Peasant can use a Git remote or project name only when the current scan finds one
physical clone with that value. Peasant does not use a remote or name alone to choose between
ambiguous clones. An old selection entry without `clonePaths` stays readable and follows the same
uniqueness rule until kickstart migrates it from stored clone evidence.

For a project with no Git remote, Peasant uses the resolved path as identity. Kickstart shows the
project name with a short path when it must distinguish equal names. See the
[Selection index example](../README.md#selection-index) for the saved YAML shape.

## Exact exclusions

Each harness can store exact branch and session exclusions. Peasant applies positive project and
session rules first. It then applies these exact exclusions.

- A branch exclusion contains one resolved `clonePath` and one or more branch names. It does not
  exclude the same branch in another clone.
- A session exclusion contains one exact session ID. It still excludes that session when a project
  rule admits the session.
- Selecting the branch or session again removes its exclusion.

Harvest, viewer lists, the save gate, push, and `peasant prune --unselected` use the same matcher.
Direct links to stored history remain available. An exclusion changes discovery. It does not delete
local data.

## Convert old pathless selected rules

An old `selection.mode: selected` entry can identify a project by name or Git remote without a
`clonePaths` list. On the next kickstart run, Peasant compares that entry with sessions in the local
store. It migrates every matching stored physical clone to exact paths. A matching clone found only
by the current scanner starts clear.

When several saved rules apply to one matching stored clone, Peasant unions their branch lists,
removes duplicates, and sorts the result. For example, `[dev, main]` plus `[main, release]` becomes
`[dev, main, release]`. Disjoint lists such as `[main]` and `[release]` retain the clone with both
branches. If any matching rule has no branch list, the migrated clone remains unrestricted. An
already path-bound rule for that stored clone also contributes its saved branches while kickstart
canonicalizes the cohort.

Same-remote Git clones share one entry only when their resulting branch unions are equal. Unresolved
stored sessions remain exact session choices only when the canonical positive matcher admits them
after exact exclusions.

This migration runs in memory before the project editor opens. Peasant writes the exact paths only
after you confirm the final save. No, Back, quit, or cancel leaves the old file unchanged.

## Convert an old all-projects setting

An old `selection.mode: all` value contains no exact project list. On the next kickstart run, Peasant
prepares an exact `selected` list in memory. It uses sessions that are in the local store at the time
of that run.

- A scanned project starts selected only when Peasant can match it to a current stored session by
  harness and resolved physical path.
- A newly scanned project with no matching stored session starts clear.
- A stored session whose project is not available in the scan stays saved as an explicit session ID.
- The saved `autoIngestNewBranches` value does not change.

The conversion does not write the config when the screen opens. It writes only after you review and
confirm the final save. If you decline or cancel, the file stays unchanged and the conversion runs
again on the next kickstart run. Peasant does not have an old project snapshot. It cannot reconstruct
which projects existed when the old setting was written.

## Save with no effective project

Before the final write, kickstart checks the current selection against evidence from the current
discovery scan and the local session store. It asks for confirmation only when no effective project
and no available selected child session remain. A selected child with canonical project evidence
makes its parent project effective. A selected explicit session that is already in the local store can
also suppress the warning even when the discovery scan cannot find its project or path. Peasant keeps
its exact descendant identity and does not guess a project identity. A saved choice that is absent
from both the scan and the local store stays in the config, but it cannot suppress the warning or
create an unknown project row.

The confirmation states these effects:

- Existing projects stay ingested and indexed.
- The web viewer does not list them.
- They are not available for a future push until you change the selection.
- Peasant does not delete data.

The No choice is selected by default. Kickstart asks again on every run that reaches this state.
Choosing No or Back returns without a write. If you cancel, kickstart exits without a write. These
paths do not start ingest. Yes saves the no-project choice through the normal atomic config write. It
does not delete existing data.

## Viewer lists and stored data

Home, Map, and the command palette show the same project list. If one available child session is
selected, its stored parent project appears in that list. Showing the parent does not select its
sibling sessions. The normal push chooser still offers only sessions that the saved selection admits.

When the saved selection hides every stored project, Home and Map show one recovery panel. The panel
shows aggregate hidden project and session counts. It does not show project names, session IDs, or
paths. It states that the data stays ingested and indexed, is absent from the web viewer, and is not
available for a future push. It also states that Peasant did not delete data. The panel provides a
copyable `peasant kickstart` command. A store with no recorded projects keeps the first-use ingest
message instead.

Selection scopes discovery, viewer lists, and future push choices. It is not an access-control rule
for stored history. A direct link to a stored session or canonical project still resolves. Saving a
narrow selection does not delete files or database rows, and it does not unpublish Village copies.

`peasant prune --unselected` is a separate destructive command. It requires
`selection.mode: selected`. It checks harness, project, clone, branch, explicit session, and exact
exclusion rules. A stored session stays only when the complete saved selection admits its recorded
branch and exact identity.

Preview the deletion first. The second command shows the deletion preview and asks for confirmation
before it permanently deletes the listed sessions from the local database and filesystem.

<!-- verified-example: unselected-prune-commands -->
```bash
peasant prune --unselected --dry-run
peasant prune --unselected
```

Kickstart never runs this command. Changing the selection does not delete stored data.

Other lifecycle actions remain separate:

- Publish with the explicit share or Village push flow.
- Install or remove repository upload hooks with `peasant village hooks` commands.
- Remove local transcript data with `peasant prune`.

See also:

- [Kickstart CLI reference](cli/peasant_kickstart.md)
- [TUI keyboard shortcuts](TUI.md)
