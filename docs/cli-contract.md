# The CLI contract for scripts and agents

This page describes what a peasant command promises a program that drives it: a
shell script, a CI job, or a coding agent. The rules are implemented once, in
`internal/clicontract`, and a command follows them by calling that package.

Not every command follows every rule yet. The table at the end lists which
commands do today.

## The five rules

1. **`--json` prints one document.** With `--json`, stdout holds one JSON
   document and nothing else.
2. **stdout is data only.** Notes, progress and warnings go to stderr. With
   `--json`, every warning is also inside the document, in `warnings`.
3. **`--sessions-from <file|->` takes a list of sessions.** It reads session
   ids from a file, or from standard input when the value is `-`.
4. **`--dry-run` never prompts and never writes.** A command that has no safe
   dry run does not offer the flag.
5. **An empty result is a success with a reason.** It exits 0 and the document
   carries a typed reason. A failure exits non-zero.

## Exit statuses

| status | meaning |
|---|---|
| 0 | The command succeeded. An empty result is a success. |
| 1 | The command failed after it had started its work. |
| 2 | The command refused how it was called: flags that cannot be combined, a `--sessions-from` input it cannot read, or a confirmation it cannot ask for. Nothing was attempted. Fix the call; retrying it unchanged fails again. |
| 3 | `village push` failed before it made a single request to peasant online. Nothing was published and nothing was recorded as published. |

Status 2 is raised by the refusals that `internal/clicontract` owns. An unknown
flag or a flag with a missing value is reported by the flag parser and still
exits 1.

## Session ids in: `--sessions-from`

`--sessions-from` reads a file, or standard input when the value is `-`. It
accepts three shapes and picks one from the first character that is not a
space:

- **One id per line.** Blank lines and lines that start with `#` are skipped.
- **A JSON array of ids**, for example `["a1b2...", "c3d4..."]`.
- **A JSON array of objects.** Each object gives its `id` key, or else its
  `session_id` key. This is the array `peasant sessions list --json` prints, so
  its output can be piped straight in.

A repeated id is used once, at its first position. An input in any other shape
is refused with the line or array index of the fault. An input that names no id
at all is refused: a list that selects nothing never widens to every session.

`--sessions-from` replaces the other ways to choose sessions. It cannot be
combined with `--session` or `--all`, except on `village push`, where
`--session` adds to the list: the repeated `--session` ids come first, then the
list, each id once.

```sh
peasant sessions list --json --limit 20 | peasant export sessions --sessions-from - --output-dir out --json
```

## Warnings and empty results out

Every JSON document from a command that follows rule 2 carries these fields at
its top level:

| field | type | when |
|---|---|---|
| `warnings` | array | Always. It is `[]` when there are none, never `null`. |
| `empty_code` | string | Only when the result is empty. |
| `empty_reason` | string | Only when the result is empty. A sentence saying why and what to do next. |

Each warning is an object:

| field | meaning |
|---|---|
| `code` | One of the warning codes below. |
| `message` | What happened, in a sentence. |
| `session_id` | The session the warning is about, when it is about one. |

The same warning is printed on stderr as `warning: session <id>: <message>` or
`warning: <message>`.

Warning codes:

| code | meaning |
|---|---|
| `session_refused` | A session the command was asked for and did not process. |
| `index_stale` | A result read from an index that is behind its sources. |
| `index_maintenance_pending` | Index maintenance is due and has not run. |
| `filter_ignored` | A filter the command accepted and did not apply. |

Empty codes:

| code | meaning |
|---|---|
| `store_has_no_sessions` | The local store holds no session. |
| `filters_excluded_all` | Sessions exist, and the filters, selection or scope excluded every one. |
| `input_list_was_empty` | The input list named nothing to act on. |
| `nothing_changed` | There was nothing new to do. |
| `not_fetched_yet` | A local copy of state from peasant online has not been fetched yet. |

Both sets are closed. A new code is added to the set before any command prints
it.

## Prompts

A command that would ask for confirmation decides in this order:

1. With `--dry-run`, it does not ask, and it changes nothing.
2. With `--yes`, it proceeds without asking.
3. With `--json`, or with no terminal to answer, it refuses with exit status 2
   and a message naming `--yes`. Under `--json` a prompt would corrupt the
   document on stdout, so it is never shown.
4. Otherwise it asks.

Every command that has `--yes` also accepts `-y`.

`village push` has its own public-consent rule. A public push needs `--yes`
(or `--non-interactive`) or a `y` at its prompt. Under `--json` without `--yes`
a public push is refused before any request to peasant online, exits 3, and
names `--yes` in the document's `refusal` field.

## The `village push` plan

