# Session-scoped harvest and open timing

Measured on 2026-11-09 at revision
`74fc97a100a3a4c77bd782633a17282582de056f` (clean worktree), using the pinned
Nix development shell (Go 1.26.3). This revision is the integration branch tip
after the final teammate-link changes; the numbers below were re-taken at it,
not copied from the earlier sample at `778c1c131` (see **What changed since the
previous measurement**).

This is the landing record for the session-scoped `harvest --session` and
`open --session` cost. Three requirements apply, all on a sandbox copy of the
real store:

- `peasant harvest --session X` for a stored, unchanged session: **median under
  1 s**.
- `peasant open --session X`: **every run under 2 s end to end**.
- The time must not depend on the size of the store.

Both commands are measured against the pre-change binary on the same data, and
the live store is never written.

## Build and provenance

- `make build` at the revision above completed; it runs the web export, the
  provider-build-provenance check, and the CLI build with the development
  version injected from `scripts/dev-version.sh`.
- The served binary is `$SB/bin/peasant`, copied from the worktree's
  `./peasant` (`$SB` is a local sandbox directory under `/tmp`). It reports:

  ```text
  $ $SB/bin/peasant version
  peasant v0.14.1-dev.153+74fc97a10
  ```

- The revision is carried by the binary itself, not assumed from the worktree:

  ```text
  $ git -C <worktree> rev-parse --short=9 HEAD
  74fc97a10
  $ grep -c 74fc97a10 $SB/bin/peasant
  3
  $ grep -c 778c1c131 $SB/bin/peasant
  0
  $ go version -m $SB/bin/peasant | grep ldflags
  	build	-ldflags="-X github.com/peasant-labs/peasant/internal/defaults.version=v0.14.1-dev.153+74fc97a10"
  $ sha256sum $SB/bin/peasant
  9a5c7437ef46b3dbdd3bf83f75d6da12fa9330ec21e95fa203aeb8d97f158ce7
  ```

  The `grep` of the previous measured revision returning zero is the direct
  evidence that the binary under test is the new tip rather than the earlier
  one. `make build` also reported that the web export and `./peasant` carry the
  same provider-build provenance hash.

- For the delta below, the previous measured build (`778c1c131`) was rebuilt
  from an archive into `$SB/bin/peasant-oldtip` and run against the same store
  state, so a build effect can be separated from the store's accumulated state.

- The pre-change binary was built from the change's base commit,
  `2f6e41d3626dc56b40949cc85349dcbe7d195f51` (`develop` at the merge base), by
  `git archive` into a scratch tree and `go build` with a distinct injected
  version, so the two binaries are never confused:

  ```text
  peasant v0.14.0-prechange.2f6e41d36
  ```

## Sandbox

### The live store was never written

The live store is the local Peasant data directory's `peasant.db`:

- 42,989,264,896 bytes (40 GiB), 10,495,426 pages, 2,692,048 free pages.
- 20,615 sessions, 11,736 `claude_transcript_evidence` rows.
- Schema version 69 (the derived lookup tables this change adds are not
  present in the live store yet).

Every read of it used `file:...?mode=ro` or a read-only `ATTACH`. Its data file
still carries its pre-measurement modification time (`2026-11-09 02:55`) and the
write-ahead log its own (`02:59`) after the whole run. The live Claude Code
transcript root was only ever read.

### The copy is a session sample, and why

A whole-store copy did not fit reliably while this ran: free space on the host
moved between 11 GB and 79 GB as other workloads started and finished, while
the store's live data pages are about 31 GB and `VACUUM INTO` needs that much
free at once. A `VACUUM INTO` was started and abandoned for that reason.

The sandbox is therefore a **session sample** of the live store, built by
`ATTACH`ing the live database read-only and copying:

- the schema, minus nothing;
- every row of every table for the 3,002 most recently started sessions, plus
  every ancestor those sessions name;
- every global table in full, including the complete 11,736-row
  `claude_transcript_evidence` table (the table a lost scope would scan);
