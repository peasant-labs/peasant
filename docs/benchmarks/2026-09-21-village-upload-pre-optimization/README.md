# Village Upload Pre-Optimization Benchmarking

**Date:** 2026-09-21 (PDT)  
**Host:** Grok Bot box (not the user’s Mac)  
**Scope:** Peasant → Village **upload dry-run** profiling before optimization work  
**Policy:** Synthetic / e2e fixtures only. No live Village publish. No reads of real `~/.claude`, `~/.codex`, `~/.cursor`, or `~/.grok`.

---

## Important: no network upload today

**No actual network upload to Village was performed at any point on 2026-09-21.**

Every benchmark run (the small fixture profile and all seven scaled sets, including any later `007-2gb` retry) used **`village push --dry-run` only**. Fake credentials pointed at `http://127.0.0.1:9`. **Zero HTTP requests** were made to a real Village backend. Stage timings cover local harvest / payload / redaction / dry-run stubs only — not publish, negotiate, receipt, or remote storage.

---

## Purpose

Establish a pre-optimization baseline for the Peasant Village upload pipeline:

1. Document what was measured and how.
2. Capture seven per-dataset sub-agent reports under a consistent template.
3. Consolidate cross-scale findings (where time goes, where memory breaks).

---

## Process we took

### 1. Orient on the product path

- Mapped the peasant-labs polyrepo and wrote foundational / design notes (`FOUNDATIONAL-KNOWLEDGE.md`, `VILLAGE-UPLOAD-DESIGN.md`).
- Confirmed ingestion is **coding-agent session files on disk**, not Grok TUI sessions.
- Live ingest adapters: `claude-code`, `opencode`, `codex`, `cursor`, `pi`, `strike` (strike off by default). Grok is **not** a harness.

### 2. Small fixture profile (control)

- Built Peasant on the box (`profile-box` binary).
- Isolated XDG sandbox: `profile-run-20260921-105322`.
- Harvested committed e2e fixtures (~9 KiB JSONL across Claude Code / Codex / Cursor).
- Ran `village push --dry-run --timing --profile-output`.
- Result (tiny corpus): dry-run push **~37–39 ms**; local hotspot **payload build + redaction**. Network / Village storage **not measured** (no docker/podman Village stack).

### 3. Scale the corpus

- Generated synthetic datasets under `/workspace/peasant-labs/bench-datasets/sets/`:
  - `001-9kb-e2e` … `007-2gb` (≈15 KiB → ≈2 GiB).
- Each set: Claude Code + Codex + Cursor trees, `MANIFEST.json`, fake secrets/PII for redaction stress.
- Documented run recipe in `bench-datasets/README.md` (isolated XDG under `bench-runs/<set>/`).

### 4. Agree a fixed report template

- Template: **Village Upload Benchmark — Report Template** (dataset identity, harvest, push stages, bottleneck, scaling notes, anomalies, fixed `REPORT COMPLETE` line).
- Dry-run only: `harvest --json`, then `village push --dry-run --timing --profile-output`.

### 5. Seven parallel sub-agents

- One executor per dataset (`001`–`007`), isolated under `bench-runs/<set>/`.
- Each filled the template from measured artifacts (no invented stage times).
- Reports landed in chat as they finished; copies archived here under `Individual Sub-Agent Reports/`.

### 6. Package this directory

| Path | Contents |
|------|----------|
| `README.md` | This process description |
| `Individual Sub-Agent Reports/` | Seven intact per-set reports |
| `CONSOLIDATED-REPORT.md` | Cross-set findings |

---

## Commands used (pattern)

```bash
PEASANT=/workspace/peasant-labs/polyrepo/peasant/develop/bin/peasant
SET=<set-name>   # e.g. 004-100mb
SETDIR=/workspace/peasant-labs/bench-datasets/sets/$SET
RUN=/workspace/peasant-labs/bench-runs/$SET

# Config points sources at $SETDIR/{claude-code,codex/sessions,cursor}
# Fake credentials; village_url=http://127.0.0.1:9

$PEASANT … harvest --json --verbose --force
$PEASANT … village push --dry-run --non-interactive --yes --timing --verbose \
  --profile-output $RUN/profiles/push-profile.json \
  --profile-trace  $RUN/profiles/push-profile.jsonl
```

Exact flags and XDG layout: `/workspace/peasant-labs/bench-datasets/README.md`.

---

---

