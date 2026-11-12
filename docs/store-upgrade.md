# Store upgrades

The local store has a schema version. A new build can need a newer schema than
the store on disk has. This page says what happens then.

## The rule

`store.Open` never changes the schema of a store that holds data.

- It creates a new store at the head schema.
- It opens a store that is already current.
- It refuses a store that a newer build upgraded.
- It refuses a store that is behind, with `store.SchemaBehindError`, and changes
  no byte of it.

Exactly one function applies migrations: `store.Upgrade`. `store.PlanUpgrade`
reads what an upgrade would do and changes no file. A source guard in
`internal/store` fails when the migration engine has a second caller.

## What each command does

Every entry point calls one helper, `ensureStoreCurrent`, before it opens the
store. The helper classes the entry point and applies this policy.

| entry point | behind, quick steps only | behind, a long step |
|---|---|---|
| a command at a terminal | names the steps and runs them | shows the plan, the store size and the free disk, and asks first |
| the same command with no terminal (a pipe, a script, CI) | runs, one line per step on standard error | refuses, changes nothing, exits 1 |
| a generated git hook (`village push --non-interactive`) | runs | skips with the nothing-attempted status; the hook script exits 0, so git is not blocked |
| `peasant web start` | the parent upgrades before it starts the server | the same, and asks first at a terminal |
| a server process (the web server, an import started from the web) | never upgrades; refuses a behind store | the same |
| `peasant migrate` | the explicit upgrade | the explicit upgrade; `--confirm` skips the prompt |

A server refuses to start on a behind store. Its message says what the store
needs and names `peasant migrate --schema-only`, to run in a terminal.

A terminal also asks first when free disk is tight: known, and below twice
what the plan asks for. Below what the plan asks for, the upgrade refuses
before any step runs, and the message names the bytes needed and the bytes
free. Unknown free disk gives a warning before a long step and the upgrade
continues.

## Free disk

Each step in the catalog carries a disk factor: the share of the database size
it asks to be free. The plan asks for the database size times the largest
factor among the steps planned for that store. Each step commits before the
next starts, so the log space of one step is reused by the next.

| factor | value | steps | evidence |
|---|---|---|---|
| entry table rebuild | 1.69 | 75 (and 11) | measured: log high-water 1.348 times the database, plus 0.340 growth |
| search rebuild | 0.39 | 76 on a store with stale search results (and 35) | derived from the search index build estimate; not measured |
| row-scaled | 0.01 | 70 to 74 and other row rewrites | measured: at most 0.0018 for one step, 0.0032 for five |
| quick | 0.001 | everything else | measured: the quick step wrote nothing |

The values and the measurement are in `internal/defaults/store_upgrade.go`.
Step 75 uses 1.69 for every store. A store that keeps its text in the body
table needs far less, but the plan cannot tell the two layouts apart without
reading the whole entry table, so it takes the conservative value.

A refusal names the command to run:

```
peasant migrate --schema-only
```

`peasant migrate --dry-run` prints the plan of a behind store and changes
nothing. `peasant migrate` without `--schema-only` runs the schema phase first
and then its conversion phases.

`peasant kickstart` upgrades first, then discovers sessions, then opens the
store to read. A declined upgrade ends the command with nothing changed.

## What an upgrade does

1. It takes the data directory's run lock, the one a harvest holds. A second
   process waits.
2. It checks free disk against the plan.
3. It runs each step in one transaction: the migration's script, any data fill
   the step needs, and the version stamp commit together.
4. After the last step it truncates the write-ahead log.

Ctrl-C stops the running step. That step rolls back. The steps before it stay
applied, and the next `peasant migrate --schema-only` resumes at the step that
was stopped.

## The step catalog

`internal/store/upgrade_catalog.go` gives every migration a title, a disk
factor and a cost class: `quick`, `scales_with_rows`, or `scales_with_text`. Every cost except
`quick` is a long step. A test fails when a migration has no entry or an entry has no disk factor, and
its message names the file and the fields to add.
`testdata/upgrade_step_catalog.manifest.yaml` lists the versions by name. A new
migration adds both.

A cost can depend on the store. The step that repairs the search index
triggers (schema 76) is long only on a store that carries stale search results
from 0.16.1; the plan reads the store to decide.

## Progress events

`store.UpgradeOptions.Progress` receives `store.UpgradeEvent` values. This type
is the contract between the upgrade and whatever shows it; commands print one
plain line per step today.

- Events arrive one at a time, never concurrently.
- For each step: one `step_started`, zero or more `step_progress`, then one
  `step_finished`. A step that fails or is cancelled sends no `step_finished`.
- `Index` is the 1-based position of the step and `Total` the number of steps.
- `Done`, `Of` and `Unit` count the step's work when the step can count
  (`Unit` is `sessions` or `bytes`); they are zero and empty otherwise.
- `WALBytes` is how much the write-ahead log grew during the step.
- `Elapsed` is the time since the step started.

A running step sends a `step_progress` event every two seconds with its elapsed
time and log growth, and one whenever its own count moves.

## Measuring

`TestSandboxStoreUpgradeMeasure` in `internal/store` builds a synthetic
schema-69 store of a chosen text size under `/tmp/opencode`, upgrades it, and
logs the duration and log growth of each step. It never runs in the gate. The
free-disk factors in `internal/defaults/store_upgrade.go` record the last run.
