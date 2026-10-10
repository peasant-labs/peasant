# The bigger issue: dry-run open double-copies the analytics database

**Date:** 2026-09-21 (PT)  
**Repo:** `/workspace/peasant-labs/polyrepo/peasant/develop` (binary `bin/peasant`, version `profile-box`)  
**Bench:** `/workspace/peasant-labs/bench-runs/007-2gb`  
**Status:** Root cause confirmed. The failure is the dry-run open path, not leftover Chrome RAM, and not something a live Village push does at open.

Supporting log: [FINDINGS-LOG.md](FINDINGS-LOG.md). Issue text: [ISSUE-DRAFT.md](ISSUE-DRAFT.md).

## Executive summary

`village push --dry-run` dies on the ~4.29 GiB `007-2gb` database while opening the store, before redaction and before any HTTP. `store.OpenReadOnly` reads the whole SQLite file into a Go slice (`io.ReadAll`) and then asks SQLite to malloc a second buffer of that same length (`sqlite3_malloc64` inside `zombiezen.com/go/sqlite` `Conn.Deserialize`). The Go slice is still live when that malloc runs. For this file the retained slice capacity is 4.63 GiB and the sqlite allocation is another 4.29 GiB (8.92 GiB together). The last slice growth also overlaps an older 3.71 GiB buffer with the new one (~8.34 GiB) before that second malloc.

That matches the two observed failures on a 15.6 GiB host with **no swap**:

| Attempt | Exit | Peak anonymous memory | Profile written |
| --- | --- | --- | --- |
| Primary dry-run | 137 (kernel OOM) | dmesg `anon-rss:11383776kB`; sampler `11313188` KiB | no |
| Post-cleanup retry (`MemAvailable` ~10.5 GiB at start) | 3 | `10476284` KiB | no |

The retry stderr is exactly the driver error for a failed `sqlite3_malloc64`:

```text
sqlite: deserialize to "main": memory allocation failure
```

Closing Chrome could not fix this. The process already held roughly 10 GiB when the second multi-GiB allocation was refused. `CommitLimit` on this box is only ~7.82 GiB (`overcommit_memory=0`, ratio 50, zero swap).

A normal push does **not** use this function. `openRunStore` calls `store.Open` on the file unless `--dry-run` is set. A local Village (real Postgres, MinIO, and the village HTTP server) accepted live pushes of copied small and mid-size databases. Those runs issued real `POST /api/v1/transcripts/publish` requests, did not print the deserialize error, and on the 368 MiB database peaked at 734 MiB versus 1.30 GiB for dry-run of the same copy. The 4.29 GiB database was not live-pushed: getting past open would still run redaction, and the 500 MiB set already peaked at ~4.25 GiB in dry-run. That remaining cost is a different problem.

## Call chain

Dry-run:

```text
peasant village push --dry-run
  cmd/peasant/cmd_push.go          load credentials, then open the store
  cmd/peasant/readonly_run.go      openRunStore(cmd, dryRun=true)
  internal/store/readonly.go       OpenReadOnly
    readCheckpointedDatabase       io.ReadAll of peasant.db
                                   set header bytes 18–19 to rollback on the copy only
    sqlitex.NewPool                file:peasant-readonly?mode=memory&cache=private
    sqlite.Conn.Deserialize       sqlite3_malloc64(len) + copy into that buffer
  (never reached) internal/push    redaction, payload build, HTTP skipped entirely on 007
```

Live push:

```text
peasant village push                 (no --dry-run)
  openRunStore(cmd, dryRun=false)
  internal/store/store.go Open       sqlitex.NewPool(dbPath)
    PRAGMA journal_mode=WAL
    PRAGMA mmap_size=268435456       256 MiB
    PRAGMA cache_size=-64000         64 MiB
  internal/push.Pipeline             same preflight as dry-run, then real HTTP
```

`openRunStore` is only called from `village push` (`cmd_push.go:244`) and `harvest` (`cmd_harvest.go:496`). A repository search finds no other `OpenReadOnly` production caller. Other `--dry-run` flags (ingest, pull, prune, models, redact, annotate, metrics, upgrade) do not use this open.

The comment on `OpenReadOnly` says why the copy exists: even `mode=ro` can create or update WAL/SHM files. Deserialize cannot load a WAL-mode image, so the code changes bytes 18 and 19 on the private buffer only (the source file is never written). `requireCheckpointedDatabase` refuses the open if `-wal`, `-shm`, or `-journal` is present.

## Root cause

Three stacked facts, all required:

1. **The whole file is retained.** `readCheckpointedDatabase` uses `io.ReadAll`. Go 1.25 grows that slice by ~1.25× once it is large. For `4606197760` bytes the final capacity is `4975769013` bytes (4.634 GiB). A second streaming read only feeds SHA-256; it does not keep another copy. The duplicate is not a second `ReadAll`.

