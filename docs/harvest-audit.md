# Harvest audit records

`ingest_log` records aggregate harvest totals. `ingest_run_outcomes` records
worker failures and diagnostic reason codes for each completed audit run.
`index_log` remains the independent index-attempt ledger: worker errors and
index errors are not interchangeable counts.

Detailed outcomes retain the latest 100 `ingest_log` runs, including successful
runs without diagnostics. Older aggregate rows remain unchanged. Details are
written best-effort after the aggregate row exists; an interrupted or failed
audit write can leave incomplete details. An empty detail set alone is not
proof that a run succeeded. Runs interrupted before AUDIT have no completed
audit record.

Use read-only SQLite queries against the local database to inspect retained
details. For example, with a database path supplied in `$DB`:

```sh
sqlite3 -readonly "$DB" '
SELECT r.id, r.started_at, r.sessions_error,
       o.session_id, o.kind, o.reason_code, o.created_at
FROM ingest_log AS r
LEFT JOIN ingest_run_outcomes AS o ON o.run_id = r.id
WHERE r.id = (SELECT max(id) FROM ingest_log)
ORDER BY o.id;'
```

`worker_error` names failed session processing; `diagnostic` names a nonfatal
warning, including warnings raised during indexing. Diagnostic reason codes
reuse the producer's `ErrorType`. Untyped worker failures use the existing
`error` outcome reason; native acquisition failures use
`adapter_refresh_unavailable`. Multiple warnings with the same reason for a
session are represented once per run. A NULL session identity denotes a
run-wide or unattributed diagnostic, never an inferred session.

The ledger deliberately stores no transcript content, raw error message,
diagnostic message, remediation text, or filesystem location: these free-text
values can contain private source data. Session identities and reason codes
support triage without retaining those values. The existing live CLI diagnostic
report is unchanged. Store callers can read details through
`ListIngestRunOutcomes(ctx, runID)`.
