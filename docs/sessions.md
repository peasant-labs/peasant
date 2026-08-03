# Sessions (`peasant sessions`)

`peasant sessions` inspects the sessions peasant has ingested into the local
analytics database. It is the terminal-first counterpart to the web dashboard:
`list` answers *"what sessions do I have?"* and `context` answers *"what
happened around this point in one session?"* — both rendered for humans in the
terminal.

> **`sessions context` vs `export sessions`:** `sessions context` produces
> **human-readable terminal rendering** of a window of a transcript. `peasant
> export sessions` produces a **machine-readable JSON full-dump**
> (`SessionDetailPayload`) for tooling and the annotation pipeline. Reach for
> `context` when reading, `export` when piping.

## Prerequisites

- `peasant` binary built: `make build`
- Sessions ingested: `peasant ingest` (populates `sessions` and, via the indexer,
  `session_entries`)

## `peasant sessions list`

List ingested sessions, most recent first, as a table (or JSON).

```bash
peasant sessions list
peasant sessions list --limit 50
peasant sessions list --harness claude-code --since 7d
peasant sessions list --project peasant --sort tokens
peasant sessions list --json | jq '.[].id'
```

Flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--limit <n>` | `20` | Maximum sessions to show (`0` = no limit) |
| `--harness <h>` | — | Filter by harness: `claude-code`, `gemini-cli`, `codex`, `opencode` |
| `--project <name>` | — | Filter by project name (matches git remote URL or directory basename) |
| `--tag <tag>` | — | Filter by session tag (see [Tags](#peasant-sessions-tag)) |
| `--since <when>` | — | Sessions starting after a date (`7d`, `24h`, `2026-01-01`) |
| `--until <when>` | — | Sessions starting before a date |
| `--sort <field>` | `date` | Sort by `date`, `turns`, `tokens`, or `project` |
| `--reverse` | — | Ascending instead of descending |
| `--json` | — | Emit a JSON array instead of a table |

Use `--harness` with the canonical harness values; see
[`docs/bestiary.md`](bestiary.md) for the full harness set and display names.

## `peasant sessions context`

Show a window of transcript entries centered on a target entry, for reading what
happened around a specific point in a session.

```bash
# List recent sessions to pick one (when --session is omitted)
peasant sessions context

# Show 3 entries before/after entry_index 42
peasant sessions context --session <session-id> --turn 42

# Widen the window to 10 on each side
peasant sessions context --session <session-id> --turn 42 -C 10

# Compact tool-call rendering, or hide tool calls entirely
peasant sessions context --session <session-id> --turn 42 --format-tool-calls compact
peasant sessions context --session <session-id> --turn 42 --format-tool-calls quiet

# JSON for tooling
peasant sessions context --session <session-id> --turn 42 --json
```

Flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--session <id>` | — | Session to inspect (omit to list recent sessions to pick from) |
| `--turn <n>` | `0` | Target `entry_index` — the **raw DB index**, not a dense ordinal; `0` = first indexed entry |
| `-C, --context <k>` | `3` | Entries to show before and after the target, clamped to session boundaries |
| `--format-tool-calls <mode>` | `verbose` | `verbose` (full box), `compact` (one line), or `quiet` (hidden) |
| `--json` | — | Emit JSON instead of terminal rendering |

Each depth-0 and depth-1 entry is rendered on its own line; depth-1 entries are
indented and tool entries use box-drawing characters. `--turn` indexes by raw
`entry_index` (the value stored in `session_entries`), so it lines up with the
indices reported elsewhere rather than a re-counted ordinal.

## `peasant sessions tag`

Session tags are free-form labels you can attach to sessions and then filter on
with `sessions list --tag`.

```bash
peasant sessions tag add <session-id> <tag>      # add a tag
peasant sessions tag remove <session-id> <tag>   # remove a tag
peasant sessions tag set <session-id> <tags...>  # replace all tags
peasant sessions tag list <session-id>           # show a session's tags
```

## See also

- [`docs/cli/peasant_sessions_list.md`](cli/peasant_sessions_list.md),
  [`docs/cli/peasant_sessions_context.md`](cli/peasant_sessions_context.md) —
  auto-generated flag references
- [Annotation CLI reference](cli/peasant_annotate.md) — commands for creating and importing
  annotations
- [`docs/bestiary.md`](bestiary.md) — harness identifiers used by `--harness`
