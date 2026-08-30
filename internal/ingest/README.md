# Ingest Pipeline: Detailed Architecture

Detailed diagrams and deep-dives for the ingest pipeline's concurrency model,
DB-backed stages, and lock-free data structures. For the compact agent
reference (constraints, invariants, assumptions), see [AGENTS.md](AGENTS.md).

---

## Pipeline Overview

```
                        SEQUENTIAL                          CONCURRENT
                   ┌──────────────────┐         ┌──────────────────────────────────────────┐
                   │                  │         │                                          │
  ┌─────────┐     │  ┌──────┐        │         │  ┌───────────────────┐                  │
  │DISCOVER │─────┼─▶│ DIFF │        │         │  │  EXTRACT+WRITE    │                  │
  └─────────┘     │  └──┬───┘        │         │  │  (N workers)      │                  │
                  │     │            │         │  │                   │                  │
                  │  ┌──▼───┐        │         │  │  processSession() │                  │
                  │  │FILTER│────────┼────────▶│  │  + BFS subtree    │                  │
                  │  └──────┘        │         │  │        │          │                  │
                  │                  │         │  │        ▼          │                  │
                  │                  │         │  │  StagingBuffer    │                  │
                  │                  │         │  │    .Add()         │                  │
                  │                  │         │  └────────┬──────────┘                  │
                  │                  │         │           │ lock-free CAS + arena copy   │
                  │                  │         │           │                              │
                  │                  │         │  ┌────────▼──────────┐  ┌─────────────┐ │
                  │                  │         │  │  drainLoop        │  │  indexLoop  │ │
                  │                  │         │  │  (goroutine)      │  │  (goroutine)│ │
                  │                  │         │  │  Drain            │  │ parser pool │ │
                  │                  │         │  │  → DB Insert      │──▶ per-session │ │
                  │                  │         │  │  → Commit         │  │ work        │ │
                  │                  │         │  │  → AckBatch(done) │◀─│ batch token │ │
                  │                  │         │  │                   │  │ serial write│ │
                  │                  │         │  └────────┬──────────┘  └─────────────┘ │
                  │                  │         │           │                              │
                  │                  │         └───────────┼──────────────────────────────┘
                  │                  │                     │ wg.Wait()
                  │  ┌───────┐       │                     │
                  │  │COMPUTE│◀──────┼─────────────────────┘
                  │  └───┬───┘       │
                  │      │           │
                  │  ┌───▼────┐      │
                  │  │ANNOTATE│      │
                  │  └───┬────┘      │
                  │      │           │
                  │  ┌───▼───┐       │
                  │  │CLEANUP│       │
                  │  └───┬───┘       │
                  │      │           │
                  │  ┌───▼───┐       │
                  │  │REPORT │       │
                  │  └───┬───┘       │
                  │      │           │
                  │  ┌───▼──┐        │
                  │  │AUDIT │        │
                  │  └──────┘        │
                  └──────────────────┘
```

The main controller owns stage ordering. The DB insert and INDEX goroutines run
at the same time as EXTRACT+WRITE so transcript parsing and SQLite work do not
wait for the full extraction batch. The ANNOTATE stage starts after COMPUTE so
classifiers always see current metrics.

Profile-only timings include `PREPARE`, `INDEX LOG`, and `AUDIT`. They appear in
index profile output, but they are not normal progress-renderer stages.

---

## Stage Reference

| Stage | Progress stage? | Concurrency | Fatal? | Description |
|-------|-----------------|-------------|--------|-------------|
| DISCOVER | Yes | Sequential | Partial | Ask enabled adapters to enumerate sessions. All providers failing is fatal; per-provider failure is partial. |
| PREPARE | Profile only | Sequential | No | Resolve stored origins and load lookup caches before DIFF. |
| DIFF | Yes | Sequential | No | Classify sessions as new, updated, unchanged, or active. |
| FILTER | Yes | Sequential | No | Apply flags, selection, and FK parent eligibility. |
| EXTRACT+WRITE | Yes | Parallel workers | Per session | Read provider data, redact when configured, write transcript/debug files through staging. |
| DB INSERT | Yes | drainLoop goroutine | Best effort | Upsert session rows, current commit projections, durable association rows, and OpenCode sequence cursors. |
| INDEX | Yes | Parser workers plus one serial writer | Best effort | Parse transcripts into `session_entries`; batch SQLite writes and skip unchanged projections by hash. |
| INDEX LOG | Profile only | Sequential | Best effort | Persist per-session index outcome rows after INDEX work completes. |
| COMPUTE | Yes | Sequential engine call | Best effort | Compute metrics and daily insights for sessions indexed successfully. |
| ANNOTATE | Yes | Prepare workers plus one serial writer | Best effort | Skip current sessions by `annotation_run_state`, or prepare and flush classifier annotation batches. |
| CLEANUP | Yes | Sequential | Best effort | Remove orphan `.tmp-*` dirs and orphan project rows. |
| REPORT | Yes | Sequential | No | Build `PipelineResult` and aggregate counts. |
| AUDIT | Profile only | Sequential | Best effort | Write the final `ingest_log` row. |

