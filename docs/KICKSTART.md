# Kickstart

`peasant kickstart` opens Peasant's guided local setup flow. It discovers recorded agent sessions,
walks through the canonical settings one section at a time, saves once after review, and then imports
the selected sessions into the local Peasant store.

Kickstart never publishes a transcript. Sharing remains a separate, explicit action.

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
   explicit publication. Local imports remain original unless you explicitly run `peasant redact`.
   If kickstart cannot validate an example, it withholds that unverified output and displays an
   actionable error; the settings flow remains available.
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

Each visible section begins with a short guide band before its normal setting fields. Conditional
sections and their guidance disappear together. The fields, validation, and persisted values are the
same canonical definitions used by the dense config editor.

On the transcript-selection step, the left pane is the project tree and the right pane previews the
highlighted session. An imported session is read through the same local-store path used by the
transcript viewer. A session that has not been imported yet is identified as such rather than shown
as an empty recording.

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

After a successful import, the completion screen reports that kickstart published nothing and offers
three separate next steps:

```text
peasant web start
peasant village login
peasant village push
```

`peasant web start` reports the real local address when it starts. Kickstart does not invent a
localhost URL. Village login and push remain separate, explicit actions.

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
starting kickstart again. Without `--reset`, kickstart loads the resolved `--config` or `--config-dir`
file and preserves unrelated settings and custom paths.

Other lifecycle actions remain separate:

- Publish with the explicit share or Village push flow.
- Install or remove repository upload hooks with `peasant village hooks` commands.
- Remove local transcript data with `peasant prune`.

See also:

- [Kickstart CLI reference](cli/peasant_kickstart.md)
- [TUI keyboard shortcuts](TUI.md)
