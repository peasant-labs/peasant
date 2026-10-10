================================================================================
FINDINGS LOG — Village upload “bigger issue” (dry-run store memory)
================================================================================
Session: 01a0c7af-570d-7a81-89d4-03f95e735094 (continued 2026-09-21 22:57–23:20 PT)
Agent: Grok Build TUI
Repo: /workspace/peasant-labs/polyrepo/peasant/develop
Related bench: /workspace/peasant-labs/bench-runs/007-2gb
Status: ROOT_CAUSE_CONFIRMED
================================================================================

### Finding 001
- timestamp_pt: 2026-09-21 22:44:00 PT
- category: code_path
- severity: critical
- confidence: high
- summary: Dry-run opens the analytics DB via store.OpenReadOnly, which ReadAlls the entire file then sqlite Deserialize into a private in-memory DB.
- details: |
    internal/store/readonly.go OpenReadOnly:
      1) readCheckpointedDatabase → io.ReadAll(file) → full []byte copy of peasant.db
      2) mutates bytes 18–19 on the PRIVATE copy only (WAL→rollback header tweak)
      3) sqlitex.NewPool("file:peasant-readonly?mode=memory&cache=private", …)
      4) conn.Deserialize("main", data) — loads that buffer into an in-memory sqlite
    Comments state the intent: avoid even mode=ro opening which can create WAL/SHM.
    cmd/peasant/readonly_run.go openRunStore(dryRun=true) → store.OpenReadOnly(path).
    cmd/peasant/cmd_push.go calls openRunStore(cmd, dryRun) before any pipeline work.
- evidence_paths:
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/store/readonly.go:17-86
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/store/readonly.go:88-138
    - /workspace/peasant-labs/polyrepo/peasant/develop/cmd/peasant/readonly_run.go:40-50
    - /workspace/peasant-labs/polyrepo/peasant/develop/cmd/peasant/cmd_push.go:244-248
- symbols:
    - store.OpenReadOnly
    - store.readCheckpointedDatabase
    - sqlite.Conn.Deserialize
    - openRunStore
- metrics:
    - db_bytes: 4606197760
    - peak_rss_kib: 10476284
    - exit_code: 3
    - wall_ms: 20140
- links_to_findings: []
- issue_flag: true
- issue_labels: ["bug","performance","memory","village-push","dry-run"]
- notes: |
    Intentional dry-run isolation, not an accidental shared path with harvest writes.

--------------------------------------------------------------------------------

### Finding 002
- timestamp_pt: 2026-09-21 22:44:30 PT
- category: measurement
- severity: high
- confidence: high
- summary: Post-cleanup retry still failed with deserialize memory allocation failure despite ~10.5 GiB MemAvailable.
- details: |
    After Box IT killed linkedin-guest Chrome and Peasant Lead closed 18 Fork-4 tabs,
    MemAvailable ≈ 10.47–10.5 GiB. Retry of village push --dry-run on 007-2gb exited 3
    with sqlite deserialize memory allocation failure; peak RSS 10476284 KiB; no profile.
    Exact stderr is preserved in bench-runs/007-2gb/profiles/push.stderr.
- evidence_paths:
    - /workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/README.md
    - /workspace/peasant-labs/bench-runs/007-2gb/profiles/push.meta
    - /workspace/peasant-labs/bench-runs/007-2gb/profiles/push.stderr
- symbols:
    - store.OpenReadOnly
- metrics:
    - db_bytes: 4606197760
    - peak_rss_kib: 10476284
    - exit_code: 3
    - wall_ms: 20140
- links_to_findings: [001]
- issue_flag: true
- issue_labels: ["bug","memory","dry-run"]
- notes: |
    Closing Chrome was necessary hygiene but cannot fix a multi-GiB double materialization.

--------------------------------------------------------------------------------