2. **SQLite allocates the same length again.** `zombiezen.com/go/sqlite@v1.4.2` `Conn.Deserialize` (`sqlite.go:631-636`) does `sqlite3_malloc64(len(data))`. If that returns NULL it returns `sqlite: deserialize to "main": memory allocation failure` **before** the copy. On success it copies into that C buffer and hands it to `sqlite3_deserialize` with `FREEONCLOSE|RESIZEABLE`. `modernc.org/libc` `GoBytes` is a view of the C memory, not a third Go buffer. The Go slice stays reachable until `OpenReadOnly` returns, so both images exist at once. Floor for this file: 4.634 + 4.290 = **8.92 GiB**, plus whatever of the previous 3.71 GiB growth buffer the runtime has not returned.

3. **The host cannot grant that second chunk.** Primary kill, 2026-09-21 11:30:13 local, process 470758: `total-vm:19027544kB`, `anon-rss:11383776kB`, `file-rss:1604kB`. Anonymous RSS, not file cache. The soft failure is the malloc returning NULL after the process had already reached `10476284` KiB. `MemAvailable` of ~10.5 GiB was measured **before** the retry. `overcommit_memory` is 0 and `CommitLimit` is `8198808` kB. `ReadAll` gets there in ~25% steps. One 4.29 GiB `sqlite3_malloc64` is a different request, and it fails.

Redaction is not what kills 007. `push.Pipeline` redacts only after `openRunStore` returns (`pipeline.go` dry-run loop around lines 259–270; per-session dry-run return around lines 1019–1036). No `push-profile.json` was written. Sets whose databases fit — `004` at 368 MiB, `005` at 1.26 GiB — complete dry-run, and on those runs redaction dominates CPU. That remains a separate performance issue.

`007` harvest succeeded (exit 0, peak RSS `8335152` KiB) because harvest without `--dry-run` uses `store.Open`, not `OpenReadOnly`.

## Evidence

### Existing 007 artifacts (not re-run)

- Database: `/workspace/peasant-labs/bench-runs/007-2gb/data-home/peasant/peasant.db`, `4606197760` bytes, no sidecar files. Still that size and still sidecar-free after this research. It was not opened.
- Post-cleanup meta: exit 3, wall `20140` ms, peak RSS `10476284` KiB. Stderr quoted above.
- Primary numbers from the campaign README and `Individual Sub-Agent Reports/007-2gb.md`: exit 137, peak `11313188` KiB, dmesg anon-rss `11383776` kB.
- `005-500mb` dry-run completed at peak `4458952` KiB on a `1355591680`-byte database. `004-100mb` historical dry-run peak `1554336` KiB.

### Local Village, this session

Docker and podman are not installed. `apt-get` against `deb.debian.org` failed (502 / unsigned InRelease). `dl.min.io` returned 410. The documented `make backend-dev` path could not be used as written.

What ran instead, all on loopback, under `bench-runs/007-local-village/` (stopped afterward):

- PostgreSQL 18.6 portable build, port `55432`, database `peasant`
- MinIO `RELEASE.2025-10-15T17-29-55Z`, `127.0.0.1:59000`, bucket `peasant-transcripts`
- `village` server built from `polyrepo/village/develop/backend`, port `58080`
- `GET /health` → `{"status":"ok"}` after migrations
- `village-setup-demo --local` minted `demo-user`; `GET /api/v1/auth/me` returned 200

Pushes used **copies** of bench databases plus the demo credentials. Original `001`–`007` trees were not the write targets.

| Run | DB bytes | Exit | Wall | Peak RSS | HTTP publish |
| --- | --- | --- | --- | --- | --- |
| 001 live, first try | 749568 | 0 | 208 ms | 54320 KiB (100 ms sample) | 4× **500** |
| 001 live, retry | 749568 | 0 | 162 ms | 58508 KiB | 4× **200** (summary: 0 new, 4 updated, 2 no-model errors) |
| 003 dry-run | 51372032 | 0 | 24772 ms | 251136 KiB | none ("8 would push, 0 unchanged") |
| 003 live | 51372032 | 0 | 28789 ms | 209012 KiB | 8× **422** |
| 004 dry-run | 386048000 | 0 | 197887 ms | 1364868 KiB (~1.30 GiB) | none ("16 would push, 0 unchanged") |
| 004 live | 386056192 after the run | 0 | 215744 ms | 751480 KiB (~734 MiB) | 16× **422** |

`004` live created `peasant.db-wal` and `peasant.db-shm` while the process was running and grew the copied file by 8192 bytes. That is file-backed `store.Open`, not a memory deserialize. Dry-run of the same copy left no sidecar and made no HTTP. Neither stderr contained `deserialize`.

Village log totals for the session: 4× publish 200, 4× publish 500, 24× publish 422.

The 500s and 422s are real HTTP failures **after** a successful open:

