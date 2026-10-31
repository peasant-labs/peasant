# Sessions (`peasant sessions`)

`peasant sessions` inspects the sessions peasant has ingested into the local
analytics database. It is the terminal-first counterpart to the web dashboard:
`list` answers *"what sessions do I have?"*, `context` answers *"what happened
around this point in one session?"*, and `prompts` answers *"what did the person
ask for?"*.

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
peasant sessions list --include-subagents
peasant sessions list --has subagent,mcp --min-turns 5 --max-turns 200
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
| `--has <facet>[,<facet>...]` | — | Only sessions where every named facet holds; repeat it or use commas |
| `--min-turns <n>` | — | Only sessions with at least `n` turns, using the `turns` column |
| `--max-turns <n>` | — | Only sessions with at most `n` turns, using the `turns` column |
| `--sort <field>` | `date` | Sort by `date`, `turns`, `tokens`, or `project` |
| `--reverse` | — | Ascending instead of descending |
| `--json` | — | Emit a JSON array instead of a table |

Use `--harness` with the canonical harness values; see
[`docs/bestiary.md`](bestiary.md) for the full harness set and display names.

### Workflow facets

A facet says what happened inside a session. Peasant reads it from the stored
entries when the command runs, so nothing needs a re-index. An unknown facet, a
negative turn bound, or `--min-turns` above `--max-turns` exits with status 2
before the store opens. `--limit` applies after the facets.

| facet | holds when |
|---|---|
| `subagent` | The session has a child session, or it called the `Task` or `Agent` tool. |
| `skill` | The session ran a custom slash command, or it called the `Skill` or `skill` tool. |
| `mcp` | It called a tool whose name starts with `mcp__`. |
| `compact` | Its context was compacted: a compact boundary, or a compaction summary from Codex or OpenCode. Typing `/compact` alone does not count. |
| `clear` | A `/clear` command is recorded in the session. |
| `model-switch` | Its assistant turns observed more than one model. Claude Code's `<synthetic>` marker does not count as a model. |
| `background` | A tool call's input sets `run_in_background` to true. |
| `partial` | The session's active generation is marked partial. An older generation does not count. |

A facet can only report what the harness recorded. `mcp` sees only tool names
that start with `mcp__`, so a harness that names its MCP tools another way is not
matched. OpenCode's current format stores no model on its assistant turns and no
marker on a model switch, so `model-switch` does not hold for those sessions.

With `--json`, each row carries `facets`: every facet that holds, not only the
ones requested, in the order of the table above. It is `[]` when none holds. The
table output does not read facets unless `--has` asks for them.

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

`parent_index` names the parent entry. For a depth-1 entry it is the enclosing
depth-0 turn. For a depth-0 entry it is empty. A harness message graph, when one
exists, is carried on the entry `parent_entry_id` link instead: OpenCode records
a depth-0 message's parent message there. Renderers decide nesting by `depth`.

## `peasant sessions prompts`

Print only what the person typed in one session, in entry order. Use it to read
a session's requests before you publish it, without the harness text that
`context` and `export` include.

```bash
peasant sessions prompts <session-id>
peasant sessions prompts <session-id> --max-chars 200
peasant sessions prompts <session-id> --include-commands
peasant sessions prompts <session-id> --json | jq -r '.prompts[].text'
```

Flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--max-chars <n>` | `0` | Cut each prompt to `n` characters, on a character boundary (`0` = no cut). A cut prompt has `truncated: true` |
| `--include-commands` | — | In the text view, print a command entry's stored text instead of `/name args`. The JSON `command` field is always present |
| `--json` | — | Emit one JSON document instead of text |

The selection is the stored fields: `role = user`, `depth = 0` and
`entry_type = text`. The command adds no classification of its own. An entry
that a harness injected is left out only when ingest stored it as system
content. If a wrapper still reaches `role = user`, such as an IDE note from
Claude Code, the command lists it; the fix belongs in the indexer.

A slash-command or skill invocation stays in the list with `command` set, and
its `text` is `/name args`.

```json
{
  "warnings": [],
  "sessionId": "11111111-2222-4333-8444-555555555555",
  "parentSessionId": null,
  "promptCount": 2,
  "prompts": [
    { "entryIndex": 0, "timestamp": "2026-10-29T10:00:00Z", "text": "fix the widget", "truncated": false, "command": null },
    { "entryIndex": 9, "timestamp": "2026-10-29T10:12:00Z", "text": "/review 12", "truncated": false,
      "command": { "name": "review", "args": "12" } }
  ],
  "note": ""
}
```

- A subagent session has no human prompts. Its first user entry is the parent
  agent's instruction. The command lists the entries it finds, sets
  `parentSessionId`, and says so in `note`.
- A session with no prompts exits 0 with `prompts: []`, the empty code
  `no_matching_entries`, and a `note` that says why.
- A session whose capture is a bounded preview gets the warning
  `capture_bounded`: a long prompt may be cut. Text is otherwise whole, not the
  2,000-byte preview that `context` shows.
- A session that is not indexed is an error, exit 1, with the same text as
  `sessions context`.

The output is local and is not redacted.

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
  [`docs/cli/peasant_sessions_context.md`](cli/peasant_sessions_context.md),
  [`docs/cli/peasant_sessions_prompts.md`](cli/peasant_sessions_prompts.md) —
  auto-generated flag references
- [Annotation CLI reference](cli/peasant_annotate.md) — commands for creating and importing
  annotations
- [`docs/bestiary.md`](bestiary.md) — harness identifiers used by `--harness`