---

## Repository Identity During Discovery

The ingest package owns the transient Git topology resolver used when kickstart reshapes discovered
sessions into its project-first tree. This identity is separate from the remote-derived project label
and from the exact path used by selection, import, push, and prune.

| Value | Purpose | Persisted or rendered? |
|-------|---------|------------------------|
| `ClonePath` | Exact resolved physical worktree used at selection and side-effect boundaries | Persisted when selected; shown in project and branch previews |
| `RepositoryCohortKey` | Opaque key used to group discovered sessions into one logical project root | Never persisted or rendered |
| `RepositoryPath` / `GitDirectory` | Physical Git directory retained as diagnostic evidence | Shown in project and branch previews; never persisted as selection |
| Git remote | Matcher evidence and the source of labels such as `github:owner/repo` | May be persisted and rendered; never used alone as a cohort key |

Peasant invokes the external executable named `git` from its process `PATH` through an argument-list
`exec.CommandContext` call. The `--git-common-dir` flag belongs to Git's `rev-parse` command:

```text
git -C <physical-clone-path> rev-parse --path-format=absolute --git-common-dir
```

The resolver also asks Git for `--show-superproject-working-tree` and `--show-toplevel` when it needs
to prove a direct submodule relationship. It verifies the exact relative path through the direct
superproject's `.gitmodules` file. Remote equality is not topology evidence.

### Cohort construction

An ordinary repository derives its cohort from its resolved physical Git common directory:

```text
repo:<length-prefixed physical Git common directory>
```

A declared submodule derives its cohort recursively from the direct superproject cohort and the
declared relative submodule path:

```text
submodule:<length-prefixed superproject cohort><length-prefixed relative path>
```

This produces the following project-root behavior:

| Discovered checkouts | Project roots |
|----------------------|---------------|
| Main worktree and linked worktrees of one repository | One |
| Same declared submodule path across linked worktrees of one superproject | One |
| Independent clones of the same remote | Separate |
| Same submodule path beneath independent superproject clones | Separate |
| Different declared submodule paths that point to the same remote | Separate |
| Undeclared repository nested inside another repository | Separate |

Independent clones are intentionally not aggregated by remote. They can therefore appear as separate
project nodes with the same remote-derived label. The preview's Git-directory and worktree evidence
distinguishes those nodes. A remote label describes where a repository points; it does not prove that
two local repositories share Git object or worktree topology.

### Root uniqueness and fail-safe splitting

Within one scanner load, the project forest is keyed by `RepositoryCohortKey`, so one cohort key can
emit at most one project root. A refresh replaces the prior forest, and stale asynchronous results are
discarded rather than appended. Two visible project nodes with the same label therefore have different
cohort keys; they are not duplicate insertions of one key.

When topology cannot be proved, discovery fails safe instead of grouping by remote:

- A failure before Git's common directory is available falls back to the exact `ClonePath`.
- A later topology failure falls back to the resolved physical `GitDirectory` when available.
- A working directory that cannot be resolved does not produce a project root for that load.

This conservative fallback can split checkouts that would otherwise belong to one logical project,
for example when one linked worktree resolves successfully while another has inaccessible or stale Git
metadata. That duplicate-looking presentation is preferred over merging independent repositories and
widening selection or side-effect scope. Repairing the Git metadata allows a later scan to prove and
restore the shared cohort.

The source contracts are `RepositoryIdentityResolver` in `git.go`, the identity value types in
`path_identity.go`, and the mounted forest fold in `internal/tui/kickstart/scanner.go`.

---

## Claude Discovery Artifacts

Claude Code stores conversation transcripts and non-conversation artifacts under
`~/.claude/projects`. Known artifacts include `summary` records and
`file-history-snapshot` records. File-history records reference revisions in
`~/.claude/file-history/<uuid>/`; neither artifact type contains user or assistant
conversation turns.

Claude discovery requires at least one valid top-level `user` or `assistant` record. It applies
the same rule to root and filesystem-nested subagent paths, so summary-only, snapshot-only, and
mixed summary/snapshot artifacts never reach kickstart, ingest, or the local store. Discovery
fails open for unreadable, empty, or malformed files: a future Claude transcript format must not
be silently discarded merely because Peasant cannot classify it yet.

