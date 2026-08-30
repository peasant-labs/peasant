# Peasant INDEX Optimization Handoff

This document lets a new team continue the INDEX optimization work from the
current state. It records what is done, what the last profile proved, what it did
not prove, and the exact commands used for copied-data profiling.

For the repeatable setup, command sequence, cleanup rules, and public-safe report
template, see [Harvest Benchmark Procedure](procedure.md).

## Current State

The historical streaming PR was:

- PR: <https://github.com/peasant-labs/peasant/pull/250>
- Branch: `peasant-239--perf--stream-index-drain`
- Base branch: `develop`
- Main issue: <https://github.com/peasant-labs/peasant/issues/239>

The current annotation performance stack is split across focused PRs:

- PR #262 adds durable annotation target anchors and safe unresolved/superseded
  repair state.
- PR #263 streams COMPUTE and ANNOTATE after indexed session batches.
- PR #264 combines annotation run-state, session hash, and compute-version lookup.
- PR #265 adds annotation profile detail and per-annotation timing attribution.

The current measured bottleneck is no longer INDEX row insertion on the warm
copied corpus. After the INDEX optimizations and annotation run-state skip, the
no-annotations profile shows ANNOTATE is dominated by annotation persistence
contention. Classifier compute is small compared with waiting for the single
SQLite annotation writer.

The PR replaces full-batch INDEX handoff with a bounded per-session work queue:

```text
drainLoop -> streamed INDEX work -> parser workers -> serial SQLite writer
```

The PR also keeps the staging arena safe with one completion token per drained
batch. `AckBatch` runs only after all parser workers that can read that drain
batch finish.

SQLite writes remain serial. The PR adds an in-pipeline mutex so metadata DB
inserts and INDEX entry writes do not race each other into transient SQLite
locks.

The current optimization work adds:

- a store-level batch INDEX writer with one outer transaction and one savepoint
  per session;
- pipeline flushing through that batch writer when the metrics store implements
  it;
- profile counters for `write txs` and savepoints;
- conservative annotation-target remapping by `entry_id`, `tool_call_id`, or
  unique content anchor;
- a V47 `sessions.session_entries_hash` column that stores a deterministic digest
  of the parsed `session_entries` projection after a successful INDEX write;
- hash-backed unchanged-session skip, with derived projection checks and
  annotation span-bound validation before any delete;
- stage-level profile timings and INDEX write cause counters, including hash
  decisions and annotation carry, remap, and rollback outcomes;
- metadata session upserts that preserve `sessions.session_entries_hash`, so the
  INDEX batch writer remains the only path that sets or clears the hash proof;
- `scripts/profile-index-copy.sh`, which automates the copied-corpus profile
  run described below.

## Safe Data Rules

Use the existing copied real-data corpus:

```text
/tmp/opencode/peasant-index-profile-live-source
```

This directory should already exist. It contains a copy of Peasant data with:

```text
peasant.db
peasant-sync/
village-pulls/
```

The copied DB is not empty. A read-only check showed:

- `102569` rows in `annotation_target_entries`;
- `7100` sessions with entry annotation targets;
- `3877750` rows in `session_entries` before the copied-data reindex.

Therefore, this profile path is a reindex of raw transcripts into an existing
copied DB, not an empty rebuild from raw transcripts. Annotation carry-over is
part of this specific copied-corpus workload because the DB already contains
entry annotations.

Rules:

- Do not read or mutate live data under `~/.local/share/peasant`.
- Do not copy live data again.
- Do not create full per-run copies under `/tmp/opencode`.
- Reuse `/tmp/opencode/peasant-index-profile-live-source` directly.
- It is acceptable to rewrite copied metadata `source.filePath` values in place
  when they still point at live source paths; this rewrite is idempotent and
  keeps reindexing on copied transcript files.
- Delete only tiny control directories named
  `/tmp/opencode/peasant-index-profile-control*`, never the copied corpus.

## Last Profile Result

### Local Batched/Remap Run

This run used the local uncommitted batch-writer and remap changes. It happened
before the no-copy harness correction, so it used a full disposable copy at
`/tmp/opencode/peasant-index-profile-run-batched-copyonly`. That copy was
deleted after the result was recorded.

- profile status: `1`
- command wall time: `20m12.9s`
- script wall seconds: `1215`
- `1` INDEX batch record
- `7897` sessions in the INDEX profile
- `2989152` entries in the INDEX profile
- `7976250453` bytes in the INDEX profile
- batch sizes: `7897x1`
- work items: `7897`
- INDEX queue capacity: `64`
- write transactions: `137`
- savepoints: `7692`
- parse time: `2m18.595s`
- write time: `11m27.222s`
- max parse workers: `32`
- successful indexed sessions: `7421`
- session import errors: `589`
- imported but not indexed warning count: `346`
- `database is locked` lines: `0`
- annotation span rollback warnings: `271`
- missing provider-root warnings: `74`

Interpretation:

- The batch writer is active: `write txs: 137; savepoints: 7692`.
- It did more successful INDEX writes than the earlier streaming run: `7421`
  versus `7116`.
- It is still not a clean speedup result. It used a per-run copy, and it did a
  different amount of completed DB work than the earlier profiles.
- Write time remains the bottleneck. Fewer transactions did not by itself reduce
  total write time on this copied workload.

### Local Prepared-Statement Reuse Run

This run used the no-copy control-dir harness after adding batch-scoped prepared
statement reuse for `session_entries`, `session_entries_ext`, and
`session_commands` inserts. It reused the copied corpus directly at
`/tmp/opencode/peasant-index-profile-live-source` and created only the small
control directory
`/tmp/opencode/peasant-index-profile-control-prepared-stmts`. The control
directory was deleted after this result was recorded.

Preparation:

