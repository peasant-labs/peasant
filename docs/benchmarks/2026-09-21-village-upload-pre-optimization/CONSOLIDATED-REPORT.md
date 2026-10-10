# Consolidated Report — Village Upload Pre-Optimization Benchmarking

**Date:** 2026-09-21 (PDT)  
**Mode:** Dry-run only (`village push --dry-run`). No live Village.  
**Sources:** Individual reports in `Individual Sub-Agent Reports/`.

---

## Executive summary

| Scale | Push result | Dominant local cost (when profiled) | Memory outcome |
|-------|-------------|-------------------------------------|----------------|
| 001 (~15 KiB) | Completed, **37 ms** | `push.payload.build` + redaction | ~49 MiB peak RSS |
| 002 (~1 MiB) | Completed, **2226 ms** | `redaction.apply` / `push.payload.build` | ~64 MiB |
| 003 (~10 MiB) | Completed, **24242 ms** | `redaction.apply` (~81%) | ~220 MiB |
| 004 (~100 MiB) | Completed, **217415 ms** (~3.6 min) | `redaction.apply` (~88.5%) | ~1.48 GiB |
| 005 (~500 MiB) | Completed exit 0, **499490 ms** (~8.3 min), **0 would-push** | Session redact / `redaction.apply` (~95–98%) | ~4.25 GiB |
| 006 (~1 GiB) | **Did not complete profiled push** | n/a | Harvest OOM (exit 137); push exit 3 at sqlite deserialize (~7.75 GiB RSS) |
| 007 (~2 GiB) | **Did not complete profiled push** | n/a | Push OOM-killed (exit 137, ~10.8 GiB); retry exit 3 sqlite alloc |

**Pre-optimization takeaway:** On workable dry-runs (≤~500 MiB corpus), **redaction** (and payload build that includes document assembly) dominates wall time. At **≥~1 GiB**, this 15 GiB / 0-swap box fails in **memory** (harvest OOM, push sqlite deserialize / kernel OOM) before transport can be measured. Transport and Village storage remain **unmeasured** (dry-run + no Village stack).

---

## Cross-set timing table (measured)

| Set | Dataset size (approx) | Harvest | Push total | Hottest profiled stage | Peak push RSS |
|-----|----------------------|---------|------------|------------------------|---------------|
| 001-9kb-e2e | 14.6 KiB / 10 files | ~1.5 s | **37 ms** | payload.build 20 ms; redaction.apply 11 ms | 49.7 MiB |
| 002-1mb | 1.04 MiB / 20 files | 400 ms | **2226 ms** | redaction.apply 1624 ms; payload.build 1499 ms | 63.5 MiB |
| 003-10mb | 10.2 MiB / 61 files | 13.2 s | **24242 ms** | redaction.apply 19626 ms (~81%) | 220 MiB |
| 004-100mb | 100 MiB / 113 files | 22.2 s | **217415 ms** | redaction.apply 192486 ms (~88.5%) | 1.48 GiB |
| 005-500mb | 506 MiB / 163 files | 50 s | **499490 ms** | session.redact 487475 ms; redaction.apply 477553 ms | 4.25 GiB |
| 006-1gb | 1.01 GiB / 178 files | **OOM 89604 ms** | **10737 ms** wall then fail (no profile) | n/a | ~7.75 GiB at push fail |
| 007-2gb | 2.02 GiB / 217 files | OK 165 s | **11324 ms** then OOM (no profile) | n/a | ~10.8 GiB at kill |

Stage totals from profiles **overlap** and must not be summed to wall clock.

---

## Bottleneck analysis

### When profiling succeeded (001–005)

1. **Redaction is the primary CPU cost** once corpora leave the toy size. Share of profiled run rises from ~26% (001) to ~81% (003) to ~88% (004) to ~95%+ (005).
2. **`push.payload.build`** is the second major cost (document/map assembly; no separate serialize stage in the catalog).
3. **`push.session` envelope** always looks huge in profiles (~99%+) because it wraps load + redact + build; use child stages for optimization targeting.
4. **Transport is always n/a** in this campaign (dry-run; fake `127.0.0.1:9`). Do not treat dry-run times as end-to-end upload SLAs.

