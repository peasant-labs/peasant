---
name: peasant-harvest-audit
description: How to query the local peasant database for past harvest runs and the ingest/index audit log. Use whenever the user asks about harvest results, "what did the last harvest do", ingest or worker errors, why a session failed, was skipped, or was not re-indexed, run summaries, audit logs, per-session outcomes, diagnostics, reason codes, index history, or session origins — anything that needs `ingest_log`, `index_log`, `ingest_run_outcomes`, or the peasant SQLite store.
---

# Peasant audit queries

The local store keeps a per-run and per-session audit. Read it with SQL; never modify it.

## Safety — read this first

- **Always open the database read-only**: `sqlite3 -readonly "$DB" "..."`. Never open it for writing, never VACUUM (that is an operator maintenance action), and never copy it to work around a lock — the app being open is normal and read-only queries work alongside it.
- The database can be large (tens of GiB). Always add `LIMIT` and select only the columns you need.
- Timestamps are Unix **milliseconds**. Format with `datetime(col/1000,'unixepoch')`; add `, 'localtime'` when the user wants their own clock.

## Locate the database

```bash
DB="${XDG_DATA_HOME:-$HOME/.local/share}/peasant/peasant.db"
```

## Tables

- **`ingest_log`** — one row per harvest run. Columns: `id, started_at, finished_at, sessions_new, sessions_updated, sessions_unchanged, sessions_error, indexed_count, computed_count, error_message, source_path`. `id` is the run id.
- **`ingest_run_outcomes`** — per-session outcomes for a run (the detailed audit): `id, run_id, session_id, reason_id, created_at`. `reason_id` points at `ingest_outcome_reasons` (`id, kind_id, code, retired`), whose `kind_id` points at `ingest_outcome_kinds` (`worker_error`, `diagnostic`, `refresh`). Every refreshed session has one `refresh` row per run. Detail is kept for the latest 100 runs; older detail is pruned while the `ingest_log` row remains.
- **`ingest_run_stages`** — the run's profile, one row per stage: `run_id, stage, duration_ms, done, total, bytes, lane_wait_ms`. Kept for the latest 100 runs. Stages overlap, so the rows do not sum to wall time.
- **`index_log`** — index-stage outcomes per session: `id, session_id, provider, outcome, index_version, entries_count, source_path, original_root, reason, started_at, finished_at, error_message, index_format_version, parse_ms, write_ms, bytes`. The last three are NULL when the attempt was not measured.
- **`sessions`** — one row per session: `session_id, parent_id, model_harness, session_origin, origin_version, adapter_version, index_version, index_format_version, source_path, ingested_ms, indexed_at, active_generation_id, artifact_hash, root_session_id, …` (check with `PRAGMA table_info(sessions)`).
- **`session_content_captures`** — content capture status per session: `session_id, status, source_authority, transcript_origin, capture_format, entry_count, content_row_count, full_capture_sha256, captured_at_ms, failure_code, failure_message, publication_capture_revision`.

## Ready queries

Recent runs, newest first:

```bash
sqlite3 -readonly "$DB" "
SELECT id,
       datetime(started_at/1000,'unixepoch') AS started,
       round((finished_at-started_at)/1000.0,1) AS seconds,
       sessions_new, sessions_updated, sessions_unchanged, sessions_error,
       indexed_count, computed_count
FROM ingest_log ORDER BY id DESC LIMIT 10;"
```

One run's full summary row:

```bash
sqlite3 -readonly "$DB" "SELECT * FROM ingest_log WHERE id = <RUN>;"
```

One run's per-session outcomes, grouped by reason:

```bash
sqlite3 -readonly "$DB" "
SELECT k.name AS kind, r.code AS reason, COUNT(*) AS n
FROM ingest_run_outcomes o
JOIN ingest_outcome_reasons r ON r.id = o.reason_id
JOIN ingest_outcome_kinds k ON k.id = r.kind_id
WHERE o.run_id = <RUN>
GROUP BY 1,2 ORDER BY n DESC;"
```

Where one run's time went, by stage:

```bash
sqlite3 -readonly "$DB" "
SELECT stage, duration_ms, done, total, bytes, lane_wait_ms
FROM ingest_run_stages WHERE run_id = <RUN> ORDER BY duration_ms DESC;"
```

The sessions behind a run's worker errors (harness and origin add context):

```bash
sqlite3 -readonly "$DB" "
SELECT o.session_id, r.code AS reason, s.model_harness, s.session_origin,
       datetime(s.ingested_ms/1000,'unixepoch') AS ingested
FROM ingest_run_outcomes o
JOIN ingest_outcome_reasons r ON r.id = o.reason_id
JOIN sessions s ON s.session_id = o.session_id
WHERE o.run_id = <RUN> AND r.kind_id = 1 ORDER BY o.id;"
```

One session's index history:

```bash
sqlite3 -readonly "$DB" "
SELECT datetime(started_at/1000,'unixepoch') AS at, provider, outcome,
       index_version, reason, error_message
FROM index_log WHERE session_id = '<SESSION>' ORDER BY id DESC LIMIT 20;"
```

Stuck origin verdicts (a parent link exists but the origin stayed `unknown`):

```bash
sqlite3 -readonly "$DB" "
SELECT model_harness, COUNT(*) FROM sessions
WHERE session_origin='unknown' AND parent_id IS NOT NULL GROUP BY 1;"
```

## Reading the results

- A completed run with `sessions_error > 0` is still a successful run: per-session failures are recorded, not fatal. Check what they were before calling a run "clean".
- `worker_error` reasons are pipeline failures (`error`, `adapter_capture_failed`). `refresh` reasons say why a session was refreshed (`new`, `source_changed`, `adapter_behind`, `indexer_behind`, `forced`, …). `diagnostic` reasons come from the adapters (`unknown_data_retained`, `missing_model`, `parse_error`, `index_refused`, `content_recovery_unavailable`, `pair_repair_unavailable`, `oversize_write_advisory`, …). The reason set is closed and validated on write — a code you do not recognize means a newer build wrote it.
- These tables hold identities, kinds, and reason codes only — never transcript text. Do not try to read content here; use the session APIs for that.

## Triage recipe

1. `SELECT id … FROM ingest_log ORDER BY id DESC LIMIT 5` — which run, how long, how many errors.
2. Group that run's `ingest_run_outcomes` by kind and reason code — the shape of the failure.
3. Join the `worker_error` rows to `sessions` — the failing harnesses, origins, and whether their sources are old (stale refresh) or new.
4. For one session, `index_log` shows its index-stage history; the `sessions` row shows origin/adapter/index state.
5. When a session keeps failing every run with the same reason, say so explicitly — a recurring deferral is different from a one-off error.
