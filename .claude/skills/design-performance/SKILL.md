---
name: design-performance
description: Design, implement, and review performance-critical changes in the peasant repository. Use this skill when you change a hot path (harvest, ingest, the store write lane, search, migration, reclaim), when you write or review a performance claim, when you set or check a batch size, a timeout, or a budget, when you run a benchmark or a profile, or when you plan a schema or query change on large tables. Use it also when the user says slow, fast, latency, throughput, scale, optimize, stall, or performance.
---

# Design for performance

Performance is a first-class requirement in this repository. This skill gives the rules for
performance-critical work: the design, the implementation, and the review. Every rule comes from a
measurement or from a mistake that cost time. The profiling procedures and the measured results
are in `references/profiling.md`.

## When to use this skill

- You change a hot path: harvest, ingest, the store write lane, search, migration, or reclaim.
- You write or review a performance claim, a budget, or a benchmark.
- You plan a schema, a query, or a batch change on large tables.
- The user says slow, fast, latency, throughput, scale, optimize, or stall.

Skip this skill when the change has no hot path and no claim to verify.

## Rule 1: Measure before you decide

- The live store is the user's data. It is read-only for agents.
- Run every experiment on a sandbox copy. Use `--data-dir` or a copied database.
- Keep scratch data and logs in a local scratch directory outside the repository (for example `$TMPDIR`). Do not commit them.
- Record the date, the revision, and the exact command for every number.
- Keep the difference between a measurement and an estimate. Write "estimate" and a band on an estimate.
- Do not present a guess as a measurement.
- Verify the provenance of the build under test before you trust a number.

## Rule 2: Parallel CPU, serial batched writes

- Do the parse, prepare, encode, hash, and classify work in bounded worker pools.
- Send every SQLite write through one serial lane.
- Group the writes into bounded transactions.
- Use one budget splitter for the batch bounds: session count and bytes. Every grouping site calls the splitter.
- Flush a batch when it is full. The flush interval is only a fallback: it prevents blocking when the input is slow to fill the batch.
- Send a session that is larger than the budget alone.

## Rule 3: Shape the work and the memory for the cores

State the parallel shape in the design:

- **Data parallel:** one operation over many independent items (for example, parse each session).
- **Task parallel:** different stages run at the same time (for example, the drain loop and the writer).
- **Hybrid:** a pipeline of stages, and each stage is data parallel. The split of this repository (parallel CPU, serial writer) is hybrid.

**False sharing.** Do not put values that different cores write on the same cache line. The line then travels between the cores on every write, and each core waits for the line.

- Give each worker its own counter or buffer. Merge the values after the workers stop.
- Do not write adjacent elements of one slice from different goroutines.
- Pad values that separate workers write to a cache line (64 bytes on amd64; in Go, `cpu.CacheLinePad`).
- Shard the hot state by worker or by hash, so separate writers touch separate lines.

**Sequential buffers.** Split the data for each task into contiguous buffers.

- Pre-allocate one buffer for each worker before the work starts. Partition the input so the buffers are disjoint.
- Process one buffer per worker. Reuse the buffer. Do not allocate for each item.
- Cap each buffer and the total memory. The user's machine must not run out of memory.
- Prefer a slice of values over a graph of pointers. A pointer chase costs a cache miss.
- Keep the access pattern sequential. The hardware prefetcher then fills the cache ahead of the work.
- Choose a batch size that keeps the working set inside the CPU cache.

## Rule 4: Bound every transaction. Savepoint every session.

- One failing session rolls back its own savepoint. The batch continues.
- Take multiple session locks in sorted order. This prevents deadlock.
- Keep the writer-lane hold below `busy_timeout`. Another process waits that long for the single writer.
- Run a large delete after the commit, in bounded batches.

## Rule 5: Skip the unchanged work

- Skip a refresh when the projection is identical. Do the bookkeeping only.
- Make staging idempotent with `ON CONFLICT DO NOTHING`. A retry is then free.
- Keep the skip state of each stage: index, compute, and annotate.

## Rule 6: Make every budget data-driven

A budget has three parts:

1. A derivation: a measurement or a real limit.
2. A config knob with a documented default.
3. An invariant gate. The gate asserts the invariant, not the constant.

| Budget | Derivation | The gate asserts |
|---|---|---|
| Writer-lane hold | `busy_timeout`: the wait of another process | hold ≤ the configured timeout |
| Activation batch | hold target ÷ the measured commit cost of one session | batch ≤ the configured caps |
| Sweep batch | hold target ÷ the measured delete rate | batch ≤ the configured cap |
| Refresh commits | a contract: staging ≤ 2, activation 1, sweep ≤ 1 | the WAL commit frame count ≤ 4 |

- Do not put a budget literal in the write path. Read it from config.
- Do not assert the constant in a test. Assert the invariant.
- Do not add a runtime adaptive loop in the first version. It moves the subject of the tests. Derive the defaults in the sandbox.

## Rule 7: Normalize the schema, unless a measurement pays for the exception

- Keep the schema in BCNF.
- Allow a denormalization only when it gives a large performance jump.
- Document each exception with a measured reason. Keep a gate on it.

## Rule 8: Reuse work. Do not fsync each row.

- Reuse prepared statements on the write lane.
- Do not fsync each row or each blob. One commit per batch is the durability point.
- Do not write a file when a database row can hold the data. A file adds fsync, inode, and path costs.
- Do not store a canonical JSON blob on a hot path. Store columns and derive the text.

## Rule 9: Count the work, and test the count

- Count the WAL commit frames in the test. Use `WithWALAutocheckpointDisabled` and `CountWALCommitFrames`.
- Write a commit-count fixture for every write path. An ordinary refresh costs 4 commits or fewer.
- Measure the wall time against the configured target in the sandbox.
- Use `dbstat` for table sizes.

## Anti-patterns

| Anti-pattern | The measured cost |
|---|---|
| A content copy for each generation | Write amplification; 91 GB on disk for 13 GB of blob bytes |
| A manifest file | 32 GB of `manifest.json`; a second commit point |
| A per-blob fsync | One fsync for each of 6 M files |
| An unbounded transaction | A 20–26 s writer-lane stall; about 80% of a 40-minute harvest |
| A per-row statement preparation | A measured 18% of the write lane |
| A count-based deletion guard | The guard churns on each addition; use a required-name manifest |
| A benchmark inside the timed region | The benchmark pollutes its own measurement |
| An estimate shown as a measurement | A wrong decision |

## Test promotion

Before you build a large performance harness, answer these ten questions. Record the answers in
the plan or the review handoff.

1. Subject: which invariant does the test protect?
2. Necessity: why cannot a unit test or a fixture test catch the failure?
3. Production path: does the test run the real path?
4. Cost: what does the test copy, spawn, or scan? What is the maximum runtime?
5. Lifetime: which temporary files, directories, and processes does it create?
6. Concurrency: can another test touch the same files or database?
7. CI parity: does the exact command run from a clean checkout?
8. Evidence: does the test observe user-visible or production behavior?
9. Mutation: can a small mutation make the test fail for the intended reason?
10. Exit condition: what evidence allows a later simplification or deletion?

## References

- `references/profiling.md`: the condensed profiling procedures, comparison rules, and results.
- `docs/benchmarks/procedure.md`: the full local-only benchmark procedures.
- `docs/pipeline.md`: the legacy DB-backed pipeline shape.
- Workspace: `llm/peasant--index-pipeline-performance-handoff.md` (the issue inventory) and `llm/peasant--harmonized-content-model.md` §0.5 (the performance principles).
