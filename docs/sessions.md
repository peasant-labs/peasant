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

## `peasant sessions review`

Redaction removes what its rules know. `sessions review` looks for what is left
in sessions you name, before you publish them. It reads each session, runs your
configured redactor on it in memory, and scans the result. It writes nothing,
and it does not stop a publish on its own.

```bash
peasant sessions review --session <id>
peasant sessions review --session <id> --owner-email you@example.org
peasant sessions list --json --limit 20 | peasant sessions review --sessions-from - --json
```

| kind | what it found |
|---|---|
| `credential_assignment` | A secret-named `name: value` whose value is long, high in entropy, and known to no rule. |
| `credential_file_output` | A tool result from reading a credential-shaped file, or one listing two or more secret-named `NAME=value` lines. |
| `foreign_email` | An address that survived redaction and is not yours. Your addresses are your git identity and each `--owner-email`. |
| `shareable_document_link` | A link on a closed list of document hosts that anyone holding it can open. |
| `redaction_residue` | A hit from the redaction module's advisory rules. These are broad on purpose, so expect some on ordinary text such as long paths and hashes. |

A finding shows a masked preview, such as `secret_key=**** (len 32-63)`, and
never the matched text. Text output is one line per finding,
`<session> entry <n> <role> <kind> <preview>`, then a total.

Each session gets a status. `clean` means it was read in full and nothing was
found. `findings` means it has at least one finding. `error` means it could not
be reviewed, and `reason` says why: a session stored only as a bounded preview
is an error, never clean. A session's subagent sessions are reviewed with it, as
`village push` sends them with it, so its status covers them. A finding's
`sessionId` names the session it is in, and `subagents` counts the subagent
sessions reviewed.

The exit status is 0 when every session is clean, 4 when at least one has
findings and none failed, and 1 when at least one could not be reviewed. With
no session named it exits 2 and reads nothing.

The `--json` document can be piped into a command that takes `--sessions-from`,
which then takes only the clean sessions.
[The CLI contract](cli-contract.md#piping-a-review-into-a-publish) explains why
that holds even when the shell ignores the exit status.

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
  [`docs/cli/peasant_sessions_review.md`](cli/peasant_sessions_review.md) —
  auto-generated flag references
- [Annotation CLI reference](cli/peasant_annotate.md) — commands for creating and importing
  annotations
- [`docs/bestiary.md`](bestiary.md) — harness identifiers used by `--harness`