---

## Execution Timeline

```text
Time ──────────────────────────────────────────────────────────────▶

Main goroutine (pure controller):
  DISCOVER -> PREPARE(profile) -> DIFF -> FILTER -> spawn goroutines -> wg.Wait()
  -> stale-index sweep -> INDEX LOG(profile) -> COMPUTE -> ANNOTATE -> CLEANUP -> REPORT -> AUDIT(profile)

Worker goroutines (created by runParallel):
  ┌─ worker 1: root A + subtree ─── Add() ─── Add() ──────┐
  ├─ worker 2: root B + subtree ─── Add() ────────────────┤
  ├─ worker 3: root C + subtree ─── Add() ─── Add() ──────┤
  └─ worker N: root D ─── Add() ──────────────────────────┘ ──▶ workersDone.Store(true)

drainLoop goroutine (stage 4b, DB INSERT + coordination):
  ┌── Drain → DB Insert → Commit → enqueue streamed INDEX work ──┐
  ├── Drain → DB Insert → Commit → enqueue streamed INDEX work ──┤
  └── ...until workersDone + empty + all batch tokens acked ─────┘

indexLoop goroutine (stage 5, INDEX parser pool + serial writer):
  ┌── receive work → parse session → signal batch token when complete ──┐
  ├── parsed result → IndexSessionEntries + UpdateIndexState serially ──┤
  └── ...until indexCh closed and parsed results drained ───────────────┘

ANNOTATE prepare workers:
  ┌── receive session id -> load combined run-state inputs -> skip current ─┐
  └── or prepare classifier writes + run state for writer goroutine ────────┘

ANNOTATE writer goroutine:
  ┌── collect prepared batches -> FlushAnnotationBatches serially ──────────┐
  └── flush on 256 sessions, 4096 writes, 500ms, or input close ────────────┘
```

The drainLoop and indexLoop are **pipelined**: each DB-visible session is sent
to INDEX without waiting for a full later batch boundary. The drainLoop calls
`AckBatch` only after the INDEX parser workers signal that every work item for
that drain batch has finished reading arena-backed transcript bytes.

INDEX is parallel only on parse. SQLite entry writes stay serial. The serial
writer flushes up to `64` parsed sessions per outer transaction and keeps
per-session rollback with savepoints. A session whose `session_entries_hash`,
derived entry tables, and entry-target annotation spans still match is reported
as written but skipped, so warm runs do not replace millions of unchanged entry
rows.

ANNOTATE is also split into parallel preparation and serial persistence. Each
prepare worker first loads combined annotation run inputs. If the stored
`annotation_run_state` matches the current entry hash, compute version, and
classifier version, the session is marked done without listing entries or
running classifiers. Otherwise, classifier results are converted into prepared
writes and one writer goroutine flushes multi-session batches. This shape keeps
CPU-bound classifier preparation parallel while avoiding concurrent SQLite
annotation writers.

---

## StagingBuffer (parallel.go)

Holds completed `workerResult` values while respecting parent-before-child
ordering for DB insertion.

**Components:**
- **Slot array** — Fixed-size, CAS-indexed. State machine per slot:
  `empty(0) → ready(1) → claimed(2) → acked(3)`
- **Byte arena** — 2 GiB pre-allocated slab (virtual; RSS grows on demand via
  Linux overcommit). Ring-buffer semantics with linear coordinate space.
- **Committed set** — `map[SessionID]struct{}` guarded by mutex. Only written
  by the single drainer after DB insert succeeds.

### Sequence Diagram: Parent-Before-Child Ordering

Two workers producing root A (with child A1) and root B. drainLoop goroutine
drains with parent-before-child ordering enforced by the committed set.

The pipelined pattern is `Drain → DB Insert → Commit → enqueue streamed INDEX work → AckBatch(when token completes)`.
`Commit` runs before `AckBatch` so children become eligible for the next `Drain`
while arena space is still held for parser workers that need it.