When `village push` is given `--session` or `--sessions-from`, its `--json`
document gains these keys beside the ones it always had:

| key | meaning |
|---|---|
| `dry_run` | Whether the run was a dry run. |
| `collectives` | The collectives `--collective` named, as `{id, name}`. |
| `visibility` | The visibility the run publishes at. |
| `needsConfirmation` | Whether the same run without `--dry-run` would ask before it publishes, because `--collective` skips the auto-publish rules and `--yes` is absent. |
| `plan` | One entry per requested session, in request order. |

Each plan entry has `session_id`, `verdict`, `selectedBy` (`request`, or
`parent` for a subagent session that travels with its main session) and, when
there is one, `reason`, `detail`, `title`, `next` and `shares`. A main
session's subagent sessions are listed in its `subagents`, the whole tree of
them, a subagent session's own subagent sessions included, each with
`selectedBy: parent`; with `--without-subagents` the entry counts the whole
tree in `subagentsOmitted` instead.

A subagent session pulled in with a named session follows that session's
saved-selection decision rather than its own branch: subagent sessions often
run on auto-named worktree branches, or after a branch switch, that the saved
selection never chose to exclude. The inheritance holds only while the named
session passes the selection and is ready, so the run sends it; the subagent
session's own readiness and every other filter still apply. Its entry carries
`selectionInheritedFrom: <named session id>` only where its own branch would
have been excluded. A subagent session named on its own keeps its own
selection check. Each
share names `collective_id`, `step` (`add_collective`) and `would` (`share`);
a real run adds `outcome` and, for a skipped or failed share, `reason`.

Verdicts: `will_publish`, `will_update`, `unchanged`, `held`, `excluded`,
`not_ready`, `unknown_session`, `failed`. Reasons: `not_in_store`,
`outside_repository`, `harness_mismatch`, `branch_selection_excluded`,
`needs_ingest`, `cohort_not_ready`, `latest_attempt_kept`,
`unchanged_content`, `transcript_is_public`, `no_metrics`, `no_model_reply`.
Both sets are closed. `needs_ingest` names the forced harvest that re-indexes
the session; `no_model_reply` is a session that never reached a model (it
holds no assistant reply, only setup or local commands), so its source names
no model, which peasant online requires, and no harvest can clear it.

With a plan, the counts are the tally of the plan and nothing else. Every
entry, each subagent session included, adds one to exactly one count, so the
counts add up to the number of entries:

| count | the entries with verdict |
|---|---|
| `new` | `will_publish` |
| `updated` | `will_update` |
| `skipped` | `unchanged` |
| `held` | `held` |
| `errors` | `failed`: the run tried the session and could not send it |
| `not_ready` | `not_ready`: the session is not ready to publish; its `reason` and `detail` say why |
| `excluded` | `excluded`: the run's own narrowing left the session out |
| `unknown_session` | `unknown_session` |

`not_ready`, `excluded` and `unknown_session` appear only with a plan. A push
that names no sessions keeps the counts it always printed, and there a session
that is not ready counts in `errors`. The text summary prints the same counts, and the push error log and the `--profile-output` outcome read them too: with a plan, a session that is not ready was not sent, which is not an error, so it alone writes no error log and does not fail the profile. The exit status reads the same counts: a run that reached peasant online exits 1 when `errors` is above zero (an upload error, an upload peasant online refused, or a failed share), and a session that is `not_ready`, `excluded` or `unchanged` never makes it exit 1. A push that names no sessions decides the same way from its `sessions`, leaving out those that were not ready.
The `sessions` list still holds only the sessions the run sent or tried to
send, so it can be shorter than the plan.

`village push` reports every refusal it makes before it sends peasant online
anything to write with exit status 3, a refusal of how it was called included:
an unreadable `--sessions-from` list, a session id the store does not hold, an
unknown collective (found by the one read, `GET /api/v1/collectives`, that
resolves the names), or a `--collective` confirmation it cannot ask for. Under
`--json` the refusal is also in the document's `refusal` field.

## Which commands follow the contract today

| command | follows |
|---|---|
| `export sessions` | `--sessions-from` (the earlier `--session-from-file` still works, unlisted), `--json` with `warnings`, and one stderr summary line for a run over more than one session, such as `2 of 3 sessions exported, 1 refused` |
| `redact` | `--sessions-from` |
| `village push` | `--json` with `warnings`, `empty_code` and `empty_reason`; `-y`; `--sessions-from`, with a per-session `plan` (below) |
| `upgrade` | `-y` |

`sessions list --json` prints a bare array, which has no place for `warnings`.
`export annotations`, `annotate prune` and `annotate sample` still take
`--session-from-file`, a path only.
