# Ingest Pipeline: Agent Reference

Constraints, invariants, and assumptions for agents modifying `internal/ingest/`.
For detailed diagrams and sequence flows, see [README.md](README.md).

---

## Pipeline at a Glance

```
DISCOVER ─▶ DIFF ─▶ FILTER ─┬─▶ EXTRACT+WRITE (N workers) ─▶ StagingBuffer.Add()
                             │                                       │ lock-free CAS
                             │   drainLoop goroutine ◀── Drain ◀────┘
                             │     DB Insert → Commit
                             │     → stream per-session INDEX work
                             │     → AckBatch when parser token completes
                             │
                             │   indexLoop goroutine ◀── indexCh
                             │     parser workers → serial SQLite writer
                             │     → signal drain-batch token on indexDoneCh
                             │
                             │   main goroutine (pure controller): wg.Wait()
                             │
                             └─▶ COMPUTE ─▶ CLEANUP ─▶ REPORT ─▶ AUDIT
```

**Sequential:** DISCOVER, DIFF, FILTER, COMPUTE, CLEANUP, REPORT, AUDIT (main goroutine)
**Concurrent:** EXTRACT+WRITE (N workers) + drainLoop (DB INSERT) + indexLoop (INDEX) overlap
**Pipelined:** drainLoop streams each DB-visible session to INDEX; parser workers overlap with later drains and SQLite writes stay serial

Core data structure: **statically-allocated lock-free batched circular queue**.
Design is MPMC; currently runs **MPSC** (workers produce, drainLoop goroutine consumes).

## Stage Reference

| # | Stage | Concurrency | Fatal? | Description |
|---|-------|-------------|--------|-------------|
| 1 | DISCOVER | Sequential | Partial | `Discover()` per provider. All-fail is fatal; partial OK. |
| 2 | DIFF | Sequential | No | Classify: New / Updated / Unchanged / Active. |
| 3 | FILTER | Sequential | No | Skip Unchanged + Active; resolve FK parent deps. |
| 4a | EXTRACT+WRITE | **Parallel** (N) | Per-session | Extract metadata, redact, atomic write (tmp + rename). |
| 4b | DB INSERT | **Concurrent** (drainLoop goroutine) | Best-effort | Drain StagingBuffer → upsert SQLite → stream indexable sessions. Pipelined with INDEX. |
| 5 | INDEX | **Concurrent** (parser workers + serial writer) | Best-effort | Parse transcripts in bounded workers → serial `session_entries` writes. Receives streamed work from drainLoop. |
| 6 | COMPUTE | Sequential | Best-effort | 16 metric functions + daily insights. |
| 7 | CLEANUP | Sequential | Best-effort | Remove orphan `.tmp-*` dirs. |
| 8 | REPORT | Sequential | No | Aggregate counts → `PipelineResult`. |
| 9 | AUDIT | Sequential | Best-effort | Write `ingest_log` row. |

**Best-effort** = cannot fail the pipeline. Logs warning, continues. Only total DISCOVER failure is fatal.

---

## Lock-Free Primitives (Summary)

| Structure | File | Producers | Consumer | Purpose |
|-----------|------|-----------|----------|---------|
| **StagingBuffer** | parallel.go | N workers | drainLoop goroutine | workerResult with parent-before-child ordering |
| **SessionEntryQueue** | parallel.go | N indexers | Metric workers | Vyukov bounded MPMC ring buffer for COMPUTE |

**StagingBuffer** — slot array + 2 GiB byte arena + committed set.
Slot state machine (monotonic): `empty(0) ──Add──▶ ready(1) ──Drain──▶ claimed(2) ──AckBatch──▶ acked(3)`
`Drain()` returns a `DrainBatch` (Results + Claimed indices). `AckBatch(DrainBatch)` frees arena space.
Multiple `DrainBatch` values may be outstanding simultaneously; each owns its own `Claimed` slice. The drain loop now attaches one completion token to the streamed INDEX work for that batch, so arena bytes are released only after all parser workers that can read that batch finish.

**SessionEntryQueue** — Vyukov MPMC. Each slot has a `sequence` atomic (sole sync point).
Push: CAS `head`, write, store `seq = pos+1`. Pop: CAS `tail`, read, store `seq = pos+cap`.

See [README.md](README.md) for full sequence diagrams covering contention, backpressure, and slot recycling.

---

## Constraints