```
  Worker 1            Worker 2            StagingBuffer          drainLoop           indexLoop
  ────────            ────────            ─────────────          ─────────           ─────────
     │                   │                     │                     │                   │
     │ processSession(A) │                     │                     │                   │
     │───────────────────┼────────────────────▶│                     │                   │
     │                   │ processSession(B)   │                     │                   │
     │                   │────────────────────▶│                     │                   │
     │                   │                     │      Drain()         │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │  return A, B         │                   │
     │                   │                     │                     │                   │
     │                   │                     │                DB Insert(A, B)           │
     │                   │                     │      Commit(A, B)   │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │                     │──work A──────────▶│ parse A
     │                   │                     │                     │──work B──────────▶│ parse B
     │ BFS: child A1     │                     │                     │                   │ serial write A
     │ processSession(A1)│                     │                     │                   │ serial write B
     │───────────────────┼────────────────────▶│                     │◀──batch token────│
     │                   │                     │      AckBatch(A,B)  │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │                     │                   │
     │                   │                     │      Drain()         │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │  A1.parent=A ok      │                   │
     │                   │                     │  return A1           │                   │
     │                   │                     │                     │──work A1────────▶│ parse A1
     │                   │                     │                DB Insert(A1)             │ serial write A1
     │                   │                     │      Commit(A1)     │◀──batch token────│
     │                   │                     │◀────────────────────│                   │
     │                   │                     │      AckBatch(A1)   │                   │
     │                   │                     │◀────────────────────│                   │
     ▼                   ▼                     ▼                     ▼                   ▼
```

### Sequence Diagram: Arena Backpressure

What happens when the 2 GiB arena fills — worker uses bounded exponential
backoff (1ms→16ms cap) until the drainLoop's `AckBatch` frees arena space.

```
  Worker 3            StagingBuffer          drainLoop
  ────────            ─────────────          ─────────
     │                     │                     │
     │ Add(large result)   │                     │
     │────────────────────▶│                     │
     │                     │ CAS arenaHead        │
     │                     │ free < payload size! │
     │                     │                     │
     │              ┌────▶ │ sleep(1ms)           │
     │              │      │ (backoff: 1→2→4→16ms)│
     │              │      │                     │
     │              │      │      Drain()         │
     │              │      │◀────────────────────│
     │              │      │   ... DB Insert ...  │
     │              │      │      AckBatch()      │
     │              │      │◀────────────────────│
     │              │      │  arenaTail advanced  │
     │              │      │  (space freed)        │
     │              │      │                     │
     │              └───── │ retry CAS arenaHead  │
     │                     │ free >= payload → ok │
     │                     │ copy transcript       │
     │                     │ CAS count → slot N    │
     │                     │ state[N].Store(ready) │
     │                     │─────────────────────▶│
     ▼                     ▼                     ▼
```

### Producer Path (Add)

1. CAS on `arenaHead` to reserve byte range for transcript data
2. `copy()` transcript bytes into arena (contiguous, no straddle)
3. CAS on `count` to claim a slot index
4. Write `stagedEntry` to slot (sole owner after CAS)
5. `state[idx].Store(1)` — publish to consumer (release semantics)

### Consumer Path (Drain → DrainBatch → AckBatch + Commit)

1. `Drain()` scans slots with `state == ready(1)` and `isEligible()` (parent committed)
2. Transition eligible slots to `claimed(2)`; return `DrainBatch` (Results + Claimed indices)
3. DB insert using `batch.Results`
4. `Commit(ids...)` unlocks children for the next `Drain` (before `AckBatch`)
5. Attach a drain-batch completion token to each indexable session and send it to indexLoop via `indexCh`
6. Keep draining later eligible work while parser workers read arena-backed transcript data
7. When `indexDoneCh` returns a completed drain batch, call `AckBatch(batch)` to transition slots to `acked(3)` and advance `arenaTail`

The `DrainBatch` bundles results and claimed slot indices together — no shared
mutable state between calls. Multiple `DrainBatch` values may be outstanding
simultaneously; each owns its own `Claimed` slice.

---

## SessionEntryQueue (parallel.go)

Dmitry Vyukov's bounded MPMC ring buffer for the COMPUTE stage. Multiple
indexers push `SessionEntry` values; metric workers pop concurrently.

### Sequence Diagram: MPMC Contention and Slot Recycling

Two producers and two consumers on a queue with capacity 4 (mask=3). Shows
CAS contention, slot ownership handoff via sequence numbers, and recycling.

