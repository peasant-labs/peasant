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

Each step in the catalog carries a disk factor and a basis: what the factor
multiplies. The basis is the whole database, or the tables the step rewrites,
measured on the store. A step's need is its factor times its basis. The plan
asks for the largest need among the steps planned for that store. Each step
commits before the next starts, so the log space of one step is reused by the
next.

| factor | value | steps | basis | evidence |
|---|---|---|---|---|
| entry table rebuild | 1.85 | 75 | the entry table with its indexes, and the two search indexes the step rebuilds | measured: the step used 1.760 times the basis with the text in the entry rows, 1.020 times with the text in the bodies |
| entry table rebuild | 1.85 | 11 | the entry table with its indexes (no search index exists yet) | the same factor |
| search rebuild | 0.39 | 76 on a store with stale search results (and 35) | the database | derived from the search index build estimate; not measured |
| row-scaled | 0.01 | 70 to 74 and other row rewrites | the database | measured: at most 0.0018 for one step, 0.0032 for five |
| quick | 0.001 | everything else | the database | measured: the quick step wrote nothing |

The values and the measurements are in `internal/defaults/store_upgrade.go`.

Step 75 copies `session_entries` into a new table with `image` in its entry
type set, drops the old table, re-creates its five indexes, and drops and
rebuilds two search indexes: `session_entries_fts` over the entry rows and
`session_search_fts` over the entry rows and the entry bodies. It does not copy
`session_entry_bodies`; it reads that table's text to index it. So on a store
whose text has moved into the bodies, step 75 writes about one search index and
a small entry table, not a copy of the store.

### How the plan measures a basis

The plan only reads, and its cost does not grow with the store:

- **The entry table.** It reads the smallest and the largest rowid, then at
  most 2,048 rows at evenly spaced rowids. Each row is measured with
  `octet_length`, which SQLite answers from the record header without loading
  the value or its overflow pages. A row's size follows SQLite's page layout:
  its share of a leaf page, its whole overflow pages, and a fixed 384 bytes for
  the record header and its index entries. The rowid span times the mean
  sampled row, times a margin of 1.25, is the estimate. A table with no more
  than 2,048 rowids is read in full.
- **Each search index.** FTS5 keeps two records current in the index's own
  `_data` table: the structure record lists every segment's leaf pages, and the
  averages record starts with the document count. Leaves times their pages,
  plus one small row per document, is the index's size, read from two rows.

The estimate errs high: the rowid span counts deleted rows, the margin covers a
sample that misses the largest entries, and the search index estimate came out
a few percent above `dbstat`. A basis never exceeds the database size, so the
plan never asks for more than the factor times the whole database. A store
whose tables cannot be measured (a search index record that does not decode)
gets the factor times the whole database and one warning line saying why.

What was considered, in order:

1. **A stored conversion fact.** `sessions.index_format_version` records, per
   session, whether its entries were converted into the bodies, and
   `session_search_state` records whether the search index was consolidated.
   They say which layout a store has, not how large its tables are, so the plan
   does not use them.
2. **`dbstat`.** The bundled SQLite has it, and the tests use it as the ground
   truth. It walks every page of a table, overflow pages included: 259 ms for
   the entry table and search indexes of a 522 MB store, so tens of seconds for
   a 40 GB entry table. Not bounded.
3. **Sampling.** Bounded and read-only; chosen for the entry table. The search
   indexes are not sampled: FTS5's own records give their size in two rows.

### Measured

Two synthetic schema-74 stores, about 256 MiB of entry text each (2026-11-12,
`TestSandboxStoreUpgradeDiskBasisMeasure`):

| | text in the entry rows | text in the bodies |
|---|---|---|
| database | 522 MB | 758 MB |
| entry table and search indexes (`dbstat`) | 519 MB | 166 MB |
| estimated basis | 624 MB, capped at 522 MB | 166 MB |
| estimate time, rows read | 7.3 ms, 2,050 | 12.0 ms, 2,050 |
| step 75: log high-water plus database growth | 914 MB | 170 MB |
| the plan asked for | 967 MB (1.06 times what was used) | 307 MB (1.81 times) |

The incident store (43 GB, a 39 MiB entry table, 16.4 GiB of text in the
bodies) left a log of 4.31 GB in step 75. The old rule asked it for 1.69 times
43 GB; this one asks for 1.85 times its entry table and search indexes.

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
factor, a disk basis and a cost class: `quick`, `scales_with_rows`, or `scales_with_text`. Every cost except
`quick` is a long step. A test fails when a migration has no entry or an entry has no disk factor or no disk basis, and
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
logs the duration and log growth of each step.
`TestSandboxStoreUpgradeDiskBasisMeasure` builds one schema-74 store per entry
text layout, compares the plan's basis estimate and its time with `dbstat`, and
logs what step 75 used: the log's high-water mark plus the database's growth.
Neither runs in the gate. The free-disk factors in
`internal/defaults/store_upgrade.go` record the last runs.