- metadata paths rewritten in copied corpus: `16030`
- metadata paths already correct: `0`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- command wall time: `18m16.3s`
- script wall seconds: `1099`
- `1` INDEX batch record
- `7897` sessions in the INDEX profile
- `2975696` entries in the INDEX profile
- `7976250453` bytes in the INDEX profile
- batch sizes: `7897x1`
- work items: `7897`
- INDEX queue capacity: `64`
- write transactions: `132`
- savepoints: `7706`
- parse time: `2m2.232s`
- write time: `9m46.151s`
- max parse workers: `32`
- successful indexed sessions: `7149`
- session import errors: `589`
- imported but not indexed warning count: `632`
- `database is locked` lines: `0`
- annotation span rollback warnings: `557`
- missing provider-root warnings: `74`

Interpretation:

- The no-copy harness worked and did not create a full data copy.
- Prepared statement reuse reduced write time relative to the prior local batched
  run (`11m27.222s` to `9m46.151s`), but the runs are not perfectly equivalent:
  this one used the reusable copied corpus directly and had more annotation span
  rollbacks.
- Write time remains the bottleneck.

### Local Unchanged-Session Skip Run

This run used the no-copy control-dir harness after adding a safe unchanged-row
skip. The skip compares raw `session_entries` rows and the derived
`session_entries_ext` / `session_commands` projections before any delete. It
skips only when all projections match, so missing projection rows fall back to the
normal rewrite and are repaired.

It reused the copied corpus directly at
`/tmp/opencode/peasant-index-profile-live-source` and created only the small
control directory `/tmp/opencode/peasant-index-profile-control-skip-unchanged`.
The control directory was deleted after this result was recorded.

Preparation:

- metadata paths rewritten in copied corpus: `5132`
- metadata paths already correct: `12427`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- command wall time: `10m42.9s`
- script wall seconds: `646`
- `1` INDEX batch record
- `7932` sessions in the INDEX profile
- `3003194` entries in the INDEX profile
- `7983332826` bytes in the INDEX profile
- batch sizes: `7932x1`
- work items: `7932`
- INDEX queue capacity: `64`
- write transactions: `143`
- savepoints: `7761`
- skipped rewrites: `7086`
- parse time: `2m11.143s`
- write time: `1m50.145s`
- max parse workers: `32`
- successful indexed sessions: `7322`
- session import errors: `74`
- imported but not indexed warning count: `514`
- `database is locked` lines: `0`
- annotation span rollback warnings: `439`
- missing provider-root warnings: `74`

Interpretation:

- The no-copy harness worked and did not create a full data copy.
- Skipping unchanged entry rewrites is a material win on a warm copied corpus:
  compared with the prepared-statement run, write time dropped from `9m46.151s`
  to `1m50.145s`, and wall time dropped from `18m16.3s` to `10m42.9s`.
- This profile is a warm-corpus measurement. It proves the skip removes avoidable
  FTS churn when the parsed entries match the DB. It does not replace the earlier
  cold-ish prepared-statement measurement where most successful sessions still
  rewrote rows.

### Local Guarded Unchanged-Session Skip Run

This run used the no-copy control-dir harness after tightening the skip so it
also checks existing entry-annotation target spans before returning success. A
session with an invalid existing annotation span must still take the old fail-safe
rewrite path and roll back with an actionable error instead of being marked as
successfully indexed.

It reused the copied corpus directly at
`/tmp/opencode/peasant-index-profile-live-source` and created only the small
control directory `/tmp/opencode/peasant-index-profile-control-skip-guard`. The
control directory was deleted after this result was recorded.

Preparation:

- metadata paths rewritten in copied corpus: `5167`
- metadata paths already correct: `12587`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- command wall time: `11m9s`
- script wall seconds: `672`
- `1` INDEX batch record
- `7897` sessions in the INDEX profile
- `2999336` entries in the INDEX profile
- `7976250453` bytes in the INDEX profile
- batch sizes: `7897x1`
- work items: `7897`
- INDEX queue capacity: `64`
- write transactions: `143`
- savepoints: `7702`
- skipped rewrites: `7101`
- parse time: `2m19.683s`
- write time: `1m39s`
- max parse workers: `32`
- successful indexed sessions: `7268`
- session import errors: `109`
- imported but not indexed warning count: `509`
- `database is locked` lines: `0`
- annotation span rollback warnings: `434`
- missing provider-root warnings: `74`

Interpretation:

- The annotation-span guard did not erase the performance win.
- Compared with the prepared-statement run, write time dropped from `9m46.151s`
  to `1m39s`, and wall time dropped from `18m16.3s` to `11m9s`.
- The run remains a warm-corpus measurement because the no-copy harness mutates
  the copied DB in place.

### Local Session Entries Hash Runs

These runs used the no-copy control-dir harness after adding V47
`sessions.session_entries_hash`. The digest is computed from the parsed
`schema.SessionEntry` projection after parse. A matching stored digest lets the
writer skip the expensive raw `session_entries` row comparison, while still
checking the derived projections and entry-annotation target span bounds before
it returns success.

All runs reused the copied corpus directly at
`/tmp/opencode/peasant-index-profile-live-source`. The control directories were
small per-run directories only.

#### Hash Seed Run

The first run populated hashes for sessions that already matched the parsed
projection. It used control directory
`/tmp/opencode/peasant-index-profile-control-hash-seed`.

Preparation:

- metadata paths rewritten in copied corpus: `5132`
- metadata paths already correct: `12616`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- command wall time: `9m57.9s`
- script wall seconds: `601`
- `1` INDEX batch record
- `7932` sessions in the INDEX profile
- `2998390` entries in the INDEX profile
- `7983332826` bytes in the INDEX profile
- batch sizes: `7932x1`
- work items: `7932`
- INDEX queue capacity: `64`
- write transactions: `144`
- savepoints: `7729`
- skipped rewrites: `6979`
- parse time: `2m7.018s`
- write time: `1m51.733s`
- max parse workers: `32`
- successful indexed sessions: `7165`
- session import errors: `74`
- imported but not indexed warning count: `639`
- `database is locked` lines: `0`
- annotation span rollback warnings: `564`
- missing provider-root warnings: `74`