### Finding 003
- timestamp_pt: 2026-09-21 23:00:00 PT
- category: code_path
- severity: critical
- confidence: high
- summary: The exact stderr string is produced by zombiezen sqlite.Conn.Deserialize when sqlite3_malloc64 of len(data) returns NULL, before the bytes are copied.
- details: |
    zombiezen.com/go/sqlite@v1.4.2 Conn.Deserialize (sqlite.go:621-641):
      n := int64(len(data))
      pData := lib.Xsqlite3_malloc64(c.tls, uint64(n))
      if pData == 0 {
          return fmt.Errorf("sqlite: deserialize to %q: memory allocation failure", dbName)
      }
      copy(libc.GoBytes(pData, len(data)), data)
    modernc libc GoBytes is a slice view of the C allocation, not a third Go copy.
    Flags passed to sqlite3_deserialize are SQLITE_DESERIALIZE_FREEONCLOSE|RESIZEABLE,
    so the C allocation is a second full buffer owned by SQLite for the life of the
    connection. The Go []byte from io.ReadAll is still live until OpenReadOnly returns.
    Failure of 007 is this malloc returning 0 while the Go buffer is still held.
    The wrapped CLI text matches push.stderr exactly:
    "sqlite: deserialize to \"main\": memory allocation failure"
- evidence_paths:
    - /home/box/go/pkg/mod/zombiezen.com/go/sqlite@v1.4.2/sqlite.go:621-641
    - /home/box/go/pkg/mod/modernc.org/libc@v1.55.3/etc.go:613-619
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/store/readonly.go:48-49
    - /workspace/peasant-labs/bench-runs/007-2gb/profiles/push.stderr
- symbols:
    - sqlite.Conn.Deserialize
    - sqlite3_malloc64
- metrics:
    - db_bytes: 4606197760
    - peak_rss_kib: 10476284
    - exit_code: 3
    - wall_ms: 20140
- links_to_findings: [001, 002]
- issue_flag: true
- issue_labels: ["bug","memory","dry-run"]
- notes: |
    Driver is zombiezen.com/go/sqlite (modernc), not go-sqlite3/cgo.

--------------------------------------------------------------------------------

### Finding 004
- timestamp_pt: 2026-09-21 23:02:00 PT
- category: measurement
- severity: high
- confidence: high
- summary: io.ReadAll of the 4.29 GiB file retains a 4.63 GiB Go slice; the last growth briefly holds ~8.34 GiB before Deserialize asks for another 4.29 GiB.
- details: |
    Go 1.25 io.ReadAll starts at cap 512 and grows via append (nextslicecap).
    For large slices the growth factor is ~1.25, not 2x.
    Simulated against db_bytes=4606197760 (Go 1.25 nextslicecap):
      final cap = 4975769013 bytes (4.634 GiB), overshoot 0.344 GiB
      last growth: old 3.707 GiB + new 4.634 GiB live together (8.341 GiB)
      then Deserialize malloc of len(data) = 4.290 GiB while the 4.634 GiB slice is still referenced
      simultaneous floor after growth completes = 8.924 GiB, before Go releases the previous slice
    Soft-fail peak RSS 9.99 GiB sits just above that 8.92 GiB floor.
    Primary OOM total-vm 19027544 kB (~18.1 GiB) and anon-rss 11383776 kB (~10.85 GiB)
    match several large anonymous mappings overlapping.
- evidence_paths:
    - /usr/local/go/src/io/io.go:709-726
    - /usr/local/go/src/runtime/slice.go:288-313
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/store/readonly.go:111
- symbols:
    - io.ReadAll
    - runtime.nextslicecap
    - store.readCheckpointedDatabase
- metrics:
    - db_bytes: 4606197760
    - peak_rss_kib: 10476284
    - exit_code: 3
    - wall_ms: n/a
- links_to_findings: [001, 003]
- issue_flag: true
- issue_labels: ["bug","memory","dry-run"]
- notes: |
    The second full copy is the sqlite malloc, not a second Go ReadAll.
    Slice growth makes the peak worse than a clean 2x.

--------------------------------------------------------------------------------