```
  Producer P1         Producer P2         Queue (cap=4)       Consumer C1         Consumer C2
  ───────────         ───────────         ─────────────       ───────────         ───────────
     │                   │                     │                   │                   │
     │                   │                head=0, tail=0           │                   │
     │                   │                seq=[0,1,2,3]            │                   │
     │                   │                     │                   │                   │
     │ Push(X)           │                     │                   │                   │
     │ pos←head=0        │                     │                   │                   │
     │ seq[0]=0          │                     │                   │                   │
     │ diff=0-0=0        │ Push(Y)             │                   │                   │
     │ CAS head 0→1 ✓    │ pos←head=0          │                   │                   │
     │ (owns slot 0)     │ CAS head 0→1 ✗      │                   │                   │
     │                   │ (retry)             │                   │                   │
     │ write X           │                     │                   │                   │
     │ seq[0].Store(1)   │ pos←head=1          │                   │                   │
     │ ──(release)──     │ seq[1]=1             │                   │                   │
     │                   │ diff=1-1=0           │                   │                   │
     │                   │ CAS head 1→2 ✓       │                   │                   │
     │                   │ (owns slot 1)        │                   │                   │
     │                   │ write Y              │                   │                   │
     │                   │ seq[1].Store(2)      │                   │                   │
     │                   │ ──(release)──        │                   │                   │
     │                   │                     │                   │                   │
     │                   │                     │ Pop()              │                   │
     │                   │                     │ pos←tail=0         │                   │
     │                   │                     │ seq[0]=1           │ Pop()              │
     │                   │                     │ diff=1-1=0         │ pos←tail=0         │
     │                   │                     │ CAS tail 0→1 ✓     │ CAS tail 0→1 ✗    │
     │                   │                     │ (owns slot 0)      │ (retry)            │
     │                   │                     │ read X             │                   │
     │                   │                     │ seq[0].Store(0+4)  │ pos←tail=1         │
     │                   │                     │ ──(recycle)──      │ seq[1]=2            │
     │                   │                     │                   │ diff=2-2=0          │
     │                   │                     │ return X          │ CAS tail 1→2 ✓     │
     │                   │                     │──────────────────▶│ (owns slot 1)      │
     │                   │                     │                   │ read Y              │
     │                   │                     │                   │ seq[1].Store(1+4)   │
     │                   │                     │                   │ ──(recycle)──       │
     │                   │                     │                   │                   │
     │                   │                     │                   │ return Y            │
     │                   │                     │                   │──────────────────▶ │
     │                   │                     │                   │                   │
     │                   │                head=2, tail=2           │                   │
     │                   │                seq=[4,5,2,3]            │                   │
     │                   │                     │                   │                   │
     │ Push(Z)           │                     │                   │                   │
     │ pos←head=2        │                     │                   │                   │
     │ seq[2]=2          │                     │                   │                   │
     │ diff=2-2=0        │                     │                   │                   │
     │ CAS head 2→3 ✓    │                     │                   │                   │
     │ write Z           │                     │                   │                   │
     │ seq[2].Store(3)   │                     │                   │                   │
     │                   │                     │                   │                   │
     │    slot 0 now reusable: pos=4, seq=4, diff=0               │                   │
     │                   │                     │                   │                   │
     ▼                   ▼                     ▼                   ▼                   ▼

  Key:  CAS ✓ = won compare-and-swap       CAS ✗ = lost race, retry
        seq.Store(pos+1) = release to consumer (slot filled)
        seq.Store(pos+cap) = recycle for next producer (slot freed)
```

### Sequence Diagram: Queue Full

Producer spins on `Gosched()` until a consumer frees a slot.

```
  Producer P1            Queue (cap=2)            Consumer C1
  ───────────            ─────────────            ───────────
     │                        │                        │
     │                   head=2, tail=0                │
     │                   seq=[1, 2]                    │
     │                   (both slots filled)           │
     │                        │                        │
     │ Push(W)                │                        │
     │ pos←head=2             │                        │
     │ slot[0].seq=1          │                        │
     │ diff=1-2=-1            │                        │
     │ (queue full!)          │                        │
     │                        │                        │
     │ ┌───▶ Gosched()       │                        │
     │ │    (yield CPU)       │                        │
     │ │                      │ Pop()                  │
     │ │                      │ CAS tail 0→1 ✓         │
     │ │                      │ read entry             │
     │ │                      │ seq[0].Store(0+2=2)    │
     │ │                      │ (slot 0 recycled)      │
     │ │                      │                        │
     │ └──── retry            │                        │
     │ pos←head=2             │                        │
     │ slot[0].seq=2          │                        │
     │ diff=2-2=0             │                        │
     │ CAS head 2→3 ✓         │                        │
     │ (owns slot 0)          │                        │
     │ write W                │                        │
     │ seq[0].Store(3)        │                        │
     ▼                        ▼                        ▼
```

### Algorithm

Each slot has a `sequence` atomic counter (sole sync point).
- Push: CAS `head`, write entry, store `sequence = pos + 1` (release)
- Pop: CAS `tail`, read entry, store `sequence = pos + cap` (recycle)
- Capacity must be power-of-two (enables `pos & mask` instead of modulo)
- Cache-line padding (64 bytes) on head, tail, and each slot prevents false sharing

**Reference:** https://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue
