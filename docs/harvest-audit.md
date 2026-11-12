# Harvest audit records

`ingest_log` records aggregate harvest totals. `ingest_run_outcomes` records
worker failures, diagnostic reasons and refresh reasons for each audit run.
`ingest_run_stages` records the run's profile: one row per pipeline stage.
`index_log` remains the independent index-attempt ledger: worker errors and
index errors are not interchangeable counts.

Detailed outcomes retain the latest 100 `ingest_log` runs, including successful
runs without diagnostics. Older aggregate rows remain unchanged. Details are
written best-effort after the aggregate row exists; an interrupted or failed
audit write can leave incomplete details. Atomicity is per 128-row batch, not
per run: a later batch failure preserves earlier committed batches. Failures
scale with discovered sessions and diagnostics can add multiple rows per
session, so the detail volume has no per-run bound. Bounded transactions keep
failed harvests from holding the SQLite writer for an unbounded run. Retention
is attempted even when a detail batch fails.

Neither an empty nor a populated detail set certifies completeness or proves
that a run succeeded. This ledger supports best-effort local triage, not an
authoritative completeness consumer; it has no completion marker. Consult the
aggregate totals independently. A run that is cancelled or times out before
AUDIT stores its run row, marked in `error_message`, and the stage rows it
measured; it stores no outcome rows. A killed process stores nothing.

Use read-only SQLite queries against the local database to inspect retained
details. For example, with a database path supplied in `$DB`:

```sh
sqlite3 -readonly "$DB" '
SELECT r.id, r.started_at, r.sessions_error,
       o.session_id, k.name AS kind, c.code AS reason, o.created_at
FROM ingest_log AS r
LEFT JOIN ingest_run_outcomes AS o ON o.run_id = r.id
LEFT JOIN ingest_outcome_reasons AS c ON c.id = o.reason_id
LEFT JOIN ingest_outcome_kinds AS k ON k.id = c.kind_id
WHERE r.id = (SELECT max(id) FROM ingest_log)
ORDER BY o.id;'
```

An outcome row holds a reason id. `ingest_outcome_reasons` gives each reason a
stable integer id, its code, and its kind; `ingest_outcome_kinds` names the
three kinds. The kind derives from the reason, and a code belongs to exactly
one kind. Reason ids below 1000000 are fixed by the `ingest.OutcomeReason`
constants and are the same in every store. Ids are never reused: a code that
goes out of use keeps its row with `retired = 1`. An id at or above 1000000 is
local to one store. The upgrade assigns one to a stored code no build lists,
and readers skip such rows.

`worker_error` names failed session processing. Untyped worker failures use
`error`; a native source that could not be acquired uses
`adapter_capture_failed`. `diagnostic` names a nonfatal warning, including
warnings raised during indexing. A diagnostic reason reuses the producer's
`ErrorType` spelling; `adapter_refresh_unavailable` is the diagnostic that a
refresh could not run and the previous artifact was kept. Unknown codes
(including arbitrary messages, paths, and whitespace variants) are rejected
without echoing their value in the error. Multiple warnings with the same
reason for a session are represented once per run. A NULL session identity
denotes a run-wide or unattributed diagnostic, never an inferred session.

`refresh` names why a session was refreshed. Every refreshed session has
exactly one refresh row per run, on every path, including a refresh that then
failed. An unchanged session has no row and stays a count in `ingest_log`. The
census of one run (kind 3 is `refresh`):

```sh
sqlite3 -readonly "$DB" '
SELECT c.code AS reason, count(*) AS sessions
FROM ingest_run_outcomes AS o
JOIN ingest_outcome_reasons AS c ON c.id = o.reason_id
JOIN ingest_outcome_kinds AS k ON k.id = c.kind_id
WHERE k.id = 3 AND o.run_id = (SELECT max(id) FROM ingest_log)
GROUP BY c.code ORDER BY sessions DESC;'
```

The pipeline names the reason from what it knows when it decides: `new`,
`forced`, `source_changed`, `metadata_schema`, `publication_unbound`,
`adapter_behind`, `adapter_stamp_absent`, `pair_missing` and `indexer_behind`.
A processed session with no more specific reason is recorded as
`source_changed`, and a stored index rewritten without a native pass as
`indexer_behind`. The remaining seeded reasons are not written yet.

## The run profile

Every import entry point (`peasant harvest`, the kickstart import, and the
import started from the local web app) keeps the profiler on.
`harvest --profile-index` only decides whether the profile is also printed.

`ingest_run_stages` holds one row per stage per run: `duration_ms`, `done`,
`total`, `bytes` (transcript bytes the stage moved; 0 when it reports none) and
`lane_wait_ms` (time the stage's store writes waited for the serial write
lane). Several segments of one stage are summed into its row. Stages overlap,
so the rows do not sum to the run's wall time; `ingest_log.started_at` and
`finished_at` give that. The run row, its stage rows and the stage retention
delete commit in one transaction. Stage rows are kept for the latest 100 runs.

`index_log` carries `parse_ms`, `write_ms` and `bytes` for each index attempt.
NULL means not measured: a row written before these columns existed, or an
attempt that did not parse. `index_log` has no key to `ingest_log`; join a
run's rows to its index attempts by session id inside the run's time window.

```sh
sqlite3 -readonly "$DB" '
SELECT stage, duration_ms, done, total, bytes, lane_wait_ms
FROM ingest_run_stages
WHERE run_id = (SELECT max(id) FROM ingest_log)
ORDER BY duration_ms DESC;'
```

The ledger deliberately stores no transcript content, raw error message,
diagnostic message, remediation text, or filesystem location: these free-text
values can contain private source data. Session identities and reason codes
support triage without retaining those values. The existing live CLI diagnostic
report is unchanged. Store callers can read details through
`ListIngestRunOutcomes(ctx, runID)` and the profile through
`ListIngestRunStages(ctx, runID)`.