### Finding 005
- timestamp_pt: 2026-09-21 23:02:30 PT
- category: measurement
- severity: high
- confidence: high
- summary: This host has 0 swap, overcommit_memory=0, and CommitLimit ~7.82 GiB, so a single 4.29 GiB sqlite malloc can fail after the Go buffer is already resident.
- details: |
    Measured 2026-09-21 22:57 PT before the local Village experiment:
      MemTotal 16397616 kB, SwapTotal 0, overcommit_memory 0, overcommit_ratio 50
      CommitLimit 8198808 kB, Committed_AS already 22182184 kB (above CommitLimit)
    Mode 0 is heuristic overcommit: a single huge malloc is more likely to be
    refused than the same bytes allocated in ~25% steps by ReadAll.
    MemAvailable ~10.5 GiB was measured BEFORE the retry, not at the malloc.
    By the time sqlite3_malloc64(4606197760) runs, the process already holds
    the ReadAll buffer (and possibly the previous growth buffer). Peak RSS
    10476284 KiB at exit 3 is that first materialization, and the second
    allocation is the one that returns NULL.
- evidence_paths:
    - /proc/meminfo
    - /proc/sys/vm/overcommit_memory
    - /workspace/peasant-labs/bench-runs/007-2gb/profiles/push.meta
- symbols:
    - sqlite3_malloc64
- metrics:
    - db_bytes: 4606197760
    - peak_rss_kib: 10476284
    - exit_code: 3
    - wall_ms: 20140
- links_to_findings: [002, 003, 004]
- issue_flag: true
- issue_labels: ["bug","memory"]
- notes: |
    Freeing Chrome cannot create a contiguous ~4.3 GiB allocation on top of an
    already-resident ~4.6–8 GiB Go buffer on this 15 GiB / 0-swap box.

--------------------------------------------------------------------------------

### Finding 006
- timestamp_pt: 2026-09-21 23:01:00 PT
- category: measurement
- severity: critical
- confidence: high
- summary: dmesg links the primary exit 137 to peasant anon-rss 11383776 kB, the same open, before any profile was written.
- details: |
    sudo dmesg: Mon Sep 21 11:30:13 2026
    Out of memory: Killed process 470758 (peasant) total-vm:19027544kB,
    anon-rss:11383776kB, file-rss:1604kB, shmem-rss:0kB
    README primary attempt: exit 137, sampled peak RSS 11313188 KiB, wall ~11324 ms,
    empty stdout/stderr, no push-profile.json. anon-rss is anonymous, not page cache.
    A different peasant PID 441717 was OOM-killed earlier the same morning at
    anon-rss 6213960 kB during other concurrent work; that is not the 007 push.
- evidence_paths:
    - /workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/README.md
    - /workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/Individual Sub-Agent Reports/007-2gb.md
- symbols:
    - village push --dry-run
- metrics:
    - db_bytes: 4606197760
    - peak_rss_kib: 11313188
    - exit_code: 137
    - wall_ms: 11324
- links_to_findings: [001, 004]
- issue_flag: true
- issue_labels: ["bug","memory","dry-run"]
- notes: |
    No profile file means the process died before the pipeline flush, which is
    consistent with failure inside openRunStore.

--------------------------------------------------------------------------------

### Finding 007
- timestamp_pt: 2026-09-21 23:03:00 PT
- category: blast_radius
- severity: high
- confidence: high
- summary: OpenReadOnly is used only for --dry-run store open on village push and harvest; a normal push calls file-backed store.Open.
- details: |
    openRunStore (readonly_run.go:40-50):
      if dryRun { return store.OpenReadOnly(path) }
      return store.Open(path)
    Callers of openRunStore: cmd_push.go:244 and cmd_harvest.go:496.
    Repo search shows no other OpenReadOnly call except readonly_run_test.go.
    store.Open (store.go:336) uses sqlitex.NewPool(dbPath) plus pragmas:
      journal_mode=WAL, mmap_size=268435456 (256 MiB), cache_size=-64000 (64 MiB).
    It does not ReadAll or Deserialize.
    Other --dry-run commands (ingest, pull, prune, models, redact, annotate,
    metrics, upgrade) do not go through openRunStore.
    Dry-run push still redacts and builds payloads AFTER open
    (pipeline.go:259-270 and pushSession dry-run return at pipeline.go:1019-1036).
    007 never reached that stage.
