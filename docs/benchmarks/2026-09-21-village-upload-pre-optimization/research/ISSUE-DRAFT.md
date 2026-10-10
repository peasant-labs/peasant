# dry-run OpenReadOnly double-copies the SQLite DB and OOMs multi-GiB stores

## Summary

`village push --dry-run` and `harvest --dry-run` open the analytics database with `store.OpenReadOnly`. That reads the entire checkpointed SQLite file into a Go slice and then `sqlite3_malloc64`s a second buffer of the same length into a private memory database, so dry-run never creates `-wal`/`-shm`. Peak RAM is on the order of 2× the file before redaction or any Village HTTP. A ~4.29 GiB `peasant.db` OOM-kills or fails deserialize on a 15 GiB / 0-swap host. A normal (non-dry-run) push uses file-backed `store.Open` and does not.

## Evidence

- Code: `internal/store/readonly.go` `OpenReadOnly` / `readCheckpointedDatabase` (`io.ReadAll`, then `conn.Deserialize`). `cmd/peasant/readonly_run.go` `openRunStore` selects it only when `dryRun` is set. Callers: `village push` and `harvest`.
- Driver: `zombiezen.com/go/sqlite` `Conn.Deserialize` mallocs `len(data)` and returns `sqlite: deserialize to "main": memory allocation failure` when that malloc returns NULL. That string is the 007 stderr.
- Bench `007-2gb`: file `4606197760` bytes. Primary `village push --dry-run` exit 137, sampled peak RSS `11313188` KiB, dmesg `anon-rss:11383776kB`, `total-vm:19027544kB`, no profile. Post-cleanup retry exit 3, wall 20140 ms, peak RSS `10476284` KiB, same stderr, `MemAvailable` ~10.5 GiB at start.
- Mid-size dry-runs fit: `004-100mb` historical peak ~1.48 GiB; `005-500mb` peak `4458952` KiB. `007` harvest (file-backed `store.Open`, not dry-run) exited 0.
- Local Village check (loopback Postgres + MinIO + `village` server, copied sandboxes, original `007-2gb` DB untouched): live push did not deserialize. `001` retry published 4 updates (`POST /api/v1/transcripts/publish` 200). `004` live peak RSS `751480` KiB versus dry-run `1364868` KiB on a `386048000`-byte DB, with `-wal`/`-shm` present during the live process and 16 real publish requests. `003` showed the same shape at smaller scale.

## Suspected root cause

Dry-run isolation is implemented as a full-file deserialize plus a second SQLite-owned copy. That is independent of redaction cost and of leftover browser RAM. Live push does not use this open.

## Reproduction

1. Harvest a synthetic corpus until `peasant.db` is multi-GiB and checkpointed (no `-wal`/`-shm`). The existing `bench-runs/007-2gb` database is 4606197760 bytes.
2. On a ~16 GiB host with no swap: `peasant --data-dir <sandbox> village push --dry-run --non-interactive --yes`
3. Observe exit 137 (OOM) or exit 3 with `sqlite: deserialize to "main": memory allocation failure`, and no profile.
4. Contrast: the same binary without `--dry-run`, pointed at a local Village, opens the file (WAL sidecars appear) and issues HTTP. Use a small copy first.

## Acceptance criteria

- Dry-run open of an N-byte checkpointed database must not require peak RSS on the order of 2N before redaction or payload work.
- A checkpointed multi-GiB database must open for dry-run without `sqlite3_malloc64` of the whole file, or the command must fail fast with an error that names the limit before that allocation.
- Dry-run must not create or modify the source database's `-wal`, `-shm`, or `-journal` files (keep today's no-sidecar guarantee unless a replacement policy is explicit).
- `village push` without `--dry-run` stays on file-backed `store.Open`.
- Help text or docs state dry-run memory behavior for large stores.

## Labels

`bug`, `performance`, `memory`, `village-push`, `dry-run`

## Pointers

| Symbol | Location |
| --- | --- |
| `store.OpenReadOnly` | `polyrepo/peasant/develop/internal/store/readonly.go:22-86` |
| `store.readCheckpointedDatabase` | `internal/store/readonly.go:88-138` (`io.ReadAll` at line 111) |
| `openRunStore` | `cmd/peasant/readonly_run.go:40-50` |
| `village push` call | `cmd/peasant/cmd_push.go:244` |
| `harvest` call | `cmd/peasant/cmd_harvest.go:496` |
| `store.Open` (live path) | `internal/store/store.go:336` (mmap 256 MiB, cache 64 MiB, WAL) |
| `sqlite.Conn.Deserialize` malloc | `zombiezen.com/go/sqlite@v1.4.2/sqlite.go:621-641` |
| Measured 007 failure | `bench-runs/007-2gb/profiles/push.stderr`, `push.meta` |
| Local Village contrast | `bench-runs/007-local-village/` |