### Scaling behavior (001→005)

- Push wall grows **super-linearly** with corpus size in this range (roughly 37 ms → 2.2 s → 24 s → 217 s → 499 s).
- Peak RSS also rises sharply (tens of MiB → multi-GiB), consistent with holding redacted payloads / DB working set in process.
- **Hypothesis:** Redaction scans grow with entry/byte volume (`bytesScanned` climbs into hundreds of MiB), and payload build re-materializes large JSON documents; together they dominate before any network I/O.

### Memory cliff (006–007)

- **006:** Harvest killed by kernel OOM; partial DB left. Dry-run push then fails opening the DB via sqlite **deserialize-to-memory** (second full copy of a multi-GiB DB) — exit 3, no stage profile.
- **007:** Harvest completes (~4.3 GiB DB, ~8 GiB harvest RSS). Push OOM-killed at ~10.8 GiB; concurrency-1 retry fails sqlite alloc on the same deserialize path.
- **Hypothesis:** Dry-run/read-only open that deserializes the entire `peasant.db` into heap cannot scale to multi-GiB DBs on a 15 GiB / 0-swap host, especially with parallel large runs.

---

## Quality / correctness notes (synthetic corpora)

These are **generator / fixture** issues that inflate “failed” session counts; they are not Village network errors:

| Pattern | Seen on | Effect |
|---------|---------|--------|
| Cursor index `tool_result` / metadata gaps | 002–007 | Many `metadata-missing` push failures |
| Codex sessions without model | 002–005 | `no-model` client-side failures |
| Oversized JSON (>4 MiB scan limit) | 004–005 | `push.payload.build` failures |
| Duplicate `agent-00000000` snapshots | several | Harvest/push session-count skew |
| Dry-run “0 errors — no HTTP” vs typed client Errors | all completed pushes | Cosmetics of dry-run accounting |

For optimization work, prefer metrics on the **ok / would-push** subset, or fix generators so more sessions are publishable.

---

## What this campaign does *not* measure

- Live Village `push.publish`, negotiate, receipt, or S3/Postgres write latency  
- Multi-machine / production concurrency  
- Real user session trees (by design)  
- Grok CLI transcripts (unsupported harness)

---

## Recommendations for optimization (pre-work)

1. **Target redaction first** for CPU: profile hot rules, streaming/incremental redact, avoid rescanning full payloads.
2. **Target payload.build second:** streaming JSON / reuse buffers / avoid multi-copy assembly of large documents.
3. **Target memory for ≥1 GiB:** avoid full-DB deserialize for dry-run/read paths; page from on-disk sqlite; serialize large-set benches (no parallel multi-GiB agents on 15 GiB hosts).
4. **Re-run campaign** after each fix on the same `bench-datasets` sets with the same template for A/B.
5. **Add a Village-backed** (or mock HTTP) transport measurement phase once local stages are improved.

---

## Per-report index

| File | REPORT COMPLETE line |
|------|----------------------|
| `Individual Sub-Agent Reports/001-9kb-e2e.md` | `REPORT COMPLETE — 001-9kb-e2e — 37 ms` |
| `Individual Sub-Agent Reports/002-1mb.md` | `REPORT COMPLETE — 002-1mb — 2226 ms` |
| `Individual Sub-Agent Reports/003-10mb.md` | `REPORT COMPLETE — 003-10mb — 24242 ms` |
| `Individual Sub-Agent Reports/004-100mb.md` | `REPORT COMPLETE — 004-100mb — 217415 ms` |
| `Individual Sub-Agent Reports/005-500mb.md` | `REPORT COMPLETE — 005-500mb — 499490 ms` |
| `Individual Sub-Agent Reports/006-1gb.md` | `REPORT COMPLETE — 006-1gb — 10737 ms` |
| `Individual Sub-Agent Reports/007-2gb.md` | `REPORT COMPLETE — 007-2gb — 11324 ms` |

---

*Generated 2026-09-21 from measured sub-agent reports. No invented stage timings.*