- `projects` and `host_slugs` for exactly the sampled sessions, so the copy
  passes `PRAGMA foreign_key_check`;
- the FTS5 shadow tables are left empty: the search index is derived, and
  neither measured command reads it.

The copy is internally consistent: `PRAGMA integrity_check` reports `ok`,
`PRAGMA foreign_key_check` is empty, and `PRAGMA user_version` was set to the
source's 69 so the candidate migrates it exactly as it would migrate a real
store. The candidate's first run performed that forward-only migration; the
store was already at 70 when the numbers below were re-taken at the new tip, so
no migration is inside any timed run.

| Store | Sessions | Evidence rows | Size | Schema at measurement |
| --- | ---: | ---: | ---: | ---: |
| Live (read-only source) | 20,615 | 11,736 | 42,989,264,896 B | 69 |
| Large sandbox | 3,002 | 11,747 | 6,370,758,656 B | 70 |
| Small sandbox | 1 | 1 | 26,161,152 B | 70 |

The smaller sandbox is a fresh store produced by one scoped `harvest --session`
of the same pinned session: it holds that session only.

### The pinned session

X = `9aec3ccd-622c-46ee-b346-92d6e20fd2a5`, a Claude Code root recorded in the
live store with eight stored child sessions.

X's source is pinned inside the sandbox:

- its 12,138,400-byte root transcript and the eight child transcripts are
  copied under `$SB/claude/projects/<slug>/`, two project slugs in all
  (15,494,751 + 591,999 bytes), with `cp -a` so the modification times are the
  live ones;
- the remaining 291 real project directories (of 293) are created empty, so the
  Claude root probe performs the same per-directory statistics it performs on
  the real tree;
- only X's cohort source paths are rewritten in the copy (the `sessions` rows
  and the matching `claude_transcript_evidence` rows). Every other stored path
  keeps its live value and is never read.

