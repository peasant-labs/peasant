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
- It refuses a store at the head schema that still owes a recount (see
  [Recounts](#recounts)) the same way: such a store is behind.

Exactly one function applies migrations and runs recounts: `store.Upgrade`. `store.PlanUpgrade`
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
| a server process (the web server, an import started from the web, the import the web setup starts when it is saved) | never upgrades; refuses a behind store before it claims the import | the same |
| `peasant migrate` | the explicit upgrade | the explicit upgrade; `--confirm` skips the prompt |

Hook mode belongs to `village push` alone: that command declares
`--non-interactive` as the flag generated hooks pass. Another command with a
flag of that name is a terminal or a piped command like any other.

A server refuses to start on a behind store. Its message says what the store
needs and names `peasant migrate --schema-only`, to run in a terminal. An
import a running server starts asks `store.RefuseBehind` first, a read-only
check that gives the refusal `store.Open` gives: the web setup's import is
refused before it is claimed, and every web import is refused again where it
would open the store.

A terminal also asks first when free disk is tight: known, and below twice
what the plan asks for. Every mode that runs the upgrade prints the plan
first, so a pipe and a hook also print the line `free disk is tight: less
than twice what the upgrade asks for`. Below what the plan asks for, the upgrade refuses
before any step runs, and the message names the bytes needed and the bytes
free. Unknown free disk gives a warning before a long step and the upgrade
continues.

## Free disk

Each step has a disk factor and a basis: what the factor multiplies. The
basis is the whole database, or the tables the step rewrites, measured on
the store. The cost class gives both: a quick step, a row-scaled step and a
text-scaled step that copies no table each multiply the whole database by
their factor. Only the steps that copy a table name their own factor and
basis in the catalog: the two that copy the entry table (11 and 75), and the
two that copy the session tables (73 and 78). The search trigger repair (76)
takes the need of the cost the plan resolves for the store.

A step's need is its factor times its basis. The plan asks for the largest
need among the steps planned for that store. Each step commits before the
next starts, so the log space of one step is reused by the next.

| factor | value | steps | basis | evidence |
|---|---|---|---|---|
| entry table rebuild | 2.0 | 75 (override) | the entry table with its indexes, and the two search indexes the step rebuilds, on a store whose text is in the bodies; the whole database on a store whose text is in the entry rows | measured: the step used 1.760 times the entry table and search indexes with the text in the entry rows, 1.020 times with the text in the bodies, and 1.848 times at the row bound; 2.0 leaves about 8% to spare |
| entry table rebuild | 2.0 | 11 (override) | the entry table with its indexes; a store that old keeps its text in the entry rows, so the whole database | the same factor |
| session tables rebuild | 2.0 | 73 and 78 (override) | `sessions` and `daily_summary_harness` with their indexes, every row read from its record header (at most a million rows each; past that, the whole database) | measured: the step's log grew 1.02 to 1.03 times the estimated tables, 1.40 to 1.45 times a database those tables fill; the plan asked for 1.38 to 1.43 times what the log used |
| search rebuild | 0.39 | every `scales_with_text` step that copies no table: 35, and 76 on a store with stale search results | the database | derived from the search index build estimate; not measured |
| row-scaled | 0.01 | every `scales_with_rows` step but 11, 73 and 78 | the database | measured on 70 to 74: at most 0.0018 for one step, 0.0032 for five |
| quick | 0.001 | every `quick` step | the database | measured: the quick step wrote nothing |

The values and the measurements are in `internal/defaults/store_upgrade.go`.

Step 75 copies `session_entries` into a new table with `image` in its entry
type set, drops the old table, re-creates its five indexes, and drops and
rebuilds two search indexes: `session_entries_fts` over the entry rows and
`session_search_fts` over the entry rows and the entry bodies. It does not copy
`session_entry_bodies`; it reads that table's text to index it. So on a store
whose text has moved into the bodies, step 75 writes about one search index and
a small entry table, not a copy of the store.

### How the plan measures a basis

The plan only reads, and what it reads is bounded. A basis it measures can
never come in low: it reads every row it counts, and where it cannot, it
takes the whole database.

- **Where the text is.** The plan first reads one row of
  `session_entry_bodies`. That table holds rows only once a session's text
  has moved into the bodies (index format 2). With no bodies, the entry table
  holds the store's text and dominates the database, so the basis is the
  whole database and nothing more is read.
- **The entry table, when the text is in the bodies.** The entry table then
  holds the rows of sessions that were never converted. Their size has no
  bound (tool output there has no length limit), so the plan reads every
  row, at most 65,536, each measured with `octet_length`, which SQLite
  answers from the record header without loading the value or its overflow
  pages. A row's size follows SQLite's page layout: its share of a leaf page,
  its whole overflow pages, and a fixed 384 bytes for the record header and
  its index entries. A table with more rows stops the read and takes the
  whole database.
- **Each search index.** FTS5 keeps two records current in the index's own
  `_data` table: the structure record lists every segment's leaf pages, and
  the averages record starts with the document count. Leaves times their
  pages, plus 64 bytes per leaf for its share of the index's interior pages
  and its `_idx` row, plus one small row per document, is the index's size,
  read from two rows.

The estimate errs high: each row's leaf share assumes rows of its own size,
its record header is counted at three bytes per column, and the search index
estimate came out about one percent above `dbstat`. It does
not count deleted rows or the free space they leave, because step 75 copies
the live rows into a fresh table. A basis never exceeds the database size, so
the plan never asks for more than the factor times the whole database. A
store whose tables cannot be measured (a search index record that does not
decode) gets the factor times the whole database and one warning line saying
why.

What was considered, in order:

1. **A stored layout fact.** `sessions.index_format_version` records, per
   session, whether its text moved into the bodies; a row in
   `session_entry_bodies` says that at least one did, in one read. The layout
   says where the text mass is, which decides whether reading the entry
   table is worth it. It does not bound an entry row: a session that was
   never converted keeps its text in its entry rows in either layout. So the
   layout cannot make a sample safe.
2. **Sampling.** Bounded, but a few rows of hundreds of megabytes can fall
   between the sampled rows, and then the estimate comes in low. Dropped.
3. **`dbstat`.** The bundled SQLite has it, and the tests use it as the
   ground truth. It walks every page of a table, overflow pages included:
   259 ms for the entry table and search indexes of a 522 MB store, so tens
   of seconds for a 40 GB entry table. Not bounded.
4. **Reading every row's header, up to a bound.** Chosen. The cost is one
   record header per row, never the text, and the bound caps it.

### Measured

Two synthetic schema-74 stores, about 256 MiB of entry text each, and a
store whose text is in the bodies and whose entry table holds 65,536 rows of
about a page each, the read bound (2026-11-12,
`TestSandboxStoreUpgradeDiskBasisMeasure`):

| | text in the entry rows | text in the bodies | bodies, at the bound |
|---|---|---|---|
| database | 522 MB | 758 MB | 988 MB |
| entry table and search indexes (`dbstat`) | 519 MB | 166 MB | 981 MB |
| basis the plan uses | 522 MB, the whole database | 168 MB, read in full | 998 MB read, capped at 988 MB |
| estimate time, entry rows read | 20 µs, none | 4.8 ms, 2,620 | 74 ms, 65,536 |
| `dbstat` time over the same objects | 227 ms | 96 ms | 377 ms |
| step 75: log high-water plus database growth | 914 MB | 170 MB | 1,812 MB |
| the plan asked for | 967 MB (1.06 times what was used) | 311 MB (1.83 times) | 1,828 MB (1.01 times) |

Every basis the plan measured was at or above `dbstat`'s. The test fixtures
add entry rows with a heavy tail (every hundredth row a hundred times the
mean), rows rising in size with their rowid, and rows at the size where
SQLite moves a payload to an overflow page, in both layouts, and assert the
same.

The incident store (43 GB, a 39 MiB entry table, 16.4 GiB of text in the
bodies) left a log of 4.31 GB in step 75. The old rule asked it for 1.69 times
43 GB; this one asks for 2.0 times its entry table and search indexes.

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

## Recounts

A migration whose data change is Go work, not SQL, records that work as owed:
it writes a marker row, by name, in `pending_recounts`. The recount is an
upgrade step of its own, of kind `recount`, that runs right after the migration
that wrote the marker. It reads the marker under the write lock, does its
work, and deletes the marker in the same transaction. A recount that fails, is
cancelled or is killed leaves the marker, and the next upgrade runs it again.
The schema version cannot say that a recount is owed: it moves as soon as the
migration commits. So the plan reads the markers, and a store that holds one
is behind at any schema version, the head included. `Open` never runs a
recount; it refuses the store with `SchemaBehindError`, whose message names
the steps an earlier upgrade did not finish, and every entry point's policy
applies as to any behind store.

Migration 77 writes the `turn_counts` marker when the store holds sessions.
Its recount folds every stored session's entries once and writes both turn
counts: `turn_count` takes the folded turns and `conversation_turn_count`
the conversation turns. A session with no stored entries, or whose entries
cannot be read, keeps both stored values and is logged. The recount walks the
sessions with the same code as the step 74 fill. 0.17.0 ran this recount when
it opened the store; a 0.17.0 store whose open was killed part way is at
schema 77 with the marker, and its plan is the recount, then 78 and 79.

A 0.16.2 store (schema 76) with sessions plans four steps: 77 (quick), the
turn count recount (scales with rows, counted in sessions), 78 (scales with
rows: it copies the session tables to widen their harness CHECK) and 79
(quick). A 0.17.0 store whose recount finished plans 78 and 79.

Step 78 is long on every store, whatever its size: it copies the `sessions`
and `daily_summary_harness` tables to widen their harness CHECK, and its time
and log grow with those rows. So every store below 78, a 0.17.0 store
included, has a long step in its plan: a terminal asks first, a script or CI
run refuses, and a git hook skips publishing (exit 0, so git is not blocked)
until `peasant migrate --schema-only` runs in a terminal.

## The step catalog

`internal/store/upgrade_catalog.go` gives every migration a title and a cost
class: `quick`, `scales_with_rows`, or `scales_with_text`, in version order.
Every cost except `quick` is a long step. The cost class gives the step's
disk factor and basis; a step that copies a table overrides them with
`disk:`. A test fails when a migration has no entry, when a cost class gives
no disk need, or when an override restates the need its cost already gives,
and its message names the file and the fields to add or remove.
`testdata/upgrade_step_catalog.manifest.yaml` lists the versions by name. A new
migration adds both. A recount has its entry in `upgradeRecountCatalog`,
keyed by its marker name, with the migration that marks it and a query that
says whether that migration will write the marker on a store; its run is
registered in `upgradeRecountRuns` in `internal/store/upgrade.go`. A test
fails when either has a name the other lacks.

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

The measurements sit behind the `upgrade_measure` build tag, so no gate
builds or runs them; they prove nothing in CI. Run one with
`go test -tags upgrade_measure ./internal/store -run <name> -v -timeout 0`
and `PEASANT_SANDBOX_UPGRADE_DIR` set to a directory under `/tmp/opencode`.

`TestSandboxStoreUpgradeMeasure` in `internal/store` builds a synthetic
schema-69 store of a chosen text size under `/tmp/opencode`, upgrades it, and
logs the duration and log growth of each step.
`TestSandboxStoreUpgradeDiskBasisMeasure` builds one schema-74 store per entry
text layout, and one whose entry table holds as many rows as the plan reads,
compares the plan's basis estimate and its time with `dbstat`, and
logs what step 75 used: the log's high-water mark plus the database's growth.
`TestSandboxTurnCountRecountMeasure` builds schema-76 stores of a chosen
number of sessions with a chosen number of entries each, half of them managed
generations, upgrades them, and logs the turn count recount's time and log
growth per session.
`TestSandboxHarnessWideningMeasure` builds schema-77 stores of a chosen number
of sessions rows, upgrades them, and logs step 78's time and log growth beside
the plan's estimate of the session tables.
None of them runs in the gate. The free-disk factors in
`internal/defaults/store_upgrade.go` record the last runs.

The last recount run (2026-11-12, load average about 18):

| sessions x entries | database | recount | per session | log written |
|---|---|---|---|---|
| 2,000 x 40 | 71 MB | 0.45 s | 0.23 ms | 161 kB, 0.0023 of the database |
| 5,000 x 40 | 176 MB | 0.99 s | 0.20 ms | 363 kB, 0.0021 of the database |
| 5,000 x 120 | 515 MB | 2.10 s | 0.42 ms | 363 kB, 0.0007 of the database |

The time grows with the sessions and with their entries, so the recount is a
row-scaled step that counts sessions, like step 74. Its log stays near 75
bytes per session, well inside the row-scaled disk factor (0.01).

The last run of step 78 (2026-11-12, load average about 8 to 17), synthetic
sessions rows shaped like stored ones and one daily summary row per day:

| sessions rows | database | step 78 | per row | log written | session tables estimate |
|---|---|---|---|---|---|
| 20,000 | 17.7 MB | 0.41 to 0.51 s | 20 to 25 µs | 24.8 MB, 1.40 of the database | 24.1 MB in 41 ms |
| 50,000 | 42.8 MB | 1.09 to 1.21 s | 22 to 24 µs | 61.8 MB, 1.45 of the database | 60.6 MB in 102 ms |

The time grows with the session rows and is not sub-second at 50,000 rows,
so step 78 is a row-scaled step on every store, like step 73, which copied
the same tables and now asks for the same disk.
Its log is about 1,240 bytes per row, about the size of the copied tables,
so it asks for twice those tables rather than the row-scaled share of the
database.