| ID | Name | Rule |
|----|------|------|
| C1 | Root-Owns-Subtree | One goroutine processes a root + its entire BFS subtree. Prevents directory races on `{hostSlug}/{parentID}/`. |
| C2 | Parent-Before-Child DB | FK ordering via `StagingBuffer.Commit()`: children invisible to `Drain()` until parent committed. |
| C3 | Atomic File Writes | Write to `.tmp-*` dir, then `os.Rename()`. CLEANUP removes orphans. |
| C4 | Schema Version Re-Ingest | `SchemaVersion < Current` → `DiffUpdated` → re-ingest. All sessions auto-upgrade on bump. |
| C5 | Arena Concurrent Drain | `Add()` uses bounded exponential backoff (1ms→16ms) when arena full. drainLoop goroutine runs concurrently with workers; arena only recycles via `AckBatch`. |
| C6 | Non-Blocking Progress | `ProgressState` pull model — `Update()` writes (pipeline goroutines), `Snapshot()` reads (renderer at its own tick rate). Never drops events. |

## Invariants

| ID | Name | Guarantee |
|----|------|-----------|
| I1 | Slot Monotonicity | States only move forward: 0→1→2→3. Never reset. Buffer sized for one full run. |
| I2 | Arena Linear Coords | `arenaHead`/`arenaTail` increase monotonically. Physical index = `coord % arenaSize`. |
| I3 | CAS Ownership | CAS winner exclusively owns slot data. No locks needed for subsequent read/write. |
| I4 | Committed Append-Only | `committed` map only grows. Single drainer reads it; `Commit` called between batches. No race. |
| I5 | Worker Count Bounds | `min(config.Parallelism, len(roots))`. Buffer allocated with `len(toProcess) + 1` slots. |

## Assumptions

| ID | Name | What it depends on |
|----|------|--------------------|
| A1 | Linux Overcommit | 2 GiB arena uses virtual memory overcommit. RSS = actual transcript volume. May fail if `vm.overcommit_memory=2`. |
| A2 | Shallow Trees | Root-owns-subtree (C1) assumes 1-2 levels. Deep trees cause load imbalance. |
| A3 | Single Instance | One pipeline per process. External PID lock prevents concurrent `peasant ingest`. |
| A4 | Unique Session IDs | UUIDs globally unique across providers/hosts. `committed` map + DB keys depend on this. |
| A5 | Rename Atomicity | `os.Rename()` atomic on local FS (ext4, APFS, NTFS). Not guaranteed on network FS. |

---

## Error Propagation

| Stage | Behavior |
|-------|----------|
| DISCOVER (all fail) | **Fatal** — pipeline returns error |
| DISCOVER (partial) | Continue with available providers |
| DIFF (corrupt) | Treat as `DiffNew` (re-ingest) |
| EXTRACT+WRITE | Per-session error in `SessionResult.Error`; continues |
| DB INSERT | `PipelineResult.Summary.StoreError`; continues |
| Commit detection | `DiagnosticEntry` appended to warnings; continues |
| INDEX / COMPUTE / AUDIT | Log warning, skip; continues |
| CLEANUP | Ignore errors |

---

## Key Types

| Type | File | Purpose |
|------|------|---------|
| `Pipeline` | pipeline.go | Orchestrator (options pattern, injected deps) |
| `PipelineConfig` | pipeline.go | Sources, output dir, flags, parallelism |
| `PipelineResult` | pipeline.go | Aggregate counts, errors, session results |
| `workerResult` | parallel.go | EXTRACT+WRITE output: metadata, transcript bytes, paths |
| `DrainBatch` | parallel.go | Drain result bundling Results + Claimed slot indices; drain loop attaches Metas to stream INDEX work |
| `DiffEntry` | pipeline.go | Session + diff classification |
| `StagingBuffer` | parallel.go | Lock-free buffer with arena + parent-child ordering |
| `SessionEntryQueue` | parallel.go | Vyukov MPMC ring buffer |
| `ProgressState` | progress.go | Pull-model progress aggregator: `Update()` + `Snapshot()` |
| `ProgressEvent` | progress.go | Single stage progress update (Kind, Stage, Done, Total, Err) |

## Files

| File | ~Lines | Contents |
|------|--------|---------|
| `pipeline.go` | 2200 | 9-stage orchestrator, `processSession()`, `drainLoop()`, `indexLoop()` |
| `parallel.go` | 710 | `StagingBuffer`, `DrainBatch`, `SessionEntryQueue`, `runParallel`, `topoLevels` |
| `progress.go` | 135 | `ProgressState` (pull model), `ProgressEvent`, `emitProgress` |
| `types.go` | | Core types: `SessionID`, `Provider`, `DiffStatus`, etc. |
| `metadata.go` | | `UnifiedMetadata`, `CurrentSchemaVersion`, extraction |
| `parallel_test.go` | | Concurrency tests: MPMC, deadlock, race detection |
| `pipeline_test.go` | | Integration tests: `MemFS` + `StubGitResolver` round-trip |