An earlier candidate session, `8ae114d8-cd14-40c4-bc4b-21a8970a544d`, was
rejected as X: its 17 subagent transcripts carry no `sessionId` record at all,
so identity verification refuses every one of them ("transcript sessionId
disagrees with the discovered session or parent identity") and the live store
itself holds zero children for that session. X is the smallest Claude root in
the sample whose stored children re-capture successfully.

A property of X to read the numbers with: when X's children are re-captured,
each child transcript records only its own `sessionId`, so the capture
re-derives each child as its own root and the stored parent links are dropped.
The settled sandbox therefore holds X plus eight roots, and the scoped cohort
of X is X alone. The timed `open` below re-captures X's own 12.1 MB. The
post-pin warm-up, which ran while the eight links were still present, processed
all nine sessions in 13.6 s wall — that is the cost of the same work over
16.1 MB, and it is the only datapoint that includes the children.

### Configuration and harness

Only `claude-code` is enabled; every other harness is disabled, and the source
path points at the pinned tree. Output, state, and configuration all live under
the sandbox. The browser opener is disabled with `BROWSER=/bin/true` (the
freedesktop `$BROWSER` chain starts that binary with the URL as its argument
and returns immediately, so `open` never blocks on a browser).

`open` needs a dashboard already serving the sandbox store, so one was started
per store and left running for the timed runs:

```sh
# large: port 8799, small: port 8798
$SB/bin/peasant --data-dir "$DATA" --config-dir "$CONFIG" --state-dir "$STATE" \
  --config "$CONFIG/peasant/config.yaml" \
  web start --foreground --port "$PORT" --no-browser
```

Both dashboards answered `GET /api/v1/health` with `{"status":"ok"}` before the
`open` runs began. Every measured command used the same explicit
`--data-dir/--config-dir/--state-dir/--config` flags:

```sh
# harvest
$SB/bin/peasant ... harvest --session X --json

# open (the dashboard on $PORT is already healthy)
$SB/bin/peasant ... open --session X --port $PORT
```

## Recording fields

Every run is one whole process. The fields are:

| Field | Meaning |
| --- | --- |
| `wall_ms` | External wall time of the whole process, measured with a monotonic clock around the process from before `exec` to after exit. This is the number the targets are judged on. |
| `run` | `warmup` or `1`..`5`. The warm-up is never part of the median or max. |
| `exit` | Process exit status. Every recorded run below exited 0. |
| `reported_duration` | The duration the command itself prints (`duration` in `harvest --json`). Recorded beside the wall time; it excludes process start and exit. |
| `diff_status` | For X, the `status` of its entry in the `harvest --json` `sessions` array. The gate fails if this is not `unchanged` in every timed run. |
| `proof` | `harvest`: X's JSON entry plus the summary counts. `open`: the two stdout lines (the opened line and the address). |

The small host cost of process start is visible on its own: `peasant version`
measured 33–47 ms and `peasant sessions list --limit 1` 35 ms in the same
session.

## Results

### Large sandbox: `harvest --session X` (3,002 sessions, 5.9 GiB)

Warm-up: 682.0 ms. The store was already settled by earlier runs at this tip.

| Run | wall_ms | reported_duration | exit | X's diff_status |
| ---: | ---: | ---: | ---: | --- |
| 1 | 699.5 | 500ms | 0 | unchanged |
| 2 | 683.8 | 500ms | 0 | unchanged |
| 3 | 676.0 | 500ms | 0 | unchanged |
| 4 | 658.8 | 500ms | 0 | unchanged |
| 5 | 667.3 | 500ms | 0 | unchanged |

**Median 676.0 ms, max 699.5 ms.** Every run reported `New 0, Updated 0,
Unchanged 1, Errors 0`; X's own verdict was `unchanged` in all five timed runs.

Two earlier five-run samples taken immediately after the build each carried one
stalled run (1,746.0 ms and 1,802.9 ms; the command's own duration was 1.4 s and
1.5 s in those runs) before settling at 634.8–698.5 ms. The stall is host-side,
not the build: an interleaved same-store run alternated the old and new binaries
and produced a ~1.6 s stall in one run of *each* (see the delta section). All
three sample medians are under 1 s (663.6, 664.6, 676.0 ms); the table above is
the clean sample.

### Large sandbox: `open --session X`

Warm-up: 7,680.9 ms. The dashboard on port 8799 served the session throughout;
every run printed both lines and exited 0.

| Run | wall_ms | exit | printed lines |
| ---: | ---: | ---: | ---: |
| 1 | 6,695.1 | 0 | 2 |
| 2 | 6,664.3 | 0 | 2 |
| 3 | 6,508.6 | 0 | 2 |
| 4 | 6,575.8 | 0 | 2 |
| 5 | 6,399.6 | 0 | 2 |

**Median 6,575.8 ms, max 6,695.1 ms.** The printed lines were identical in
every run (the session title is its recorded title):

```text
peasant: opened "<title>" · not published
http://localhost:8799/projects/<project-hash>/9aec3ccd-622c-46ee-b346-92d6e20fd2a5
```

This **misses the 2 s gate**; see the findings below.

### Small sandbox (1 session, 25 MiB)

`harvest --session X`: warm-up 686.6 ms.

| Run | wall_ms | reported_duration | exit | X's diff_status |
| ---: | ---: | ---: | ---: | --- |
| 1 | 598.6 | 500ms | 0 | unchanged |
| 2 | 605.3 | 500ms | 0 | unchanged |
| 3 | 599.3 | 500ms | 0 | unchanged |
| 4 | 600.0 | 500ms | 0 | unchanged |
| 5 | 610.8 | 500ms | 0 | unchanged |

**Median 600.0 ms, max 610.8 ms.**

`open --session X` (dashboard on port 8798): warm-up 7,085.6 ms.

| Run | wall_ms | exit | printed lines |
| ---: | ---: | ---: | ---: |
| 1 | 6,271.7 | 0 | 2 |
| 2 | 6,201.2 | 0 | 2 |
| 3 | 6,086.7 | 0 | 2 |
| 4 | 6,462.8 | 0 | 2 |
| 5 | 6,324.4 | 0 | 2 |

**Median 6,271.7 ms, max 6,462.8 ms.**

### Size independence

Same command, same pinned X, same flags, warm stores:

| Command | 1-session store | 3,002-session store | Delta of medians |
| --- | ---: | ---: | ---: |
| `harvest --session X` median | 600.0 ms | 676.0 ms | +76.0 ms (+12.7%) |
| `open --session X` median | 6,271.7 ms | 6,575.8 ms | +304.1 ms (+4.8%) |

The delta is a constant, not a scaling term: both runs process X alone, and the
interleaved same-store check put the large store at 611.0 ms against the small
one at 552.7 ms (+58.3 ms) — the same order. The difference is the same fixed
work over a 6 GiB store instead of a 25 MiB one (deeper index trees and a larger
audit write). It does not scale with the session count: the pre-change binary
takes 10.6× longer across the same pair.

### Pre-change binary baseline

The baseline binary cannot open a schema-70 store (it refuses with "database
schema is version 70, but this build understands only up to version 69"). The
sandbox store was therefore copied again with `sqlite3 .backup`, the migration's
additions were dropped (one partial index and two tables, which takes their own
indexes with them), and `PRAGMA user_version` was set to 69. Both copies passed
`integrity_check`.

On the same pinned sources and the same data, re-taken in the same measurement
window as the tables above:

| Run | Large sandbox wall_ms | Small sandbox wall_ms |
| ---: | ---: | ---: |
| warm-up | 6,754.3 | 712.0 |
| 1 | 6,735.1 | 622.9 |
| 2 | 6,148.5 | 606.8 |
| 3 | 6,184.7 | 585.9 |
| 4 | 6,044.1 | 581.4 |
| 5 | 6,261.6 | 582.3 |

**Large: median 6,184.7 ms, max 6,735.1 ms. Small: median 585.9 ms, max
622.9 ms.** The pre-change command reported `Unchanged 9` in every run.

The first baseline sample taken earlier in the day measured 7,292.1 ms (large)
and 740.1 ms (small). It is the same binary on the same data; both numbers moved
down by about 1.1 s and 150 ms respectively in this window, which is the same
host/store-state drift that moved the candidate's own smooth runs. The ratio is
what matters and it is stable: the pre-change binary is 10.6× across the two
stores (6,184.7 / 585.9), while this tip is 1.13× (676.0 / 600.0).

Two things to read the baseline with:

- The first baseline run ever taken on the large store took **14,442 ms**. That
  run performed a one-time store-wide index maintenance of 100 stored sessions
  and printed the old warning about sessions "outside this run's session
  selection" that still needed maintenance. Every sample since starts after
  that one-time work.
- The pre-change command reports `Unchanged 9` even on the 1-session store
  because its `--session` allowlist marks every discovered session it filters
  out as `unchanged` rather than `excluded by selection`. That is a reporting
  difference between the two binaries, not work performed on the small store.

This is not the 115 s figure quoted in the design for the real source tree:
that number includes the discovery walk over the real 9.7 GB,
293-project-directory transcript tree. Pinning X's source deliberately removes
that walk from *both* binaries, so what remains here is the store-side cost the
change is about.

### What changed since the previous measurement

The previous record measured `778c1c131`. The ten commits to `74fc97a10` change
two things:

- The full walk now decides Claude teammate links from two indexed reads over
  the derived lookup tables instead of building an in-memory spawn index, so
  there is one decision path instead of two.
- Two follow-up fixes make those counts exact in both build modes: the by-path
  evidence read reports whether it covered every stored row, a
  cannot-be-parsed identity list keeps its readable fields and forces a
  re-mine, and a scoped dry run now reads the evidence cache so its count delta
  is exact (a read only; the deferred write is still skipped).

The numbers were re-taken at the new tip, and the previous build was rebuilt
from an archive and run against the same store state to separate build effects
from drift:

| Command, store | Previous record (`778c1c131`) | This tip (`74fc97a10`) | Same-store A/B (`778c1c131` vs this tip) |
| --- | ---: | ---: | ---: |
| `harvest --session X`, large | 689.5 ms | 676.0 ms | 612.7 vs 611.0 ms (paired-diff median −8.5 ms) |
| `harvest --session X`, small | 652.1 ms | 600.0 ms | 551.9 vs 552.7 ms (paired-diff median +0.7 ms) |
| `open --session X`, large | 7,549.0 ms | 6,575.8 ms | 6,594.1 vs 6,575.8 ms |
| `open --session X`, small | 7,589.3 ms | 6,271.7 ms | 6,180.7 vs 6,271.7 ms |

So the absolute medians moved down, but the same-store A/B says the build is
not the cause: on an interleaved run of ten pairs the scoped harvest differs by
a median of −8.5 ms (large) and +0.7 ms (small), and `open` is flat within
noise. The earlier record's numbers were taken in a colder, busier window; the
drift is host and store-state, and it moved the pre-change baseline by the same
order (finding 6). The one real difference is the intermittent stall described
above, which both builds show.

### Full-run cost at this tip

The indexed teammate decision is not free, and it is on the full-run path, not
the scoped one:

- The decision over 2,215 identities measured **4.7 ms in memory → 44 ms
  indexed**, about **+45 ms end to end** (+0.9%) for the full Claude discover.
  What it buys is one decision path instead of two: the in-memory spawn index is
  gone.
- The whole-table evidence load it sits beside is **≈4.78 s and is not
  removed** by this change. It serves freshness, display fields, and
  native-agent linking, and it dominates the full run.
- The change that would remove it is a lighter evidence projection that omits
  `identity_records_json` for roots that are not native-agent owners. That is a
  follow-up, not part of this change; until it lands the full run keeps the
  whole-table read.

### Where the time goes (this tip, large store)

Same store, same X, three runs each, median. These were taken just after the
gate sample in a quieter window, so they sit about 10% below the gate table;
read them as a decomposition, not as replacement numbers.

| Variant | wall_ms | reported |
| --- | ---: | ---: |
| `harvest --session X --dry-run` | 51 | 0s |
| `harvest --session X --dry-run --no-related` | 52 | 0s |
| `harvest --session X --no-related` | 612 | 500ms |
| `harvest --session X` | 604 | 500ms |
| `harvest --session X --force` | 6,615 | 6.4s |

The settled dry run (51 ms) classifies X from the store's evidence without
re-reading the source tree, so it measures process start plus the store's
classifying reads. The full unchanged run (604–676 ms across the two windows)
adds the cohort source read and the evidence/audit write path, about 0.55–0.63 s.
The forced run is the `open` payload: re-extraction and re-index of X's 12.1 MB
transcript.

## Targets

| Target | Result |
| --- | --- |
| `harvest --session X` median under 1 s | **Met**: 676.0 ms median, 699.5 ms max, unchanged verdict in all five runs; 663.6 and 664.6 ms medians in the two earlier samples. |
| `open --session X` every run under 2 s | **Missed** by the forced re-index in this table (6.6 s). **Met** after `open` stopped passing `--force`: finding 1. |
| Time independent of store size | **Met** for both commands: +12.7% for harvest, +4.8% for open between a 1-session and a 3,002-session store, against 10.6× for the pre-change binary. |

## Recorded findings

1. **`open --session X` missed the 2 s target on a large X because it forced a
   re-index.** At the time of this table, `open` ran `harvest --force`, which
   re-extracted and re-indexed X's 12,138,400-byte transcript: the forced
   harvest alone was ≈6.6 s, essentially the whole of `open`'s 6.6 s. The same X
   on a 1-session store took 6.3 s, so the miss was store-independent and
   session-size-bound. The first measurement with the children still linked
   (16.1 MB, nine sessions) took 13.6 s, which is the same cost curve.

   That force is no longer how `open` records an unchanged session. `open`
   harvests the named session with `--detect-commits --refresh-commits` and
   does not pass `--force`. `--refresh-commits` is off by default, so a plain
   `harvest --session` does not detect commits for an unchanged session. An
   unchanged named session opened this way refreshes only its commit rows.
   Measured 2026-11-09 on the large sandbox at `/tmp/opencode/slice17-timing/large`,
   session `9aec3ccd-622c-46ee-b346-92d6e20fd2a5`, port 8799, five runs after
   one warmup, with `open` passing `--refresh-commits`: median 810.9 ms, max
   850.2 ms (722.8, 741.1, 810.9, 815.8, 850.2). Every timed run exited 0 and
   opened the session. The 7,210 ms median in the issue is the before number.
2. **The unchanged scoped harvest sits at about 0.68 s on this host**, and the
   settled `--dry-run` is 51 ms, so the source read plus the evidence/audit
   write path is about 0.6 s. It meets the 1 s target with about 0.32 s of
   headroom. An interleaved same-store A/B against the previous build shows no
   build effect on this command (paired medians −8.5 ms large, +0.7 ms small).
3. **The indexed teammate decision costs +45 ms on the full run and buys one
   decision path instead of two** (4.7 ms in memory → 44 ms indexed for 2,215
   identities). The whole-table evidence load beside it (≈4.78 s) is **not**
   removed. The follow-up that would remove it is a lighter evidence projection
   that omits `identity_records_json` for roots that are not native-agent
   owners; it is not part of this change.
4. **The sandbox is a 3,002-session sample of the 20,615-session live store, not
   a whole-store copy**, because the copy does not fit on this host while other
   work runs. Every row of every table for those sessions, every global table,
   and the full evidence table are present; the FTS shadow index is empty. A
   whole-store re-run should be expected to differ only in the store-side
   queries' constant factors, which this record already bounds at 12.7% between
   1 and 3,002 sessions.
5. **X's stored child links drop on re-capture.** Each child transcript records
   only its own `sessionId`, so re-capturing them re-derives each as a root.
   The timed `open` therefore re-captures X alone. The live store's eight child
   rows for X are unaffected until such a re-capture runs.
6. **The absolute numbers drift with the host and the store's accumulated
   state, and the previous record was taken in a colder, busier window.** The
   same binary measured `harvest --session` at 612.7 ms (interleaved) in this
   window against 689.5 ms in the previous one; the two windows differ in host
   load and in how much the forced `open` runs had accumulated in the store.
   The pre-change baseline moved 7,292.1 → 6,184.7 ms for the same reason. The
   previous record's numbers are therefore not directly comparable to these;
   the same-store A/B is. Two isolated ~1.6–1.8 s runs appeared in both builds
   and are host stalls, not the change.
7. **The originally pinned session could not be used.** Its 17 subagent
   transcripts record no `sessionId`, so identity verification refuses them all;
   the live store has zero stored children for it. It is recorded here so a
   later measurement does not rediscover it as a mystery.

## What was not measured

- The full-run teammate decision and the whole-table evidence load. The
  +45 ms (4.7 ms → 44 ms over 2,215 identities) and the ≈4.78 s evidence load
  are the full-run measurement recorded with that change, not re-taken here;
  this record measures the scoped commands and the pre-change binary.
- A whole-store sandbox copy (see finding 4). The live store was never written.
- The full-source-tree discovery cost: the pinned tree removes the 293-project
  transcript walk from both binaries by design, so the design's 115 s figure
  for the real tree is context, not a reproduction.
- Sessions under other harnesses. Only `claude-code` was enabled, because X is
  a Claude Code session and the pinned tree is built for it.
- A small-X `open` that passes 2 s. The sample's only Claude roots with stored
  children are 9.5 MB and larger, so there was no small capturable cohort to
  measure; the small-store run isolates store size instead.
