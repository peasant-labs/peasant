# Session-scoped harvest and open timing

Measured on 2026-11-09 at revision
`778c1c131b3eae6bf7d87f64dd89246a0b507b92` (clean worktree), using the pinned
Nix development shell (Go 1.26.3).

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
  peasant v0.14.1-dev.143+778c1c131
  ```

- The revision is carried by the binary itself, not assumed from the worktree:

  ```text
  $ git -C <worktree> rev-parse --short=9 HEAD
  778c1c131
  $ grep -c 778c1c131 $SB/bin/peasant
  3
  $ go version -m $SB/bin/peasant | grep ldflags
  	build	-ldflags="-X github.com/peasant-labs/peasant/internal/defaults.version=v0.14.1-dev.143+778c1c131"
  $ sha256sum $SB/bin/peasant
  7f2d81a9be367c59ba665fb1aa9b987f70523aa3f7f4b233dc4c03ea622cb3a5
  ```

  `make build` also reported that the web export and `./peasant` carry the same
  provider-build provenance hash.

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
store. The first candidate run performed that forward-only migration; the
warm-up absorbs it.

| Store | Sessions | Evidence rows | Size | Schema at measurement |
| --- | ---: | ---: | ---: | ---: |
| Live (read-only source) | 20,615 | 11,736 | 42,989,264,896 B | 69 |
| Large sandbox | 3,002 | 11,747 | 6,370,758,656 B | 70 |
| Small sandbox | 1 | 1 | 26,136,576 B | 70 |

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

Warm-up: 665.8 ms. The store had already been settled by the preceding
post-pin run and the migration run.

| Run | wall_ms | reported_duration | exit | X's diff_status |
| ---: | ---: | ---: | ---: | --- |
| 1 | 689.5 | 600ms | 0 | unchanged |
| 2 | 669.9 | 500ms | 0 | unchanged |
| 3 | 660.9 | 500ms | 0 | unchanged |
| 4 | 696.6 | 600ms | 0 | unchanged |
| 5 | 713.6 | 600ms | 0 | unchanged |

**Median 689.5 ms, max 713.6 ms.** Every run reported `New 0, Updated 0,
Unchanged 1, Errors 0` for the cohort; X's own verdict was `unchanged` in all
five timed runs.

### Large sandbox: `open --session X`

Warm-up: 6,923.2 ms. The dashboard on port 8799 served the session throughout;
every run printed both lines and exited 0.

| Run | wall_ms | exit | printed lines |
| ---: | ---: | ---: | ---: |
| 1 | 7,256.6 | 0 | 2 |
| 2 | 7,549.0 | 0 | 2 |
| 3 | 7,448.3 | 0 | 2 |
| 4 | 7,836.5 | 0 | 2 |
| 5 | 7,756.6 | 0 | 2 |

**Median 7,549.0 ms, max 7,836.5 ms.** The printed lines were identical in
every run (the session title is its recorded title):

```text
peasant: opened "<title>" · not published
http://localhost:8799/projects/<project-hash>/9aec3ccd-622c-46ee-b346-92d6e20fd2a5
```

This **misses the 2 s gate**; see the findings below.

### Small sandbox (1 session, 25 MiB)

`harvest --session X`: warm-up 636.7 ms.

| Run | wall_ms | reported_duration | exit | X's diff_status |
| ---: | ---: | ---: | ---: | --- |
| 1 | 732.1 | 600ms | 0 | unchanged |
| 2 | 643.0 | 500ms | 0 | unchanged |
| 3 | 652.1 | 500ms | 0 | unchanged |
| 4 | 657.3 | 600ms | 0 | unchanged |
| 5 | 648.9 | 500ms | 0 | unchanged |

**Median 652.1 ms, max 732.1 ms.**

`open --session X` (dashboard on port 8798): warm-up 7,487.8 ms.

| Run | wall_ms | exit | printed lines |
| ---: | ---: | ---: | ---: |
| 1 | 7,589.3 | 0 | 2 |
| 2 | 7,281.5 | 0 | 2 |
| 3 | 7,595.3 | 0 | 2 |
| 4 | 7,377.4 | 0 | 2 |
| 5 | 7,800.8 | 0 | 2 |

**Median 7,589.3 ms, max 7,800.8 ms.**

### Size independence

Same command, same pinned X, same flags, warm stores:

| Command | 1-session store | 3,002-session store | Delta of medians |
| --- | ---: | ---: | ---: |
| `harvest --session X` median | 652.1 ms | 689.5 ms | +37.4 ms (+5.7%) |
| `open --session X` median | 7,589.3 ms | 7,549.0 ms | −40.3 ms (−0.5%) |

The harvest delta is not a scaling term: both runs process X alone, so the
+37 ms is the same constant work over larger index trees and page-cache noise.
Neither command grows with the stored session count, and 3,002 sessions is
3,002× the small store.

### Pre-change binary baseline

The baseline binary cannot open a schema-70 store (it refuses with "database
schema is version 70, but this build understands only up to version 69"). The
sandbox store was therefore copied again with `sqlite3 .backup`, the migration's
additions were dropped (one partial index and two tables, which takes their own
indexes with them), and `PRAGMA user_version` was set to 69. Both copies passed
`integrity_check`.

On the same pinned sources and the same data:

| Run | Large sandbox wall_ms | Small sandbox wall_ms |
| ---: | ---: | ---: |
| warm-up | 7,106.6 | 667.1 |
| 1 | 7,292.1 | 742.9 |
| 2 | 7,145.1 | 740.1 |
| 3 | 7,169.7 | 759.6 |
| 4 | 7,299.1 | 681.9 |
| 5 | 7,403.3 | 666.5 |

**Large: median 7,292.1 ms, max 7,403.3 ms. Small: median 740.1 ms, max
759.6 ms.** The pre-change command reported `Unchanged 9` in every run.

Two things to read those numbers with:

- The first baseline run on the large store took **14,442 ms**. That run
  performed a one-time store-wide index maintenance of 100 stored sessions and
  printed the old warning about sessions "outside this run's session selection"
  that still needed maintenance. The table above starts after that one-time
  work; the steady state is the honest per-run baseline.
- The small-store baseline's first timed run took 1,731.9 ms before settling.
  A second, settled five-run sample is recorded beside it: 742.9, 740.1,
  759.6, 681.9, 666.5 ms (median 740.1).

The old command reports `Unchanged 9` even on the 1-session store because its
`--session` allowlist marks every discovered session it filters out as
`unchanged` rather than `excluded by selection`. That is a reporting difference
between the two binaries, not work performed on the small store.

The pre-change binary at the pinned tree costs about 7.3 s on the large store
and 0.74 s on the small one: a **9.9×** dependence on the stored session count.
This change removes exactly that dependence: 5.7% between the same two stores.

This is not the 115 s figure quoted in the design for the real source tree:
that number includes the discovery walk over the real 9.7 GB,
293-project-directory transcript tree. Pinning X's source deliberately removes
that walk from *both* binaries, so what remains here is the store-side cost the
change is about.

### Where the time goes (candidate, large store)

Same store, same X, single runs:

| Variant | wall_ms | reported |
| --- | ---: | ---: |
| `harvest --session X --dry-run` | 523 | 500ms |
| `harvest --session X --no-related` | 800 | 600ms |
| `harvest --session X` | 930 | 700ms |
| `harvest --session X --dry-run --no-related` | 178 | 100ms |
| `harvest --session X --force` | 6,837 | 6.7s |

The unchanged scoped harvest is roughly 0.18 s of locate/diff and 0.5 s of
writes (the store commit and audit row) on this host. The forced run is the
`open` payload: re-extraction and re-index of X's 12.1 MB transcript.

## Targets

| Target | Result |
| --- | --- |
| `harvest --session X` median under 1 s | **Met**: 689.5 ms median, 713.6 ms max, unchanged verdict in all five runs. |
| `open --session X` every run under 2 s | **Missed**: 7.5 s median on the 12.1 MB session. Recorded finding below. |
| Time independent of store size | **Met** for both commands: +5.7% for harvest, −0.5% for open between a 1-session and a 3,002-session store. |

## Recorded findings

1. **`open --session X` misses the 2 s target on a large X, and the miss is the
   session's own size, not the store.** `open` runs `harvest --force`, which
   re-extracts and re-indexes X's 12,138,400-byte transcript: the forced
   harvest alone is 6.8 s and `open` is 7.5 s. The same X on a 1-session store
   takes 7.6 s, so the gate is store-independent and session-size-bound. This
   is the measured-and-deferred `--force` finding: removing `--force` from
   `open` becomes a follow-up that must solve commit detection in both modes.
   The first measurement with the children still linked (16.1 MB, nine
   sessions) took 13.6 s, which is the same cost curve.
2. **The unchanged scoped harvest sits at about 0.69 s on this host, roughly
   0.5 s of it the write path** (`--dry-run` is 0.18 s). It meets the 1 s
   target with about 0.3 s of headroom, above the 0.2–0.6 s the cost model
   estimated for the warm store. The host was running other workloads
   throughout (load average 8–12), so absolute numbers on an idle host will be
   lower.
3. **The sandbox is a 3,002-session sample of the 20,615-session live store, not
   a whole-store copy**, because the copy does not fit on this host while other
   work runs. Every row of every table for those sessions, every global table,
   and the full evidence table are present; the FTS shadow index is empty. A
   whole-store re-run should be expected to differ only in the store-side
   queries' constant factors, which this record already bounds at 5.7% between
   1 and 3,002 sessions.
4. **X's stored child links drop on re-capture.** Each child transcript records
   only its own `sessionId`, so re-capturing them re-derives each as a root.
   The timed `open` therefore re-captures X alone. The live store's eight child
   rows for X are unaffected until such a re-capture runs.
5. **The originally pinned session could not be used.** Its 17 subagent
   transcripts record no `sessionId`, so identity verification refuses them all;
   the live store has zero stored children for it. It is recorded here so a
   later measurement does not rediscover it as a mystery.

## What was not measured

- A whole-store sandbox copy (see finding 3). The live store was never written.
- The full-source-tree discovery cost: the pinned tree removes the 293-project
  transcript walk from both binaries by design, so the design's 115 s figure
  for the real tree is context, not a reproduction.
- Sessions under other harnesses. Only `claude-code` was enabled, because X is
  a Claude Code session and the pinned tree is built for it.
- A small-X `open` that passes 2 s. The sample's only Claude roots with stored
  children are 9.5 MB and larger, so there was no small capturable cohort to
  measure; the small-store run isolates store size instead.