- evidence_paths:
    - /workspace/peasant-labs/polyrepo/peasant/develop/cmd/peasant/readonly_run.go:40-50
    - /workspace/peasant-labs/polyrepo/peasant/develop/cmd/peasant/cmd_harvest.go:496
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/store/store.go:275-282
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/store/store.go:336-370
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/push/pipeline.go:259-270
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/push/pipeline.go:1019-1036
- symbols:
    - openRunStore
    - store.Open
    - store.OpenReadOnly
    - push.Pipeline.pushSession
- metrics:
    - db_bytes: n/a
    - peak_rss_kib: n/a
    - exit_code: n/a
    - wall_ms: n/a
- links_to_findings: [001]
- issue_flag: true
- issue_labels: ["bug","dry-run","village-push"]
- notes: |
    007 harvest (not dry-run) succeeded, exit 0, peak RSS 8335152 KiB.
    That path is store.Open, which is why harvest could finish and dry-run push could not.

--------------------------------------------------------------------------------

### Finding 008
- timestamp_pt: 2026-09-21 23:05:36 PT
- category: repro
- severity: info
- confidence: high
- summary: A real local Village (Postgres 18 + MinIO + village server) was started on loopback; docker/podman and apt were not usable.
- details: |
    Documented full-stack path is docker compose / make backend-dev. docker and podman
    are not installed. apt-get against deb.debian.org failed (502 / InRelease no longer signed).
    dl.min.io returned 410. Workaround, all loopback, data under
    bench-runs/007-local-village/infra (007-2gb was not modified):
      PostgreSQL 18.6 portable binaries, port 55432, database peasant, trust auth
      MinIO RELEASE.2025-10-15T17-29-55Z via go install, 127.0.0.1:59000, bucket peasant-transcripts
      village server built from polyrepo/village/develop/backend (go 1.25.8), port 58080
      GET /health → {"status":"ok"} after migrations (23 tables)
      village-setup-demo --local minted demo-user; GET /api/v1/auth/me → 200
    Services were stopped after the measurements.
- evidence_paths:
    - /workspace/peasant-labs/polyrepo/village/develop/README.md
    - /workspace/peasant-labs/polyrepo/village/develop/scripts/encrypted-backend-dev.sh
    - /workspace/peasant-labs/bench-runs/007-local-village/infra/village.log
    - /workspace/peasant-labs/polyrepo/village/develop/backend/cmd/village-setup-demo/main.go
- symbols:
    - village-server
    - village-setup-demo
- metrics:
    - db_bytes: n/a
    - peak_rss_kib: n/a
    - exit_code: 0
    - wall_ms: n/a
- links_to_findings: [007]
- issue_flag: false
- issue_labels: []
- notes: |
    Not the compose stack. It is the real village HTTP API, real Postgres, and real S3 puts.

--------------------------------------------------------------------------------