#### First Hash Fast-Path Run

The second run measured a copied corpus where many sessions had stored hashes.
It used control directory `/tmp/opencode/peasant-index-profile-control-hash-fast`.
At this point the hash path still used the full annotation anchor comparison, so
it did not isolate the intended fast path.

Preparation:

- metadata paths rewritten in copied corpus: `5167`
- metadata paths already correct: `12618`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- command wall time: `11m32.4s`
- script wall seconds: `693`
- `1` INDEX batch record
- `7897` sessions in the INDEX profile
- `2982610` entries in the INDEX profile
- `7976250453` bytes in the INDEX profile
- batch sizes: `7897x1`
- work items: `7897`
- INDEX queue capacity: `64`
- write transactions: `139`
- savepoints: `7710`
- skipped rewrites: `6708`
- parse time: `2m6.299s`
- write time: `3m26.833s`
- max parse workers: `32`
- successful indexed sessions: `6929`
- session import errors: `109`
- imported but not indexed warning count: `856`
- `database is locked` lines: `0`
- annotation span rollback warnings: `781`
- missing provider-root warnings: `74`

#### Hash Span-Bounds Run

The third run followed a store-only adjustment: when the stored hash matches,
annotation validation checks only that the target span still names entries in the
parsed projection. It does not read old entry anchors because the hash match is
the proof that stored entry rows already match the parsed projection. It used
control directory `/tmp/opencode/peasant-index-profile-control-hash-fast-spans`.

Preparation:

- metadata paths rewritten in copied corpus: `5132`
- metadata paths already correct: `12618`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- command wall time: `10m29.3s`
- script wall seconds: `632`
- `1` INDEX batch record
- `7932` sessions in the INDEX profile
- `2986104` entries in the INDEX profile
- `7983332826` bytes in the INDEX profile
- batch sizes: `7932x1`
- work items: `7932`
- INDEX queue capacity: `64`
- write transactions: `137`
- savepoints: `7715`
- skipped rewrites: `6613`
- parse time: `2m1.871s`
- write time: `2m59.756s`
- max parse workers: `32`
- successful indexed sessions: `6860`
- session import errors: `74`
- imported but not indexed warning count: `930`
- `database is locked` lines: `0`
- annotation span rollback warnings: `855`
- missing provider-root warnings: `74`

#### Hash-Preserve Stage-Counter Run

This run followed the fix that changed `InsertSessions` from
`INSERT OR REPLACE` to `INSERT ... ON CONFLICT DO UPDATE`. The previous metadata
upsert deleted and recreated the session row before INDEX, which cleared
`sessions.session_entries_hash` and forced the expensive fallback row comparison.

It used control directory
`/tmp/opencode/peasant-index-profile-control-hash-preserve`.

Preparation:

- metadata paths rewritten in copied corpus: `5132`
- metadata paths already correct: `12618`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- command wall time: `9m31.2s`
- script wall seconds: `573`
- `1` INDEX batch record
- `7932` sessions in the INDEX profile
- `3006675` entries in the INDEX profile
- `7983332826` bytes in the INDEX profile
- batch sizes: `7932x1`
- work items: `7932`
- INDEX queue capacity: `64`
- write transactions: `146`
- savepoints: `7765`
- skipped rewrites: `6993`
- hash matches: `6368`
- hash misses: `273`
- fallback compares: `1124`
- skipped by hash: `6368`
- skipped by compare: `625`
- rewrites: `124`
- projection repair rewrites: `0`
- annotation rollback failures: `648`
- annotation targets carried: `4238`
- annotation targets remapped: `16`
- parse time: `2m5.877s`
- write time: `1m17.804s`
- max parse workers: `32`
- `DISCOVER`: `4.581s`
- `DB INSERT`: `1m17.696s`
- `INDEX`: `1m18.014s`
- `INDEX LOG`: `2.118s`
- `COMPUTE`: `1m8.55s`
- `ANNOTATE`: `6m57.928s`
- successful indexed sessions: `7117`
- session import errors: `74`
- imported but not indexed warning count: `723`
- `database is locked` lines: `0`
- annotation span rollback warnings: `648`
- missing provider-root warnings: `74`

#### Annotation ID-Cache Run

This run followed the small cache that resolves seeded classifier annotator IDs
and annotation type IDs once per classifier type, then reuses them across
parallel `Annotate` calls. It used control directory
`/tmp/opencode/peasant-index-profile-control-id-cache`.

Preparation:

- metadata paths rewritten in copied corpus: `5167`
- metadata paths already correct: `12618`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- command wall time: `9m49.4s`
- script wall seconds: `595`
- `1` INDEX batch record
- `7897` sessions in the INDEX profile
- `3007531` entries in the INDEX profile
- `7976250453` bytes in the INDEX profile
- batch sizes: `7897x1`
- work items: `7897`
- INDEX queue capacity: `64`
- write transactions: `144`
- savepoints: `7790`
- skipped rewrites: `7099`
- hash matches: `7072`
- hash misses: `213`
- fallback compares: `505`
- skipped by hash: `7072`
- skipped by compare: `27`
- rewrites: `45`
- projection repair rewrites: `0`
- annotation rollback failures: `646`
- annotation targets carried: `4174`
- annotation targets remapped: `2`
- parse time: `2m10.48s`
- write time: `1m11.777s`
- max parse workers: `32`
- `DISCOVER`: `4.773s`
- `DB INSERT`: `1m11.538s`
- `INDEX`: `1m11.905s`
- `INDEX LOG`: `2.032s`
- `COMPUTE`: `1m11.605s`
- `ANNOTATE`: `7m18.231s`
- successful indexed sessions: `7144`
- session import errors: `109`
- imported but not indexed warning count: `721`
- `database is locked` lines: `0`
- annotation span rollback warnings: `646`
- missing provider-root warnings: `74`