- **500**, first 001 attempt only. The server persisted 4 transcript rows and 4 MinIO objects, then `AuthoritativePublishResponse.Validate` failed. `validatePublicationURL` requires an `https` URL (`schema/publication_contract.go`). `FRONTEND_URL` had been set to `http://127.0.0.1:58080`. Restarting the server with `FRONTEND_URL=https://127.0.0.1:58080` (the API itself stayed `http`) made the retry return 200 and "4 updated". This is a local receipt-URL rule, not the memory bug.
- **422**, 003 and 004. The village redaction scanner rejected synthetic slack tokens (`xoxb-SYNTHETIC-TOKEN-NOT-REAL-...`). Annotations on 003 did publish (74 created). The client still got past store open and onto the network.

007 was not pushed live. `MemAvailable` during the 003/004 runs was about 8.2–8.6 GiB. A dry-run of the 500 MiB set had already peaked near 4.25 GiB, and that figure includes both open and redaction. Skipping deserialize would remove the 007 open spike; it would not make a 2 GiB redaction pass obviously safe. No such run was started.

## Harness versus a real Village

The pre-optimization campaign ran **only** `--dry-run`, so it only ever exercised `OpenReadOnly`. That was the right no-network rule for benchmarking, and it is also why every 007 attempt died in a code path production push does not use.

| Question | Answer |
| --- | --- |
| Is the OOM / deserialize error caused by the dry-run open? | Yes. Same function, same error string, failure before profile flush. |
| Does live push open the same way? | No. `store.Open` on the file, WAL, 256 MiB mmap, 64 MiB cache. |
| Did a live local push hit the same wall? | No, on copied 001, 003, and 004. Lower peak than dry-run on 003 and 004, real HTTP, WAL sidecar on 004. |
| Did live push fail for other reasons? | Yes, after open: receipt URL must be https (fixed in the local server env, then 001 updates succeeded), and the server scanner 422s synthetic secrets in 003/004. Two client sessions in 001 have no model and are skipped before upload. |
| Would live 007 be cheap? | Not established. Open should be cheap relative to 2×4.29 GiB. Redaction and payload build still scale with corpus size. Not measured here on purpose. |

So the campaign's "bigger issue" is real, and it is specific to the dry-run (and harvest dry-run) store open. It is not an artifact of a stub that pretends to talk to Village. The stub **is** the thing that materializes the database. Pointing the same binary at a real local Village removes that open, and the memory cliff at open goes away with it.

## Recommendations

Not implemented.

1. **Best fit for today's dry-run rules:** open the checkpointed file with SQLite's `immutable=1` (and `query_only`) instead of deserializing it. Pages stay in the file cache. The source should still gain no `-wal`/`-shm`. This needs a test that a checkpointed WAL-header database with no sidecar opens, and that the open creates nothing beside the file. The header tweak at bytes 18–19 exists only because Deserialize cannot read a WAL image; a real SQLite open does not need that private rewrite.
2. **If immutable open cannot guarantee zero sidecars:** `VACUUM INTO` or backup into a temp file, then open that file read-only. One extra copy on disk, bounded RAM, source untouched.
3. **Minimum safe change:** if `2 * fileSize` (or the measured Go capacity plus `len`) exceeds a budget, return a clear error **before** `ReadAll`. This does not make multi-GiB dry-run work. It stops the OOM and the opaque malloc error.
4. **Not sufficient:** avoiding zombiezen's extra malloc but still holding one full image of a 4.29 GiB database, or telling operators to raise `GOMEMLIMIT` / close browsers.
5. **Leave live push on `store.Open`.** Do not route it through `OpenReadOnly` for symmetry.

Redaction of large corpora should stay a separate ticket. It dominates successful mid-size dry-runs and will dominate a live push once open is fixed.

## Open questions

- Does `immutable=1` on a checkpointed database whose header still says WAL (bytes 18–19 are 2) open cleanly with no sidecar? The current code refuses to find out and rewrites a private copy instead. That is the experiment a fix should run, on a copy, not on `007-2gb`.
- After a successful `OpenReadOnly` of a mid-size file, how fast does Go return the `ReadAll` buffer to the OS? The 004 dry-versus-live gap (1.30 GiB vs 734 MiB on a 368 MiB file, 599 MiB) is larger than one leftover copy and smaller than two full copies plus redaction. A heap profile at open-return would split "sqlite image still held" from "redaction".
- Live 007 RSS after a non-deserialize open, through redaction only, is unmeasured. Do not infer it from the 005 dry-run peak; that peak includes `OpenReadOnly` of a 1.26 GiB file.
- The https receipt check will 500 a persisted publish whenever `FRONTEND_URL` is not https, including `http://127.0.0.1`. Worth a docs note for local Village, not part of this memory bug.

## What was not done

- No second dry-run of `007-2gb`, and no live push of it.
- No change to peasant or village source.
- Original `bench-runs/007-2gb/data-home` was not modified.
- Local Postgres, MinIO, and the village server were stopped at the end of the session.
