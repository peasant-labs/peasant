# Ingest Pipeline: Detailed Architecture

Detailed diagrams and deep-dives for the ingest pipeline's concurrency model
and lock-free data structures. For the compact agent reference (constraints,
invariants, assumptions), see [AGENTS.md](AGENTS.md).

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
                  │                  │         │  │                   │  │             │ │
                  │                  │         │  │  Drain            │  │ index each  │ │
                  │                  │         │  │  → DB Insert      │──▶ session     │ │
                  │                  │         │  │  → Commit         │  │ in batch    │ │
                  │                  │         │  │  → wait INDEX     │◀─│             │ │
                  │                  │         │  │  → AckBatch(prev) │  │ signal done │ │
                  │                  │         │  └────────┬──────────┘  └─────────────┘ │
                  │                  │         │           │                              │
                  │                  │         └───────────┼──────────────────────────────┘
                  │                  │                     │ wg.Wait()
                  │  ┌───────┐       │                     │
                  │  │COMPUTE│◀──────┼─────────────────────┘
                  │  └───┬───┘       │
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

## Execution Timeline

```
Time ──────────────────────────────────────────────────────────────▶

Main goroutine (pure controller):
  DISCOVER ─▶ DIFF ─▶ FILTER ─▶ spawn goroutines ─▶ wg.Wait() ─▶ COMPUTE ─▶ ...

Worker goroutines (in a single goroutine via runParallel):
  ┌─ worker 1: root A + subtree ─── Add() ─── Add() ──────┐
  ├─ worker 2: root B + subtree ─── Add() ────────────────┤
  ├─ worker 3: root C + subtree ─── Add() ─── Add() ──────┤
  └─ worker N: root D ─── Add() ──────────────────────────┘ ──▶ workersDone.Store(true)

drainLoop goroutine (stage 4b, DB INSERT + coordination):
  ┌── Drain → DB Insert → Commit → wait indexDoneCh → AckBatch(prev) → send indexCh ──┐
  ├── Drain → DB Insert → Commit → wait indexDoneCh → AckBatch(prev) → send indexCh ──┤
  └── ...until workersDone + empty ──────────────────────────────────────────────────┘

indexLoop goroutine (stage 5, INDEX):
  ┌── receive batch → index each session → signal indexDoneCh ──┐
  ├── receive batch → index each session → signal indexDoneCh ──┤
  └── ...until indexCh closed ──────────────────────────────────┘
```

The drainLoop and indexLoop are **pipelined**: batch N+1 DB INSERT overlaps with
batch N INDEX. The drainLoop waits on `indexDoneCh` before calling `AckBatch`
on the previous batch — arena data is only freed after the indexLoop finishes
reading it.

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

The pipelined pattern: `Drain → DB Insert → Commit → wait INDEX(prev) → AckBatch(prev) → send to indexCh`.
`Commit` runs before `AckBatch` so children become eligible for the next `Drain`
while arena space is still held for the indexLoop.

```
  Worker 1            Worker 2            StagingBuffer          drainLoop           indexLoop
  ────────            ────────            ─────────────          ─────────           ─────────
     │                   │                     │                     │                   │
     │ processSession(A) │                     │                     │                   │
     │───────────────────┼────────────────────▶│                     │                   │
     │                   │                     │ CAS arenaHead        │                   │
     │                   │                     │ copy transcript A    │                   │
     │                   │                     │ CAS count → slot 0   │                   │
     │                   │                     │ state[0].Store(ready)│                   │
     │                   │                     │                     │                   │
     │                   │ processSession(B)   │                     │                   │
     │                   │────────────────────▶│                     │                   │
     │                   │                     │ CAS arenaHead        │                   │
     │                   │                     │ copy transcript B    │                   │
     │                   │                     │ CAS count → slot 1   │                   │
     │                   │                     │ state[1].Store(ready)│                   │
     │                   │                     │                     │                   │
     │                   │                     │      Drain()         │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │  slot 0: ready, root │                   │
     │                   │                     │  slot 1: ready, root │                   │
     │                   │                     │  state[0,1] → claimed│                   │
     │                   │                     │─────────────────────▶│                   │
     │                   │                     │  return DrainBatch   │                   │
     │                   │                     │                     │                   │
     │                   │                     │                DB Insert(A, B)           │
     │                   │                     │                     │                   │
     │                   │                     │      Commit(A, B)   │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │  committed += {A, B} │                   │
     │                   │                     │                     │                   │
     │                   │                     │                     │ (no prev batch)    │
     │                   │                     │                     │                   │
     │                   │                     │                     │──send batch──────▶│
     │                   │                     │                     │                   │ index A, B
     │                   │                     │                     │◀──indexDoneCh─────│
     │                   │                     │                     │                   │
     │                   │                     │      AckBatch(prev) │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │  state[0,1] → acked  │                   │
     │                   │                     │  arenaTail += freed  │                   │
     │                   │                     │                     │                   │
     │ BFS: child A1     │                     │                     │                   │
     │ processSession(A1)│                     │                     │                   │
     │───────────────────┼────────────────────▶│                     │                   │
     │                   │                     │ CAS arenaHead        │                   │
     │                   │                     │ CAS count → slot 2   │                   │
     │                   │                     │ state[2].Store(ready)│                   │
     │                   │                     │                     │                   │
     │                   │                     │      Drain()         │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │  slot 2: ready       │                   │
     │                   │                     │  A1.parent=A         │                   │
     │                   │                     │  A in committed → ok │                   │
     │                   │                     │  state[2] → claimed  │                   │
     │                   │                     │─────────────────────▶│                   │
     │                   │                     │  return DrainBatch   │                   │
     │                   │                     │                     │                   │
     │                   │                     │                DB Insert(A1)             │
     │                   │                     │                     │                   │
     │                   │                     │      Commit(A1)     │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │                     │                   │
     │                   │                     │                     │ wait indexDoneCh   │
     │                   │                     │                     │◀──indexDoneCh─────│
     │                   │                     │      AckBatch(prev) │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │                     │──send batch──────▶│
     │                   │                     │                     │                   │ index A1
     │                   │                     │                     │                   │
     ├─ done             ├─ done               │                     │                   │
     │                   │                     │                     │                   │
     │     workersDone.Store(true)             │                     │                   │
     │                   │                     │      Drain() → empty │                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │  workersDone == true │                   │
     │                   │                     │  buffer empty        │                   │
     │                   │                     │  wait final indexDone│                   │
     │                   │                     │◀──indexDoneCh─────────────────────────▶│
     │                   │                     │      AckBatch(final)│                   │
     │                   │                     │◀────────────────────│                   │
     │                   │                     │                     │ close(indexCh)     │
     │                   │                     │                     │ exit               │
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
5. Wait for previous batch's `indexDoneCh` signal (indexLoop finished reading arena data)
6. `AckBatch(prevBatch)` transitions slots to `acked(3)` and advances `arenaTail`
7. Send current batch (with `Metas` populated) to indexLoop via `indexCh`

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