Interpretation:

- V47 stores the parsed projection proof and keeps the skip path independent of
  `CurrentIndexVersion`; if parser output changes but the version is not bumped,
  the digest changes and the session rewrites.
- The batch path stores `session_entries_hash` atomically with `index_version`.
  The legacy one-by-one `UpdateIndexState` clears it because that path cannot
  prove the hash and index state were stored together.
- The hash path remains safe around derived rows: deleting a
  `session_entries_ext` row still forces a rewrite and repairs the projection.
- The latest no-copy profile is not a clean speedup proof over the guarded skip
  run. The dirty copied corpus produced many annotation rollback warnings, and
  the exact indexed session set varied across runs. Report these runs as warm,
  dirty-corpus measurements.
- The hash-preserve run proves the intended hash fast path is active:
  `6368` sessions skipped by hash, with only `1124` fallback compares.
- After this fix, entry writes are no longer the main measured cost on the warm
  copied corpus. `ANNOTATE` is the largest stage at `6m57.928s`, compared with
  `INDEX` at `1m18.014s` and `COMPUTE` at `1m8.55s`.
- The annotation ID cache is not a proven material speedup. On the next warm
  dirty-corpus run, `ANNOTATE` measured `7m18.231s`; this is close enough to the
  prior `6m57.928s` that the static ID lookups should be treated as cleanup, not
  the main performance lever. The next optimization must measure or reduce the
  repeated per-session/per-result annotation reads and writes.

#### Annotation State, Batched Persistence, and Entry-Index Fix Run

This run followed the annotation-run-state skip table, batched classifier
annotation persistence, serialized annotation writes, and the fix that makes
entry classifiers target the stored `session_entries.entry_index` instead of the
classifier result slice offset. It reused the copied corpus directly at
`/tmp/opencode/peasant-index-profile-live-source` and created only the small
control directory
`/tmp/opencode/peasant-index-profile-control-annotation-entry-index-fix`. The
control directory was deleted after this result was recorded.

Preparation:

- metadata paths rewritten in copied corpus: `5132`
- metadata paths already correct: `12618`
- missing transcript files: `0`
- invalid metadata files: `0`

Result:

- profile status: `1`
- script wall seconds: `156`
- CLI wall time: `2m34.5s`
- `1` INDEX batch record
- `7932` sessions in the INDEX profile
- `3017990` entries in the INDEX profile
- `7983332826` bytes in the INDEX profile
- batch sizes: `7932x1`
- work items: `7932`
- INDEX queue capacity: `64`
- write transactions: `159`
- savepoints: `7811`
- skipped rewrites: `7084`
- hash matches: `7084`
- hash misses: `249`
- fallback compares: `478`
- skipped by hash: `7084`
- skipped by compare: `0`
- rewrites: `34`
- projection repair rewrites: `0`
- annotation rollback failures: `693`
- annotation targets carried: `4369`
- annotation targets remapped: `0`
- parse time: `2m12.838s`
- write time: `1m8.675s`
- max parse workers: `32`
- `DISCOVER`: `4.738s`
- `DB INSERT`: `1m8.585s`
- `INDEX`: `1m8.927s`
- `INDEX LOG`: `1.915s`
- `COMPUTE`: `1m13.064s`
- `ANNOTATE`: `5.032s`
- successful indexed sessions: `7118`
- session import errors: `74`
- imported but not indexed warning count: `768`
- `database is locked` lines: `0`
- annotation target carry failures: `693`
- missing provider-root warnings: `74`
- `persist batch result` errors: `0`
- entry-index foreign-key errors: `0`

Annotation detail:

- list entries: `58ms` total; count `34`
- get metrics: `5.328s` total; count `7118`
- classifier run: `21ms` total; count `34`
- results: total `123`, session-target `93`, entry-target `30`,
  skipped-by-state `7084`
- ID cache: hits `119`, misses `4`
- batch persistence: `1m12.386s` total; batches `34`; results `123`; errors `0`
- dedup lookup, create session annotation, create entry annotation, update content
  hash, and supersede annotation profiled as `0s` because they now run inside the
  batch path for this workload.
- dedup decisions: skip `67`, create `29`, supersede `27`

Interpretation:

- The entry-index fix removed the previous foreign-key failures from this warm
  copied-corpus profile. No `FOREIGN KEY` lines, `persist batch result` warnings,
  or `database is locked` lines appeared in the log.
- The annotation-run-state skip is the material warm-path win. `ANNOTATE` fell
  from the prior steady measurement of `6.907s` to `5.032s`, and `7084` of
  `7118` annotated sessions skipped the classifier work by state.
- The remaining measured `ANNOTATE` time is almost entirely the per-session
  metrics read used to check `compute_version`: `GetMetrics` ran `7118` times and
  took `5.328s`, which is slightly larger than the aggregate `ANNOTATE` stage
  timer because stage timings overlap with the profiler detail accounting.
- The run still exits with status `1` because the copied corpus contains known
  import/index warning classes. Treat those as dirty-corpus evidence, not as a
  new branch failure.

#### Combined Annotation-State Lookup Recommendation

Do not add the remaining skip-state optimization in this PR. The profile now
shows a bounded overhead of about `5.3s` on a `2m34.5s` warm copied-corpus run,
and the branch already carries migrations, batch writing, stateful skip logic,
and several correctness fixes. Keeping the PR focused reduces merge and review
risk for users while still landing the large lock-removal and warm-path wins.

The minimal follow-up design is one store method on the optional annotation-state
capability that reads all state needed before full annotation through one
connection and one query:

```go
type AnnotationRunInputs struct {
    SessionID          ingest.SessionID
    SessionEntriesHash string
    ComputeVersion     int
    State              *ingest.AnnotationRunState
}

type AnnotationRunStateStore interface {
    GetAnnotationRunInputs(ctx context.Context, sessionID SessionID) (*AnnotationRunInputs, error)
    SaveAnnotationRunState(ctx context.Context, state AnnotationRunState) error
}
```

The SQL should select `sessions.session_entries_hash`,
`session_metrics.compute_version`, and the matching `annotation_run_state` row
with a left join, then `ClassifierAnnotator` can decide whether to skip without
calling `GetMetrics`, `GetCurrentSessionEntriesHash`, and
`GetAnnotationRunState` separately. Only the non-skip path needs the full metrics
payload for classifier execution. That follow-up should keep the current optional
interface fallback so tests and alternate stores that only implement the older
methods still run the safe full-annotation path.

#### No-Annotations Annotation Persistence Profile

This run used the reusable no-annotations copied corpus after adding batch
persistence sub-timers, redacted value output, and per-annotation timing
attribution. It measured annotation creation from a copied store whose annotation
rows had been removed before the run.

The no-annotations corpus was reused directly at
`/tmp/opencode/peasant-index-profile-no-annotations-source`. The profile logs
were moved out of the control directory after the run. The corpus was scrubbed
afterwards and verified clean: `annotations=0`, `annotation_run_state=0`, empty
annotation target tables, and SQLite integrity check `ok`.

Preparation before the run:

- `annotations`: `0`
- `annotation_run_state`: `0`
- SQLite integrity check: `ok`

Result:

- profile status: `1`
- status `1` is expected for this copied corpus because it reports known
  dirty-corpus warnings
- script wall seconds: `472`
- CLI wall time: `7m49.3s`
- `1` INDEX batch record
- `7897` sessions in the INDEX profile
- `3017012` entries in the INDEX profile
- `7976250453` bytes in the INDEX profile
- batch sizes: `7897x1`
- work items: `7897`
- INDEX queue capacity: `64`
- write transactions: `153`
- savepoints: `7798`
- skipped rewrites: `6826`
- parse time: `2m8.449s`
- write time: `1m15.889s`
- max parse workers: `32`
- `DISCOVER`: `5.256s`
- `DB INSERT`: `1m16.229s`
- `INDEX`: `1m16.294s`
- `INDEX LOG`: `2.04s`
- `COMPUTE`: `1m9.629s`
- `ANNOTATE`: `5m15.233s`
- successful indexed sessions: `7798`
- session import errors: `109`
- imported but not indexed warning count: `75`
- `database is locked` lines: `0`
- annotation target carry failures: `0`
- missing provider-root warnings: `74`

Annotation detail:

- list entries: `32.349s` total; count `7798`
- get metrics: `14.25s` total; count `7798`
- classifier run: `14.996s` total; count `7798`
- results: total `100531`, session-target `21526`, entry-target `79005`,
  skipped-by-state `0`
- ID cache: hits `100526`, misses `5`
- batch persistence: `1h24m54.399s` total; batches `7798`; results `100531`;
  errors `0`
- mutex wait: `1h19m51.913s` total; count `7798`
- connection checkout: `27ms` total; count `7798`
- savepoint SQL: `484ms` total; count `201062`
- dedup lookup: `4m31.161s` total; count `100531`
- insert annotation row: `5.9s` total; count `100531`
- insert target row: `2.605s` total; count `100531`
- update content hash: `2.483s` total; count `100531`
- supersede annotation: `0s` total; count `0`
- commit: `19.148s` total; count `7798`
- dedup decisions: skip `0`, create `100531`, supersede `0`

Top attributed annotation groups:

- `quality.session_outcome=resolved`: `7151` results, `2m49.327s` attributed,
  `139ms` classifier, `2m49.188s` persistence, `2m47.182s` dedup lookup
- `quality.user_frustration=not_detected`: `7643` results, `59.789s`
  attributed, `4.84s` classifier, `54.949s` persistence, `53.75s` dedup lookup
- `quality.session_outcome=abandoned`: `647` results, `13.878s` attributed,
  `2ms` classifier, `13.876s` persistence, `13.695s` dedup lookup
- `quality.resolution_evidence=present`: `78600` results, `12.188s`
  attributed, `1.969s` classifier, `10.22s` persistence, `3.089s` dedup lookup
- largest `metadata.session_scope` value group: `635` results, `3.677s`
  attributed, `283ms` classifier, `3.394s` persistence, `3.305s` dedup lookup

Interpretation:

- The no-annotations run proves the current ANNOTATE hot path is write
  contention, not classifier compute.
- Sessions are already processed by a worker pool in the ANNOTATE stage, but each
  session still calls the annotation batch writer separately.
- The store serializes those calls with `annotationWriteMu`, so the parallel
  session workers spend most aggregate time waiting for the single SQLite writer.
- Parallelizing classifier groups inside one session is therefore a secondary
  optimization. It can reduce about `15s` of aggregate classifier time, but it
  does not address the `1h19m51.913s` aggregate mutex wait.
- The next optimization should add a stage-level buffer: annotation workers
  prepare per-session writes in parallel, and one writer flushes larger batches
  across many sessions in fewer transactions.

### Prior Streaming PR Run

The last copied-data profile happened before the local batch-writer and remap
changes above. It used a disposable copy per branch. Each disposable copy
rewrote only its own metadata `source.filePath` fields so reindex read the copied
transcript files instead of absolute live source paths.

The result is useful but not a wall-time win proof. It is also not evidence that
the branch made INDEX slower. The branch completed many more successful writes
than the baseline, so the write-time and wall-time numbers do not measure the
same amount of completed DB work.

Develop baseline:

- `24` INDEX batches
- `7897` sessions in the INDEX profile
- `3018427` entries in the INDEX profile
- parse time: `3m15.33s`
- write time: `5m21.72s`
- command wall time: `9m18.4s`
- successful indexed sessions: `3006`
- `database is locked` lines: `4773`
- annotation span rollback warnings: `30`
- missing provider-root warnings: `74`
- command exit: `1`, because `589` sessions failed in the copied corpus

Streaming branch:

- `1` INDEX batch record
- `7897` streamed work items
- INDEX queue capacity: `64`
- `2966032` entries in the INDEX profile
- parse time: `1m37.607s`
- write time: `10m3.299s`
- command wall time: `18m26.4s`
- successful indexed sessions: `7116`
- `database is locked` lines: `0`
- annotation span rollback warnings: `381`
- missing provider-root warnings: `74`
- command exit: `1`, because `589` sessions failed in the copied corpus

Interpretation:

- The streaming queue ran. The `work items` and `queue capacity` line proves this.
- Parse time improved because parser work starts from streamed work items.
- The SQLite lock storm went away.
- Total wall time cannot be compared directly in this run. The branch did
  thousands more successful DB writes than develop. Develop was partly shorter
  because many write attempts failed early on SQLite locks.
- The copied corpus has known dirty-data behavior. Do not hide it. Report it
  separately from branch-caused failures.

## Profile Commands

The canonical procedure is now in [Harvest Benchmark Procedure](procedure.md).
The commands below are kept as historical detail for the original copied-corpus
harness work.

Run commands from the Peasant worktree for the branch you want to measure. Use
the same copied corpus path for every branch. Do not create a per-run copy of the
corpus.

Preferred command after the local harness change:

```bash
scripts/profile-index-copy.sh --work /tmp/opencode/peasant-index-profile-control-streaming
```

Use one small control directory per branch, for example:

```bash
scripts/profile-index-copy.sh --work /tmp/opencode/peasant-index-profile-control-develop
scripts/profile-index-copy.sh --work /tmp/opencode/peasant-index-profile-control-streaming
```

The script keeps the control directory and `profile.log` by default. Add
`--clean` only when the evidence has already been copied out or is no longer
needed. `--clean` deletes only the control directory, not the copied corpus.

The control directory contains a symlink because the CLI's `--data-dir` flag is
an XDG data-home override. Peasant resolves its DB at `<data-dir>/peasant`, while
the copied corpus is already the final Peasant data directory. The symlink adapts
that shape without copying the corpus.

Manual equivalent:

Set the reusable source-copy path:

```bash
CORPUS="/tmp/opencode/peasant-index-profile-live-source"
WORK="/tmp/opencode/peasant-index-profile-control-streaming"
```

Prepare one small control directory. This creates no data copy. The metadata
rewrite is idempotent and touches only the copied corpus, so transcript paths
point at the copied transcript files instead of live source paths.

```bash
rm -rf "$WORK"
mkdir -p "$WORK/data-home" "$WORK/config-home/peasant" "$WORK/state-home"
ln -s "$CORPUS" "$WORK/data-home/peasant"
CORPUS="$CORPUS" WORK="$WORK" node <<'NODE'
const fs = require('fs');
const path = require('path');

const corpus = process.env.CORPUS;
const work = process.env.WORK;
const syncRoot = path.join(corpus, 'peasant-sync');
const configPath = path.join(work, 'config-home/peasant/config.yaml');
let rewritten = 0;
let alreadyCorrect = 0;
let missingTranscript = 0;
let invalid = 0;

function walk(dir) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(p);
      continue;
    }
    if (!entry.isFile() || !entry.name.endsWith('--metadata.json')) {
      continue;
    }

    let doc;
    try {
      doc = JSON.parse(fs.readFileSync(p, 'utf8'));
    } catch (_) {
      invalid++;
      continue;
    }

    const sessionId = doc.sessionId || entry.name.slice(0, -'--metadata.json'.length);
    const format = doc.source && doc.source.format;
    if (!sessionId || !format) {
      invalid++;
      continue;
    }

    const transcript = path.join(path.dirname(p), `${sessionId}--transcript.${format}`);
    if (!fs.existsSync(transcript)) {
      missingTranscript++;
      continue;
    }

    if (doc.source.filePath === transcript) {
      alreadyCorrect++;
      continue;
    }
    doc.source.filePath = transcript;
    fs.writeFileSync(p, JSON.stringify(doc) + '\n');
    rewritten++;
  }
}

walk(syncRoot);
fs.writeFileSync(configPath, `version: 1
sources:
  claude-code: {enabled: false}
  opencode: {enabled: false}
  codex: {enabled: false}
  cursor: {enabled: false}
  strike: {enabled: false}
output:
  basePath: ${syncRoot}
`);

console.log(JSON.stringify({ syncRoot, configPath, rewritten, alreadyCorrect, missingTranscript, invalid }));
if (missingTranscript || invalid) {
  process.exit(1);
}
NODE
```

Run the profile against the reusable copied corpus:

```bash
{ time go run ./cmd/peasant \
  --data-dir "$WORK/data-home" \
  --config-dir "$WORK/config-home" \
  --state-dir "$WORK/state-home" \
  harvest index --all --profile-index; \
} > "$WORK/profile.log" 2>&1
STATUS=$?
```

The copied corpus can produce `STATUS=1`. Do not discard the run only because the
status is non-zero. The profile lines are still useful when the target session
set and warning classes are recorded.

Extract the main profile lines:

```bash
rg "^(INDEX profile|  batch sizes|  work items|  write txs|  write causes:|  annotation detail:|    (hash matches|hash misses|fallback compares|skipped by hash|skipped by compare|rewrites|projection repair rewrites|annotation rollback failures|annotation targets carried|annotation targets remapped|note:|list entries:|get metrics:|classifier run:|results:|id cache:|batch persistence:|dedup lookup:|create session annotation:|create entry annotation:|update content hash:|supersede annotation:|dedup decisions:|[A-Z][A-Z ]+:)|  parse|  stage timings:|peasant harvest|  index:|  warning:)" "$WORK/profile.log"
```

Count known warning classes:

```bash
rg -c "database is locked" "$WORK/profile.log" || true
rg -c "preserve annotation_target_entries" "$WORK/profile.log" || true
rg -c "harness stores entries under a provider root" "$WORK/profile.log" || true
```

After extracting evidence, delete only the control directory:

```bash
rm -rf "$WORK"
```

For a fair compare, repeat the same prepare-and-run sequence for `develop` and
for the candidate branch. Use different `WORK` values, for example:

```bash
WORK="/tmp/opencode/peasant-index-profile-control-develop"
WORK="/tmp/opencode/peasant-index-profile-control-streaming"
```

## Why Writes Are Still The Bottleneck

`IndexSessionEntries` writes one session per transaction. It batches rows inside
one session, but it does not batch multiple sessions into one transaction.

For each session, the current write path does this work:

- take a DB connection;
- start a transaction;
- read existing entry annotation targets;
- delete annotation targets, commands, extension rows, and entries;
- insert `session_entries` in chunks of `32` rows;
- insert `session_entries_ext` rows and `session_commands` rows;
- let FTS triggers update `session_entries_fts` for every deleted and inserted
  entry;
- restore entry annotation targets;
- commit;
- call `UpdateIndexState` as a separate write.

The likely bottleneck is DB write amplification: transaction cost, FTS trigger
cost, index maintenance, deletes, and per-session state updates. Savepoints are
expected to be cheaper than thousands of separate commits, but this must be
measured.

Annotation carry-over is not the expected source of a speedup for a clean raw
transcript import. It appears in these profiles because the copied corpus also
contains a copied `peasant.db`, and the reindex path preserves existing
`annotation_target_entries` rows from that DB while it replaces entries from raw
transcripts. If a profile starts from an empty DB, entry annotation carry-over
should not be on the hot path.

## Next Optimization 1: Batch INDEX Writes Across Sessions

### Problem

`IndexSessionEntries` writes one session per transaction. This preserves
per-session failure isolation, but it creates high write overhead on large
stores.

### Plan

Add a serial batched INDEX writer that receives parsed sessions and writes many
sessions inside one outer SQLite transaction. Preserve per-session failure
isolation with a savepoint around each session.

Proposed store API shape:

```go
type SessionEntryWrite struct {
    SessionID ingest.SessionID
    Entries   []schema.SessionEntry
    IndexedAt int64
}

type SessionEntryWriteResult struct {
    SessionID ingest.SessionID
    Written   bool
    Err       error
}

type SessionEntryBatchWriter interface {
    IndexSessionEntryBatch(ctx context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult
}
```

Implementation notes:

- keep one serial writer goroutine;
- accumulate parsed sessions up to a bounded count or byte budget;
- start one outer transaction for the batch;
- use one savepoint per session;
- on session failure, roll back to that session's savepoint and continue;
- move `UpdateIndexState` into the same per-session savepoint;
- keep per-session index log entries;
- keep the existing single-session API as a wrapper over a batch of one, unless
  there is a clear reason to keep a separate path.

### Validation

- Given one bad session in a batch, when the batch writer runs, then only that
  session rolls back and later sessions still write.
- Given annotation carry-over fails for one session, when the batch writer runs,
  then the old entries and old targets for that session remain intact.
- Given all sessions are valid, when the batch writer runs, then it produces the
  same rows as repeated single-session writes.
- Given `Parallelism=1`, when INDEX runs, then per-session result order remains
  deterministic.
- Given copied real data, when profiling runs, then write time and transaction
  count are recorded separately from parse time.

## Next Optimization 2: Remap Entry Annotations During Reindex

### Problem

Entry annotations currently target `(session_id, entry_index, end_index)`. During
reindex, the store tries to carry those targets across the delete-and-reinsert
of `session_entries`.

If the old target span no longer exists at the same entry indexes, the current
safe behavior is to roll back that session's reindex. This avoids orphaned or
wrong annotations, but it also means stale entry indexes can block a useful
reindex.

This is primarily a correctness and recovery issue. Do not treat annotation
remapping as a performance optimization unless a profile proves that annotation
target carry-over is a meaningful part of runtime for the workload under test.

### Plan

Introduce a durable annotation anchor that can survive entry reordering or entry
count changes. Then remap entry annotations to the new `entry_index` when the
same target can be identified.

Candidate anchors, in priority order:

- stable provider entry identity, when `session_entries.entry_id` is present;
- stable tool call identity, when the target is a tool-use or tool-result row;
- content fingerprint over normalized role, type, text, and local context;
- existing numeric index only as a fallback.

The migration should extend annotation target storage rather than change the
meaning of current rows in place. Current `(session_id, entry_index, end_index)`
targets remain valid, but new or reindexed targets gain enough identity data to
be remapped later.

Remap policy:

- if exactly one new entry matches the durable anchor, move the target to that
  new index;
- if a multi-entry span can be matched contiguously, move the whole span;
- if no match exists, keep the old annotation attached to the old index state and
  leave the session reindex rolled back, or mark the annotation unresolved after
  a product decision;
- if more than one match exists, do not guess.

User-authored annotations should be treated more conservatively than derived
classifier annotations. Classifier annotations can often be superseded or
recomputed. User annotations may need an explicit unresolved state and a repair
surface.

### Validation

- Given a target entry moves from index 10 to index 12 with the same stable
  anchor, when reindex runs, then the annotation target moves to index 12.
- Given an annotation target has no stable match, when reindex runs, then no
  wrong target is written.
- Given two new entries match the same weak fingerprint, when reindex runs, then
  the remap is refused as ambiguous.
- Given a classifier annotation becomes stale, when annotations are recomputed,
  then the old classifier annotation is superseded or removed through the normal
  annotation lifecycle.
- Given a user annotation becomes stale, when reindex runs, then the user's data
  is preserved and the required repair action is visible.

## Next Optimization 3: Repeatable Copied-Data Performance Harness

### Problem