## Note: initial `007-2gb` dry-run failure (2026-09-21)

The `007-2gb` Village upload dry-run **did not complete** during the original seven-way parallel campaign. Details (from measured artifacts / dmesg, not invented):

1. **Primary attempt** — `village push --dry-run` (default concurrency):
   - Process exit **137** (SIGKILL / kernel OOM-kill).
   - Peak RSS sampled ~**10.79 GiB** (`11313188` KiB); `dmesg` reported `anon-rss` ~**11.38 GiB** (`11383776` kB) for the killed `peasant` process.
   - Wall ~**11324 ms**. No `push-profile.json` / `.jsonl` written (killed before profile flush). Empty push stdout/stderr.

2. **Retry** — `--concurrency 1` with `GOMEMLIMIT=12GiB` / `GOGC=50`:
   - Soft failure, process exit **3**.
   - Peak RSS ~**9.99 GiB**. Still no profile artifacts.
   - Exact error: `sqlite: deserialize to "main": memory allocation failure` while opening analytics store for dry-run against the harvested `peasant.db` (~**4.29 GiB** / `4606197760` B under `bench-runs/007-2gb/data-home/peasant/`).

3. **Root cause (code path):** dry-run / read-only open goes through `store.OpenReadOnly`, which **deserializes the entire SQLite DB into memory** and then makes a **second full copy**. On a ~4.29 GiB DB that alone wants on the order of ~8+ GiB heap before redaction/payload work, which exceeds comfortable headroom on this **15 GiB / 0-swap** box (especially with other Chrome / agent load).

Harvest for `007-2gb` itself succeeded (exit 0, ~165 s, peak harvest RSS ~7.95 GiB). Failure is push-side memory, not harvest.

A later cleanup + memory-headroom check may attempt a single re-run of `007-2gb` only if free memory is comfortably above the last successful set’s peak (~**4.25 GiB** RSS on `005-500mb`).

## Post-cleanup `007-2gb` retry (2026-09-21 ~22:40 PDT)

After Box IT’s close list:

**Closed**
- Box IT: killed whole `linkedin-guest` Chrome (was PID 411720 / CDP 9226); profile dir kept on disk.
- Peasant Lead: closed 18 stale Fork-4 Chrome tabs via CDP 9225 (Discord KEEP tab left open). No peasant/bench leftovers were running.

**Memory after cleanup (before retry)**  
`MemAvailable` ≈ **10.47 GiB** / 15.64 GiB total — comfortably above the last successful set’s peak (~**4.25 GiB** RSS on `005-500mb`).

**Retry command** (same dry-run shape as the original campaign):
```bash
peasant --config-dir …/007-2gb/config-home --data-dir …/data-home --state-dir …/state-home \
  village push --dry-run --non-interactive --yes --timing --verbose \
  --profile-output …/profiles/push-profile.json \
  --profile-trace …/profiles/push-profile.jsonl
```

**Outcome — still failed (no profile written)**

| Field | Value |
|-------|-------|
| Exit code | **3** |
| Wall time | **20140 ms** (~20.1 s) |
| Peak RSS | **10476284 KiB** (~**9.99 GiB**) |
| Profile JSON/JSONL | **not written** |
| stderr | `Error: open analytics store: store: inspect …/peasant.db for dry-run: sqlite: deserialize to "main": memory allocation failure; source files were unchanged; …` |

Same root cause as the original soft-fail retry: `store.OpenReadOnly` deserializes the entire ~4.29 GiB DB into memory then makes a second full copy (~8+ GiB) before redaction/payload work. Extra headroom from tab cleanup was not enough to finish dry-run open on this 15 GiB / 0-swap host.

This retry also made **no network upload** (`--dry-run` only).

## Related artifacts (outside this folder)

- `/workspace/peasant-labs/VILLAGE-UPLOAD-PROFILE.md` — early small-fixture profile
- `/workspace/peasant-labs/INGEST-SOURCES.md` — harness / default path inventory
- `/workspace/peasant-labs/VILLAGE-UPLOAD-DESIGN.md` — upload design notes
- `/workspace/peasant-labs/bench-datasets/` — synthetic corpora + generator
- `/workspace/peasant-labs/bench-runs/` — isolated run sandboxes + raw profiles

---

## Reading order

1. This README (process)
2. `CONSOLIDATED-REPORT.md` (findings across all seven)
3. `Individual Sub-Agent Reports/*.md` (full measured detail per set)
