# Ingest Pipeline: Agent Reference

Constraints, invariants, and assumptions for agents modifying `internal/ingest/`.
For detailed diagrams and sequence flows, see [README.md](README.md).

---

## Pipeline at a Glance

Before persistent work selection, startup reconciliation recovers pending
publications and walks exact managed metadata locators in bounded directory
pages, parent before child. It mirrors validated missing/different artifacts
without an adapter call; a matching artifact hash leaves metadata and DerivedAt
unchanged. Explicit harness/session/since filters apply independently of saved
discovery selection. Dry-run bypasses recovery/bootstrap, and file-only runs do
not open a database. Reconciled IDs are candidates, never index success claims.

Adapter refresh is independent of indexer eligibility. Claude, Codex and Cursor
implement `ExtractMetadataFromTranscript` over captured JSONL and original
metadata context; publication keeps those transcript bytes. New native input
takes precedence. Strike and OpenCode require native input when retained context
is insufficient. Failed native acquisition preserves the prior artifact and
adapter stamp, reports a diagnostic and permits supported retained indexing.
Retained extraction preserves the acquired ingest clock, cursor and origin.

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
| 1 | DISCOVER | Sequential | Partial | `Discover()` per provider. If all fail, usable retained sessions still receive maintenance; initial import without usable input fails. |
| 2 | DIFF | Sequential | No | Classify: New / Updated / Unchanged / Active. |
| 3 | FILTER | Sequential | No | Skip Unchanged + Active; resolve FK parent deps. |
| 4a | EXTRACT+WRITE | **Parallel** (N) | Per-session | Extract metadata, redact, replace owned files under an OS lock; complete metadata commits last. |
| 4b | DB INSERT | **Concurrent** (drainLoop goroutine) | Per-session | Reconcile committed artifact, retained seeds and acquired evidence transactionally → add DerivedAt → stream indexable sessions. Failures retain file/recovery state and do not enqueue that session. |
| 5 | INDEX | **Concurrent** (parser workers + serial writer) | Best-effort | Parse transcripts in bounded workers → serial `session_entries` writes. Receives streamed work from drainLoop. |
| 6 | COMPUTE | Sequential | Best-effort | 16 metric functions + daily insights. |
| 7 | CLEANUP | Sequential | Best-effort | Remove orphan `.tmp-*` dirs. |
| 8 | REPORT | Sequential | No | Aggregate counts → `PipelineResult`. |
| 9 | AUDIT | Sequential | Best-effort | Write `ingest_log` row. |

**Best-effort** = cannot fail the pipeline. Logs warning, continues. Total DISCOVER
failure is fatal only when no usable retained session can receive maintenance.

For supported append-only and SQLite sources, each bounded root worker makes the authoritative
freshness decision from captured metadata and bytes, then processes that same capture and
persists its fingerprint/cursor; it does not reacquire the source. Batch maps hold no payloads.
No-op results still pass through staging for parent commit, progress, and release, without
store writes or indexing. Store-free logs retain a private source/identity digest bound to
the successful metadata and managed transcript, not a wire metadata extension. Matching content does
not suppress project-identity repair. Completion time remains audit data. Legacy mutable
multi-file readers retain their existing consistency limitations.

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
| C3 | Atomic File Writes | Root-confined owned-file publication uses OS advisory locks, synced temporary intents and metadata-last commit. Never delete a session subtree; children and unrelated files are not owned. Ownership is decided by NAME, never by location: inside the session `debug/` directory only names whose extension is in the closed set `defaults.DebugArtifactSuffixes()` are peasant's own, so a user file with any other extension survives a publication. Acquire file ownership before entering the serial database writer lane. |
| C4 | Metadata Compatibility | Versions below 9 require native refresh. Reading metadata 9/10 alone causes no adapter call or metadata rewrite. An omitted adapter version uses baseline 1 for refresh eligibility but stays unknown in provenance until actual extraction succeeds. Future schemas refuse refresh/index without modifying their artifacts; future adapter revisions refuse older-adapter replacement but allow supported retained reads. |
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
| I6 | Whole Records Up To The Limit | Every JSONL harness reads, redacts and indexes a single record up to `defaults.MaxJSONLRecordBytes` (256 MiB) IN FULL. No record size ever fails a session and no record is ever silently dropped. |
| I7 | Omission Is Recorded, Never Silent | A record over the limit is left out before redaction, without being loaded. It is reported with the `record_too_large` diagnostic naming its size, its line and the limit; the capture is stored incomplete with failure code `source_records_omitted`; and a PLACEHOLDER ENTRY holds its position in the indexed entries (role `tool`, entry type `tool_result`, `rawByteLength` = the record size, the typed `OmittedRecord` under `omittedRecord` in `extra`, and a reader-facing note in `contentPreview`). The rule, the diagnostic and the placeholder are the same for Claude Code, Codex, Cursor, Strike and Pi. OpenCode reads rows and legacy documents rather than lines and has no per-record size limit, so nothing there fails or is dropped at any size and this omission does not apply to it. |
| I8 | An Accounted Omission Is Full Content | A capture incomplete ONLY because records were omitted is written through the FULL content path (`ContentCaptureFormatFull`, `RequireFullContent`), so previews, export and publication all carry it with its placeholders and `diagnostics.partial`. It is the one incompleteness that may be published. The decision is read from the STORED ENTRIES, never from the failure code alone: `source_records_omitted` is also raised for an OpenCode part this build cannot render and for an orphan graph part, where nothing stands in the gap, and those keep the bounded preview and stay refused. |

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
| DISCOVER (all fail) | Continue maintenance of usable retained sessions; **fatal** when no usable retained input exists |
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
| `jsonl_records.go` | | The shared JSONL record reader (`jsonlRecordScanner`, `forEachJSONLRecord`) and the typed `OmittedRecord`. Every JSONL read site uses it; a record over the limit is skipped, never an error |
| `jsonl_omission.go` | | The omission stand-in line, the placeholder entry and its reader-facing note |
| `oversized_record_filter.go` | | The one pre-redaction filter every JSONL harness runs (`filterOversizedJSONLRecords`) |