The copied-data profile is useful, but it is manual and easy to make
non-comparable. The same copied data can produce different evidence if one run
reads original live source paths, uses a different temp-root layout, or reports
dirty-corpus warnings without separating them from branch-caused failures.

Status: the repeatable procedure is now documented in
[Harvest Benchmark Procedure](procedure.md). Keep this section for the original
requirements and use the procedure as the operational source of truth.

### Plan

Create a script or checked-in developer command that automates the commands in
this document. It must use the existing copied real-data corpus at
`/tmp/opencode/peasant-index-profile-live-source` and one fresh per-run copy for
each branch under test. It must not create a synthetic clean corpus.

The harness should record:

- session count;
- entry count;
- bytes read;
- parse duration;
- write duration;
- total wall time;
- number of SQLite transactions;
- number of savepoints;
- skipped, failed, rolled-back, and successfully indexed sessions;
- INDEX queue size, work-item count, and parser-worker high-water mark;
- known warning counts, including DB locks, annotation remap failures, and
  missing provider roots.

### Validation

- Given the same copied real-data corpus, when baseline and candidate runs
  execute, then they attempt the same target session set.
- Given a profile run has skipped or failed sessions, when results are printed,
  then the summary separates profile-gate failures from expected dirty-corpus
  warnings.
- Given a candidate claims faster wall time, when the profile report is read,
  then the report also shows equal or higher successful indexed-session count.

## Next Optimization 4: Buffer Annotation Writes Across Sessions

### Problem

The ANNOTATE stage already processes sessions concurrently, but each worker calls
the annotation batch writer for one session at a time. The store serializes those
calls with one annotation write mutex, so parallel workers queue behind the same
SQLite write lane.

The no-annotations profile shows this shape clearly:

- classifier run: `14.996s` aggregate;
- batch persistence: `1h24m54.399s` aggregate;
- mutex wait: `1h19m51.913s` aggregate;
- `7798` annotation batches for `100531` results.

Parallel classifier groups can reduce classifier CPU time, but it does not remove
the observed write contention. The next user-visible win is fewer, larger
annotation write transactions.

### Plan

Add a stage-level annotation buffer between classification and SQLite writes:

```text
ANNOTATE workers
  -> prepare per-session annotation writes in parallel
  -> buffered annotation channel
  -> one serial writer flushes many sessions per transaction
  -> per-session results update progress and run state
```

Public API shape should stay minimal and testable:

```go
type SessionAnnotationWrite struct {
    Write          ClassifierAnnotationWrite
    TypeID         string
    Value          string
    TargetKind     AnnotationProfileTargetKind
    ClassifierTime time.Duration
}

type SessionAnnotationBatch struct {
    SessionID SessionID
    Writes    []SessionAnnotationWrite
    RunState  *AnnotationRunState
}

type SessionAnnotationBatchResult struct {
    SessionID SessionID
    Results   []ClassifierAnnotationWriteResult
    Err       error
}

type BufferedSessionClassifier interface {
    PrepareAnnotations(ctx context.Context, sessionID SessionID, profiler *IndexProfiler) (SessionAnnotationBatch, error)
    FlushAnnotationBatches(ctx context.Context, batches []SessionAnnotationBatch, profiler *IndexProfiler) []SessionAnnotationBatchResult
}
```

The final names can be adjusted during implementation, but the design boundary is
fixed: classification remains parallel and SQLite writes are flushed by one
bounded serial path.

Implementation notes:

- keep the existing `SessionClassifier` and `ProfiledSessionClassifier` fallback;
- have `stageAnnotate` use the buffered path only when the classifier implements
  the optional buffered interface;
- bound the buffer by batch count and result count so memory stays predictable;
- preserve best-effort per-session behavior: one bad session must not stop later
  batches;
- save `annotation_run_state` only after that session's annotation writes
  succeed;
- batch run-state saves with the annotation write transaction, or flush them in
  the same serial writer so they do not reintroduce one write transaction per
  session;
- keep profile counters for prepared sessions, flushed batches, write results,
  mutex wait, commit time, and per-annotation attributed timing;
- keep annotation values redacted in profile output.

### Validation

- Given many sessions produce annotations, when the buffered path runs, then the
  store sees fewer annotation write transactions than one transaction per
  session.
- Given one prepared session has an invalid write, when the writer flushes a
  multi-session batch, then only that session reports an error and later sessions
  still write.
- Given a session's annotation writes fail, when the writer completes, then its
  `annotation_run_state` is not saved.
- Given a session's annotation writes succeed, when the writer completes, then
  its `annotation_run_state` is saved.
- Given the classifier does not implement the buffered interface, when ANNOTATE
  runs, then the existing one-session `Annotate` behavior remains active.
- Given the no-annotations copied corpus, when profiling runs after this change,
  then ANNOTATE should show fewer batches, lower mutex wait, and unchanged
  annotation result counts.

## Recommended Order

The current order is:

1. Keep the INDEX write optimizations: streaming work, batch entry writes,
   hash-backed unchanged-row skip, and safe annotation target handling.
2. Keep the annotation-run-state skip and combined state/hash/version lookup.
   They remove avoidable warm-path annotation work.
3. Keep the annotation profile detail and per-annotation timing attribution.
   They now identify the ANNOTATE bottleneck with enough precision to choose the
   next write-shape experiment.
4. Use [Harvest Benchmark Procedure](procedure.md) for future evidence. Do not
   rely on ad hoc command transcripts.
5. Implement the buffered annotation writer across sessions. The goal is fewer
   annotation write transactions and lower aggregate mutex wait, while preserving
   per-session best-effort results and run-state correctness.
6. Treat parallel classifier groups as a secondary optimization. It can reduce
   classifier CPU time, but the current no-annotations profile shows classifier
   run time is not the dominant cost.
7. Do better reindex handling as a separate correctness track. This is
   independent of the speed wins. Durable anchors and unresolved repair state
   need their own migration, tests, push behavior, and user-facing repair
   semantics.
