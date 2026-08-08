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
4. **Privacy:** review the currently offered Standard redaction policy for locally stored transcripts.
5. **Content license:** choose the default license for a later explicit share. Saving the default does
   not publish anything.
6. **Sharing visibility:** when connected to a Village, choose the default visibility for later
   explicit pushes. This section is hidden while disconnected.
7. **Claude retention:** when Claude Code sessions were discovered, choose how long Claude Code keeps
   its local transcript files.
8. **Review and save:** review the buffered values, then confirm the single config commit.

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

In the selection tree, use left/right or `h`/`l` to collapse and expand, `f` to filter, and the page or
jump keys shown in the footer. `ctrl+l` moves focus to the preview pane and `ctrl+h` returns focus to
the tree.

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