### Finding 009
- timestamp_pt: 2026-09-21 23:06:23 PT
- category: repro
- severity: high
- confidence: high
- summary: Live village push of a copied 001 DB did real HTTP and did not deserialize; a receipt-URL bug caused the first publish 500s, and a retry updated 4 transcripts.
- details: |
    Sandbox copy, not the original bench-runs/001-9kb-e2e tree.
    First live push (FRONTEND_URL=http://127.0.0.1:58080):
      exit 0, wall 208 ms, "Pushing to http://127.0.0.1:58080 as @demo-user..."
      Summary: 0 new, 0 updated, 6 error(s) — 4 village 500s, 2 client no-model skips
      Server log: 4x POST /api/v1/transcripts/publish 500, then annotations 200
      Postgres afterwards: 4 transcripts; MinIO: 4 ciphertext objects
      500 body: "Village persisted the publication but could not construct a complete authoritative receipt"
      Cause: schema.validatePublicationURL requires https. FRONTEND_URL was http.
      This is unrelated to sqlite deserialize.
    Village restarted with FRONTEND_URL=https://127.0.0.1:58080 (API stayed http).
    Retry: exit 0, wall 162 ms, peak RSS 58508 KiB, no deserialize string,
      Summary: 0 new, 4 updated, 2 error(s) (the same no-model skips),
      4x POST /api/v1/transcripts/publish 200.
    Original 007 peasant.db was not opened by these runs (still 4606197760 bytes, no -wal/-shm).
- evidence_paths:
    - /workspace/peasant-labs/bench-runs/007-local-village/001-9kb-e2e/profiles/live/push.stdout
    - /workspace/peasant-labs/bench-runs/007-local-village/001-9kb-e2e/profiles/live-retry/push.meta
    - /workspace/peasant-labs/polyrepo/schema/develop/publication_contract.go:748-755
    - /workspace/peasant-labs/polyrepo/village/develop/backend/internal/handler/transcripts.go:695-696
- symbols:
    - village push
    - schema.validatePublicationURL
- metrics:
    - db_bytes: 749568
    - peak_rss_kib: 58508
    - exit_code: 0
    - wall_ms: 162
- links_to_findings: [007, 008]
- issue_flag: true
- issue_labels: ["dry-run","village-push"]
- notes: |
    Proves the non-dry-run command reaches the network and a file-backed store.
    The https receipt rule is a separate local-setup footgun, not the memory bug.

--------------------------------------------------------------------------------

### Finding 010
- timestamp_pt: 2026-09-21 23:16:00 PT
- category: measurement
- severity: critical
- confidence: high
- summary: On copied 003 and 004 DBs, live push peak RSS stayed below dry-run, made real HTTP, and never hit deserialize; 004 live opened the DB as a WAL file.
- details: |
    Copies under bench-runs/007-local-village. Original bench DBs were not used for writes.
    VmHWM sampled every 50 ms. concurrency 1. Profiles were written (open succeeded).

    003-10mb DB 51372032 bytes:
      dry-run  exit 0  wall 24772 ms  peak 251136 KiB  HTTP posts 0
               stdout: "Dry run summary: 8 would push, 0 unchanged (0 errors — no HTTP calls made)"
      live     exit 0  wall 28789 ms  peak 209012 KiB  8x POST publish 422
               Summary: 0 new, 0 updated, 59 error(s) — client no-model plus server redaction scan
               422 example: Slack token "xoxb-SYNTHETIC-TOKEN-NOT-REAL-..." (synthetic corpus)
      dry minus live = 42124 KiB on a 49 MiB file. Both peaks are dominated by
      process baseline plus redaction, so the 2x open cost is hidden at this size.

    004-100mb DB 386048000 bytes dry / 386056192 bytes after live (+8192, a write):
      dry-run  exit 0  wall 197887 ms  peak 1364868 KiB (~1.30 GiB)  HTTP posts 0
               "Dry run summary: 16 would push, 0 unchanged (0 errors — no HTTP calls made)"
      live     exit 0  wall 215744 ms  peak 751480 KiB (~734 MiB)  16x POST publish 422
               Summary: 0 new, 0 updated, 108 error(s)
               While the live process was running, peasant.db-wal (97 KiB) and
               peasant.db-shm (32 KiB) existed. They were gone after clean close.
               Live peak was 613388 KiB below dry-run on the same copy.
               734 MiB live peak is under 2x the 368 MiB file; dry-run peak is 3.62x the file
               because deserialize plus redaction stack.

    Neither stderr contained "deserialize". 007 was not live-pushed: MemAvailable
    during these runs was ~8.2–8.6 GiB, and the historical 005 dry-run already
    peaked at 4458952 KiB. A 4.29 GiB live push was not memory-safe to attempt.
- evidence_paths:
    - /workspace/peasant-labs/bench-runs/007-local-village/003-10mb/profiles/dry/push.meta
    - /workspace/peasant-labs/bench-runs/007-local-village/003-10mb/profiles/live/push.meta
    - /workspace/peasant-labs/bench-runs/007-local-village/004-100mb/profiles/dry/push.meta
    - /workspace/peasant-labs/bench-runs/007-local-village/004-100mb/profiles/live/push.meta
    - /workspace/peasant-labs/bench-runs/007-local-village/004-100mb/profiles/dry/push.stdout
    - /workspace/peasant-labs/bench-runs/007-local-village/003-10mb/profiles/live/push.stdout
- symbols:
    - store.Open
    - store.OpenReadOnly
- metrics:
    - db_bytes: 386048000
    - peak_rss_kib: 751480
    - exit_code: 0
    - wall_ms: 215744
- links_to_findings: [007, 009]
- issue_flag: true
- issue_labels: ["bug","memory","dry-run","village-push"]
- notes: |
    422s are the Village server redaction scanner rejecting synthetic tokens.
    They happen after store open and after HTTP. They are not the 007 failure mode.

--------------------------------------------------------------------------------

### Finding 011
- timestamp_pt: 2026-09-21 23:18:00 PT
- category: hypothesis
- severity: medium
- confidence: high
- summary: The 007 OOM/deserialize failure is dry-run-open-specific; a live push would not take this path, but redaction of a 2 GiB corpus was not re-measured and can still be large.
- details: |
    Code: live push never calls Deserialize. Experiment: 001/003/004 live opens
    succeeded and performed HTTP with lower or similar RSS versus dry-run, and
    004 demonstrably used a WAL sidecar during the run.
    Not shown: that a live push of the 4.29 GiB 007 DB would finish. 005-500mb
    dry-run (which includes redaction after a successful OpenReadOnly of a
    1355591680-byte DB) peaked at 4458952 KiB. Live 007 would skip the deserialize
    spike and still run that redaction/payload path. That is a separate cost.
    This research did not re-run 007 dry-run and did not open the original 007 DB.
- evidence_paths:
    - /workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/Individual Sub-Agent Reports/005-500mb.md
    - /workspace/peasant-labs/polyrepo/peasant/develop/cmd/peasant/readonly_run.go:40-50
- symbols:
    - store.Open
    - push.Pipeline
- metrics:
    - db_bytes: 4606197760
    - peak_rss_kib: n/a
    - exit_code: n/a
    - wall_ms: n/a
- links_to_findings: [007, 010]
- issue_flag: false
- issue_labels: ["memory"]
- notes: |
    Do not file the redaction CPU/RSS cost as this same bug.

--------------------------------------------------------------------------------

### Finding 012
- timestamp_pt: 2026-09-21 23:18:30 PT
- category: other
- severity: low
- confidence: medium
- summary: Design-level fixes, not implemented: immutable file open, temp snapshot, or a fail-fast size guard.
- details: |
    Ranked for dry-run semantics (no source writes, no WAL/SHM on the real DB):
    1. Open the checkpointed file with SQLite immutable=1 (or equivalent) plus
       query_only, so pages come from the file/page cache instead of a private
       malloc of the whole file. Must be tested to prove no -wal/-shm appears.
       This is the only option that removes the 2x RAM spike and still answers queries.
    2. sqlite backup/VACUUM INTO a temp copy and open that file read-only.
       Costs 1x disk and bounded RAM. Slower, but does not depend on immutable semantics.
    3. Fail fast when file size (or 2x size) exceeds a budget, before ReadAll,
       with an error that names the dry-run memory behavior. Safe, does not make
       multi-GiB dry-run work.
    4. Stop using zombiezen Deserialize's malloc-and-copy. Still leaves a 1x Go
       or sqlite image of a 4.29 GiB DB, which plus redaction is still hostile
       on a 15 GiB host.
    5. Do not "fix" this by raising GOMEMLIMIT or closing other processes.
- evidence_paths:
    - /workspace/peasant-labs/polyrepo/peasant/develop/internal/store/readonly.go:17-32
- symbols:
    - store.OpenReadOnly
- metrics:
    - db_bytes: n/a
    - peak_rss_kib: n/a
    - exit_code: n/a
    - wall_ms: n/a
- links_to_findings: [001, 007]
- issue_flag: false
- issue_labels: []
- notes: |
    Not implemented. Research-only.

--------------------------------------------------------------------------------

### Root Cause Statement (fill when ready)
- statement: |
    village push --dry-run (and harvest --dry-run) open the analytics database
    through store.OpenReadOnly. That function io.ReadAlls the whole checkpointed
    SQLite file into a Go slice and then zombiezen Conn.Deserialize mallocs a
    second buffer of len(data) via sqlite3_malloc64 before copying into a private
    memory database. The copy exists so dry-run can flip the WAL header on a
    private image and avoid creating -wal/-shm. Peak anonymous memory is therefore
    on the order of 2x the file (about 4.63 GiB retained Go capacity plus a 4.29 GiB
    sqlite allocation for the 007 DB) and higher while the previous slice growth
    buffer is still live. On 007-2gb that allocation fails or is OOM-killed before
    redaction or any Village I/O. A normal village push calls store.Open on the
    file, with a 256 MiB mmap cap and a 64 MiB cache, and does not deserialize.
    Local Village runs of copied 001/003/004 databases confirmed live push reaches
    HTTP without the deserialize error, at a lower peak RSS than dry-run on 003
    and 004. Freeing Chrome cannot supply the second multi-GiB allocation.
- primary_symbol: store.OpenReadOnly
- primary_path: internal/store/readonly.go:22-50
- mechanism: |
    io.ReadAll retains a ~1.08x slice (4.634 GiB for this file). Conn.Deserialize
    then sqlite3_malloc64(len) for a second full image and copies into it. The
    error string is returned when that malloc returns NULL. The Go slice is still
    referenced, so both images are live at the failure point. Go's last 1.25x
    growth also overlaps an old ~3.71 GiB buffer with the new 4.63 GiB buffer.
- why_cleanup_was_insufficient: |
    The post-cleanup retry started with ~10.5 GiB MemAvailable and died at
    10476284 KiB RSS with sqlite3_malloc64 failure. The first image alone is
    ~4.6 GiB of Go heap, the growth overlap is ~8.3 GiB, and the second malloc
    asks for another 4.29 GiB in one shot. CommitLimit on this 0-swap host is
    only ~7.82 GiB (overcommit ratio 50, mode 0), and Committed_AS was already
    above that before the process started. Hundreds of MiB freed by closing
    Chrome tabs are not the missing 4-plus GiB contiguous allocation.
- disproved_alternatives:
    - "Redaction OOMs 007 at the start of dry-run" — stderr is the deserialize malloc error, no profile was written, and redaction runs only after openRunStore returns.
    - "The box was simply short of RAM because of Chrome" — the same exit 3 reproduced with ~10.5 GiB MemAvailable; RSS at failure was already ~10 GiB from the first materialization.
    - "Live village push uses the same OpenReadOnly path" — openRunStore uses store.Open unless --dry-run. Local HTTP pushes of 001/003/004 did not deserialize; 004 created a WAL sidecar while running and wrote 8192 bytes to the copied DB.
    - "A second Go ReadAll is the duplicate" — the duplicate is sqlite3_malloc64 inside zombiezen Deserialize. The file is hashed by a streaming re-read, not a second retained buffer.

--------------------------------------------------------------------------------

### Draft GitHub Issue
- title: dry-run OpenReadOnly double-copies the SQLite DB and OOMs multi-GiB stores
- body: |
    ## Summary
    `village push --dry-run` and `harvest --dry-run` open the analytics database
    with `store.OpenReadOnly`. That reads the entire checkpointed SQLite file into
    a Go slice and then `sqlite3_malloc64`s a second buffer of the same length
    into a private memory database, so dry-run never creates `-wal`/`-shm`.
    Peak RAM is on the order of 2× the file before redaction or any Village HTTP.
    A ~4.29 GiB `peasant.db` OOM-kills or fails deserialize on a 15 GiB / 0-swap
    host. A normal (non-dry-run) push uses file-backed `store.Open` and does not.

    ## Evidence
    - Code: `internal/store/readonly.go` `OpenReadOnly` / `readCheckpointedDatabase`
      (`io.ReadAll`, then `conn.Deserialize`). `cmd/peasant/readonly_run.go`
      `openRunStore` selects it only when `dryRun` is set. Callers: `village push`
      and `harvest`.
    - Driver: `zombiezen.com/go/sqlite` `Conn.Deserialize` mallocs `len(data)` and
      returns `sqlite: deserialize to "main": memory allocation failure` when that
      malloc returns NULL. That string is the 007 stderr.
    - Bench `007-2gb`: file `4606197760` bytes. Primary `village push --dry-run`
      exit 137, sampled peak RSS `11313188` KiB, dmesg `anon-rss:11383776kB`,
      `total-vm:19027544kB`, no profile. Post-cleanup retry exit 3, wall 20140 ms,
      peak RSS `10476284` KiB, same stderr, `MemAvailable` ~10.5 GiB at start.
    - Mid-size dry-runs fit: `004-100mb` historical peak ~1.48 GiB; `005-500mb`
      peak `4458952` KiB. `007` harvest (file-backed `store.Open`, not dry-run)
      exited 0.
    - Local Village check (loopback Postgres + MinIO + `village` server, copied
      sandboxes, original `007-2gb` DB untouched): live push did not deserialize.
      `001` retry published 4 updates (`POST /api/v1/transcripts/publish` 200).
      `004` live peak RSS `751480` KiB versus dry-run `1364868` KiB on a
      `386048000`-byte DB, with `-wal`/`-shm` present during the live process
      and 16 real publish requests. `003` showed the same shape at smaller scale.

    ## Suspected root cause
    Dry-run isolation is implemented as a full-file deserialize plus a second
    SQLite-owned copy. That is independent of redaction cost and of leftover
    browser RAM. Live push does not use this open.

    ## Reproduction
    1. Harvest a synthetic corpus until `peasant.db` is multi-GiB and checkpointed
       (no `-wal`/`-shm`). The existing `bench-runs/007-2gb` database is 4606197760 bytes.
    2. On a ~16 GiB host with no swap:
       `peasant --data-dir <sandbox> village push --dry-run --non-interactive --yes`
    3. Observe exit 137 (OOM) or exit 3 with
       `sqlite: deserialize to "main": memory allocation failure`, and no profile.
    4. Contrast: the same binary without `--dry-run`, pointed at a local Village,
       opens the file (WAL sidecars appear) and issues HTTP. Use a small copy first.

    ## Acceptance criteria
    - Dry-run open of an N-byte checkpointed database must not require peak RSS
      on the order of 2N before redaction or payload work.
    - A checkpointed multi-GiB database must open for dry-run without
      `sqlite3_malloc64` of the whole file, or the command must fail fast with an
      error that names the limit before that allocation.
    - Dry-run must not create or modify the source database's `-wal`, `-shm`, or
      `-journal` files (keep today's no-sidecar guarantee unless a replacement
      policy is explicit).
    - `village push` without `--dry-run` stays on file-backed `store.Open`.
    - Help text or docs state dry-run memory behavior for large stores.
- suggested_labels: ["bug","performance","memory","village-push","dry-run"]
- suggested_assignees: []
- blocked_by: []
- related_docs:
    - /workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/README.md
    - /workspace/peasant-labs/2026-09-21 Village Upload Pre-Optimization Benchmarking/research/GROK-REPORT-bigger-issue.md

================================================================================
END OF TEMPLATE
================================================================================
