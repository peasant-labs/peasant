# Testing Patterns

Code examples and strategies for testing the Peasant web dashboard and WebSocket protocol.
See [`AGENTS.md`](AGENTS.md) for the test and fixture writing rules and
[`README.md`](README.md#package-map) for the package map; the documentation map below links each
test layer to its entry point.

### E2E documentation map

| Layer | Runs in | Entry point |
|-------|---------|-------------|
| WebSocket hub E2E | `make check` | [Go WebSocket E2E](#go-websocket-e2e-verified) (below) |
| Committed transcript fixtures (`internal/e2e/testdata/`) | `make check` | [Fixture meta-tests](#committed-fixture-meta-tests-make-check) + [`docs/e2e-fixture.md`](docs/e2e-fixture.md) |
| Full-stack skip-gate + pull round-trip (podman + village + real CLI) | `make e2e` only | [Full-stack e2e](#full-stack-e2e-verified) + [`docs/e2e.md`](docs/e2e.md) |

## Test gate

`make check` runs the Go suite through `cmd/testgate`. The gate exists to move
the suite's expensive non-race work out of the race pass **without dropping it**,
and to prove that every test still runs exactly once across the passes.

### Two passes, one registry

- **`no-race-partition.yaml`** (committed at the repo root) is the admission
  record. Its `partition` entries run in the **no-race pass** and are excluded
  from the race pass; its `protected` entries are pinned into the race pass and
  must never be registered as partition members.
- Under `RACE=1` the gate runs a **race pass** (every listed test minus the
  partition members) and a **no-race pass** (exactly the partition members).
  Under `RACE=0` it runs a **single no-race pass** over every test, but still
  computes the plan and applies the screen. The gate computes the plan from
  `go test -list`, so `cmd/testgate plan` prints the plan and runs nothing.

### Subset runs (`-pkgs`)

`run` and `plan` take `-pkgs`, a comma-separated list of repo-relative package
patterns that defaults to `./...`:

```bash
go run ./cmd/testgate run -pkgs ./internal/testkit/coveragemap,./cmd/testgate -race=false
```

Only the named patterns are listed, planned, and executed, so the gate can be
checked end to end in seconds instead of the whole 13–15 minute suite. A subset
run is **not** a full-suite gate result and says so:

- it prints a `SUBSET RUN` header naming the patterns and the package count;
- the four-rule screen's **registered liveness** rule (rule 3) requires events
  only for registered packages the plan contains, so a registered package the
  subset deliberately excluded does not fail the screen — while a planned
  registered package that emits no events still does;
- the whole-suite budget is **not applicable**: the gate prints
  `budget: not applicable (subset run: N of M packages)` and never compares a
  subset wall against the committed 120s bar.

A full run (`-pkgs ./...`, the default) plans the whole
registry; the budget verdict follows the committed `enforcement` mode (warn
demotes a miss, blocking fails on it — see below). `profile`'s existing
single-package `-pkg` is separate and unchanged.

### Timing mode

`cmd/testgate timing` summarizes an arbitrary `go test -json` stream with the
gate's own stream library:

```bash
go test -race -json ./internal/ingest/... | go run ./cmd/testgate timing -top 40
go run ./cmd/testgate timing -top 40 < ingest.json
```

It takes `-top N` (rows per section, default 25), `-family-re RE` (regroup by a
capture-group regexp), `-no-families` (skip the per-family section), and
`-warn-pct PCT` (mark tests over that share). With no file argument it reads
stdin. The report and the gate's merged-pass report come from the same renderer,
so a hand measurement and a gate measurement are the same measurement. A failing
test exits non-zero.

### Admission — all four criteria

A registry entry is admitted only when it carries:

1. `class` from the closed set `single-threaded-bytes` · `subprocess` ·
   `static-analysis` · `toolchain`;
2. `evidence`, a `#<testName>` anchor that resolves to a test declared in the
   named test's own file (the file is implied by `package`/`test`, so a line
   offset cannot drift it);
3. an observed `cost` pair (`wall_ms`, `cpu_ms`) from a committed measurement;
4. for `subprocess`, `build_flags` resolved against the referenced
   `exec_command_site`, a `<file>#<funcName>#<fragment>` anchor that locates the
   `exec.Command` call inside the named function by a required source fragment —
   a partition child must be built **without** `-race`.
   `TestOpenCodeNativeCLI` is pinned in `protected` as the counter-example: its
   `go build` child uses `nativeCLIRaceFlag`, which is `-race=true` under the
   `race` build tag.

`internal/testkit/testgate/registry_test.go` validates the committed registry against the
tree and runs named negative cases from `testdata/registry_cases.yaml`; moving the
counter-example into `partition` fails the test.

### The four-rule exactly-once screen

The screen merges the passes and checks, in both `RACE` modes:

1. **exactly-once** — a test that ran in more than one pass is a double-run (FAIL);
2. **partition containment** — a partition member must not run in the race pass (FAIL);
3. **registered liveness** — a registered package with no test events, a partition
   member that did not run, or a protected test that did not run (FAIL);
4. **unregistered liveness** — a planned unregistered package with no events is
   **reported only** (REPORT).

The screen records no baseline and compares nothing across time. Any test failure,
screen FAIL, or invocation error makes the gate exit non-zero.

### Per-invocation records and run classes

Each `go test` invocation is a recordable unit with a `{unit, class, wall, user,
system}` record. The race pass records one unit per package (class `race`); the
`RACE=1` no-race pass records one unit per partition test, carrying that entry's
class. `user`/`system` come from `getrusage(RUSAGE_CHILDREN)`; under concurrency
the counter is process-global, so per-unit CPU is best-effort while **wall is
always exact**.

Run classes (keep them separate):

- **truth / budget (profile-free):** `-race -count=1 -timeout=0 -json -fullpath
  -outputdir <d>`. This is the only quotable wall.
- **attribution (profiles ON; wall not quotable):** add `-blockprofile`,
  `-mutexprofile`, `-cpuprofile`, `-trace`. `-cpuprofile` does not profile child
  processes, so it is for intra-binary attribution only. `testgate profile`
  additionally re-profiles the 10 slowest top-level tests with a per-test
  `-cpuprofile` by default (`-cpuprofile-top N`, `0` disables); the re-runs are
  semaphore-limited to the batch count and do not observe one another.
- **interactive debug:** `-v -fullpath -run <target>`.

Do not pin `-parallel`: it defaults to `GOMAXPROCS` (cgroup-aware) and pinning
changes the packing ceiling being measured. The gate prints the effective `-p`,
`-parallel`, `-count`, and `GOMAXPROCS`.

### Budget and calibration surface

`budget.yaml` at the repository root carries the committed budget for the whole
suite: the gate normalises the combined test wall by the calibration factor `L`
and compares it to the bar. What a miss *does* is decided by the committed
`enforcement` field, a closed set of `blocking` (miss fails the gate) and
`warn` (miss prints an unmissable `WARN (non-blocking)` line and leaves the
exit code green). An absent field defaults to `blocking`; any other value fails
closed. `CHECK_START_NS` is stamped by `make check` and the gate reports the
pre-test wall (`test-start - CHECK_START_NS`) separately from the test wall. The
calibration factor `L` is the gate's fixed CPU probe over the committed reference;
`L > 4` is reported INCONCLUSIVE and does not fail the gate. When no fixture is
present the gate reads `TEST_BUDGET` (seconds, always blocking) from the
environment and otherwise prints raw walls only. `report.json` records the mode
(`budget_enforcement`) and whether a miss was demoted (`budget_warn`).

Use `warn` while the suite is known-over-budget and the bar is a target being
worked toward: the miss stays visible on every run without red-denied landings.
Use `blocking` once the suite sustainably meets the bar, so a regression fails
the gate. The switch is a one-line `enforcement` change in `budget.yaml`.

### Current status

The committed budget is **120s** under **`enforcement: warn`**, and the suite
does **not** meet it, so `make check` prints a `WARN (non-blocking)` budget
line **by construction** and stays green. Final consolidated measurement over
the substantive tree (2026-09-29, head `fc7d9c95`): race pass **13m43s**
(823.1s), no-race pass
**2m10s** (129.6s), combined **15m53s** (952.9s), L-normalised **16m42s** at
`L` 0.951. Against the pre-epoch base `da7abd7f` (race 19m53s, no-race 1m00s,
combined 20m53s, L-normalised 21m49s at `L` 0.957) the combined wall fell
**24%** and the race pass **31%**; the no-race pass grew from 7 to 19 tests as
twelve detector-taxed tests moved into it. The branch head `c6789de7` was
re-gated after the two follow-up commits, which touch documentation and comment
text only: `make check RACE=1` exited 0, the four-rule screen printed the same
`all four rules passed`, and the gate printed `testgate: PASS`. The head run
measured combined **16m8s** (968.9s) at `L` 0.953, L-normalised **16m56.987s**
(capture `.agents.local/testgate/20260929T233304Z/`).
Per-class (focused, Class A): T3 DB-setup
conversion 946.7s → 563.9s over 22 tests; T1 no-race partition 171.2s race →
78.7s no-race over 12 moved entries; T2 SQL/seed 79.6s → 74.0s; T4 payload
shares reduce no fixture invariant; T6 packing `internal/api` 102.2s → 96.3s
(the helper-group listing 14.4s → 8.0s). The combined CPU numerator is 3353.0s
(race 2999.9s), a 32-core floor of 104.8s. The per-test before/after pairs, the
gate captures, and every L companion are in
[`docs/testing/perf/`](docs/testing/perf/) — `slow-test-taxonomy.md` (cost
drivers and fix classes) and `evidence.md` (before/after evidence).

The binding constraint and the remaining levers, measured rather than assumed:

- the largest single test — `TestUnknownLocalRetentionBeyondTransferBudget` in
  `internal/ingest` — costs **210.1s** focused and alone after its store-open
  conversion (229.9s measured at the base; 243.6s carried prior reference), so no
  batching or sharding can put the suite below it until that test's cost falls
  further;
- no cost class carries a material positive `wall − CPU` gap (the largest is
  +0.27s on the toolchain class), so there is no blocked time left to reclaim;
  the remaining work is *fewer CPU seconds under instrumentation*;
- achieved packing still leaves headroom: per-package CPU/wall in the final race
  pass is ~7.0× (`internal/api`), ~6.4× (`internal/store`), ~3.6×
  (`internal/ingest`) of the 32 hardware threads.

Do not close a budget miss by raising the value. The
bar is a target and the miss is the measurement; while the suite is over it the
committed `enforcement: warn` keeps the miss visible without blocking landings,
and the return to `blocking` waits until the suite sustainably meets the bar.

### Counting-method rule

Every count in a report must carry the exact command that produced it and the SHA
it was run at, or be explicitly labelled **"carried, not re-verified"**. No relayed
number may be restated without re-running it.

### Frozen contract

The gate's exported shape is a frozen contract: the per-invocation record, the
report document, the registry and budget schemas, the class and pre-test closed
sets, the shared stream library path, and the CLI and environment surface.
`internal/testkit/testgate/contract_test.go` and `internal/testkit/teststream/contract_test.go`
pin the shapes against `testdata/contract_shapes.yaml`; the
`contract_compile_test.go` files break the build on a rename, removal, or retype;
and `cmd/testgate/main_test.go` pins the usage text, the exit codes, and the
budget/env precedence. Each frozen axis carries a mutation case that must be
detected, so the freeze is tested rather than asserted. Update the fixture only
when a contract change is deliberate and the consumers are re-pinned.

## Test-support filesystem decorators

The suite's test filesystem decorators wrap an `ingest.FileSystem` to count and
fault an operation, hold an operation, or bound it. The shared contract is
declared once in `internal/testkit/fsdecorator`, a standard-library-only leaf package.

- `CountingFS` (`internal/testutil/counting_fs.go`) is the path-keyed fault and
  count capability.
- `fsdecorator.GatedFS` is the blocking gate: `Arm` holds one operation on one
  path, `Reached` closes when the held operation is entered, and `Release` lets it
  proceed.
- `fsdecorator.BoundedFS` is the bound and read-only case: `Limit` bounds one
  operation on a path, `ReadOnly` refuses every mutating operation.

`fsdecorator.FileSystem` mirrors `ingest.FileSystem`; a contract test asserts the
two are identical, so a decorator held as `GatedFS` or `BoundedFS` is also an
`ingest.FileSystem`.

### Two owners, no import cycle

`internal/testutil` imports `internal/ingest`, so a white-box `package ingest`
test cannot import `internal/testutil` (that would be an import cycle). The
decorators therefore have two owners:

- `internal/testutil` implements the non-white-box decorators.
- `internal/ingest/fsfault_test.go` (`package ingest`) implements the white-box
  decorators, which need the package's unexported internals.

Both import `internal/testkit/fsdecorator`, which imports nothing from `internal/ingest`,
so the same capability can be implemented on either side. Because Go interfaces
are structural, a decorator also satisfies the interface without naming it, and a
consumer can take `fsdecorator.GatedFS`/`BoundedFS` and pass the value to
production code that expects `ingest.FileSystem`.
`internal/testkit/fsdecorator/testdata/decorator_classification.yaml` records, per
decorator type, its capability, owner, and declaring file:line, so each
migration's owner is explicit before any code moves; a test asserts every entry
resolves to a real declaration and the required-name manifest matches both ways.

## Coverage map for the consolidation

The consolidation records every moved, deleted, retained, or deferred name in a
coverage map, closed against an inventory generated when the consolidation's
branch was cut:

- `internal/testkit/coveragemap` declares `Inventory` and `CoverageMap`, the destination
  closed set (`retained-in-place`, `moved:<file>`, `deleted:<rationale-ref>`,
  `followup:<task-id>`), strict loaders, and the validators.
- The inventory's `frozen_from` is the branch-point commit; the validator refuses
  anything that is not a commit-shaped value, so a plan-time inventory is not
  admissible.
- Every inventory name appears in the map exactly once, a `moved` target must
  exist, and a `deleted` or `followup` entry must name its rationale or task. An
  entry is written by the change that performs the move, in the same commit.

## Test performance: keeping `cmd/peasant` fast (and parallel)

`cmd/peasant` is the CLI integration package — each test stands up SQLite + the
cobra command tree + (sometimes) `httptest`/the filesystem. It is the slowest
package by far, so it is the one to watch. The suite was profiled and reduced
from **86.9s → 19.6s under `-race` (−77%, 4.4×)**. This section records what we
measured, why, and the rules that keep it fast.

### Measured progression (`go test -race ./cmd/peasant`)

| Stage | Time | Lever |
|-------|------|-------|
| baseline | **86.9s** | — |
| pool size | 55.1s | `store.Open` opened a fixed **10-connection** pool every call (sqlitex default), each re-parsing the 33-migration schema. Tests need **1** → `store.WithPoolSize` + `PEASANT_DB_POOL_SIZE`, set to `1` in `cmd/peasant`'s `TestMain`. |
| parallelize | 40.2s | The suite ran **fully serial** — tests isolated via process-global `t.Setenv(XDG_*)`, which forbids `t.Parallel()`. Replaced with per-invocation flag injection (below). |
| `$HOME` isolation | 30.1s | Tests resolved local default transcript stores instead of isolated fixtures. `TestMain` points `HOME`+`XDG_*` at a throwaway temp dir. |
| skip-migrate on golden copies | ~30s (store 22.4→20.7s) | `storetest.Open` copies a freshly-migrated golden DB, then re-ran the migration check on every copy → `store.WithSkipMigrations`. |
| deeper DI (creds/sync/state) | **19.6s** | The last serial cohort (push credentials/timing, redact file-writing) was blocked by *production* helpers reading env directly; made them override-aware. |

Other packages for reference (uncached `-race`): `internal/store` ~20–26s,
`internal/ingest` ~12s; most others 1–8s.

### How the time was profiled (reproduce before optimizing)

- **Per-test distribution + serial proof** — `go test -race -v ./cmd/peasant`,
  then sum the `--- PASS: … (Ns)` durations. **sum-of-per-test ≈ wall-clock ⇒
  the suite is serial** (no `t.Parallel`). This is how we proved parallelism was
  the lever, not "more shared setup".
- **`-race` multiplier** — same suite with and without `-race`: 16.8s → 86.9s
  (~5×). The race detector tax is real and unavoidable; it multiplies whatever
  base cost exists, so shrinking the base matters.
- **Where the base goes** — `go test -cpuprofile=cpu.prof ./cmd/peasant` +
  `go tool pprof -top -cum`. This is how we found `store.Open` = **38% of CPU**
  (within it: `sqlitex.NewPool` 20%, schema parse 27%, `sqlitemigration.Migrate`
  16%) and confirmed the 10-vs-1 connection cost (benchmarked: 0.8ms vs 5.8ms
  per open).
- **Default-path isolation** — compare a suspect test under a populated local
  home and an empty one. `TestFtueDiscover_EmptyOnMissingConfig` previously
  traversed local session history instead of testing the empty case it claimed
  to cover.

### Attributes of the slowest tests (what to avoid)

1. **Opening the store many times** under the default pool — the dominant
   per-test fixed cost. Use the golden DB (`storetest`) and a 1-connection pool.
2. **Process-global env isolation** (`t.Setenv("XDG_*", …)`) — correct but
   **forbids `t.Parallel()`**, forcing the whole package serial.
3. **Reading local user data** — any test that resolves a default path
   (`~/.claude`, `~/.local/share/peasant`) can walk local session history; this is
   slow, non-deterministic, and often not testing what the case claims.
4. **Real sleeps/backoff** — e.g. an HTTP client's retry backoff against a 500.
   Inject a zero/short backoff (the models test uses `bestiary.WithRetries(0)`:
   4.17s → 0.22s).

### The parallel-safe pattern (use this for new `cmd/peasant` tests)

Commands take their directories from **explicit flags**, not process env, so
tests inject per-invocation dirs and run concurrently:

- Persistent flags `--data-dir`, `--config-dir`, `--state-dir` override
  `XDG_{DATA,CONFIG,STATE}_HOME`; resolved via `defaults.Resolve*PathWith(...)`,
  `auth.LoadCredentialsFrom(...)`, `resolveOutputSyncDir(cmd)` — all fall back to
  env when unset (back-compat).
- The shared helper **`executeWithDataDir(t, sub, dir, args)`** (in
  `helpers_test.go`) runs a subcommand under a root carrying all three flags set
  to one `dir := t.TempDir()`. Each per-command helper delegates to it.
- `TestMain` (`main_test.go`) sets `PEASANT_DB_POOL_SIZE=1` and isolates
  `HOME`+`XDG_*` to a throwaway dir (via **`os.Setenv`, not `t.Setenv`**, so it
  does not mark tests non-parallel).

```go
func executeMetricsCmd(t *testing.T, dir string, args []string) (string, error) {
    return executeWithDataDir(t, BuildMetricsCommand(), dir, args)
}

func TestMetricsCompute_EmptyStore(t *testing.T) {
    t.Parallel()                 // <- the win; incompatible with t.Setenv
    dir := t.TempDir()
    out, err := executeMetricsCmd(t, dir, []string{"compute"})
    // ...
}
```

**Rules of thumb**
- `t.Parallel()` as the first line; **never** `t.Setenv` for path isolation —
  pass `dir` through `executeWithDataDir` instead.
- **Seed where the command reads**: compute seed paths with
  `defaults.ResolveDBFilePathWith(dir)` / `ResolveDataDirPathWith(dir)` (and
  `ResolveConfigDirPathWith(dir)` for credentials), so the seed and the command
  agree on `<dir>/peasant/...`.
- A test may stay serial only when it genuinely needs process-global state
  (e.g. mutating `os.Stdin`, or deliberately exercising real default-dir
  discovery). Document why in-file.

## Test memory: the staging-arena trap (and the `-race` OOM)

The full-stack E2E harness has a separate memory-sensitive path: the joined-hook
developer-state isolation guard fingerprints local files. It streams these files
through a 32 KiB buffer. Loading a multi-gigabyte local database with `os.ReadFile`
previously made the E2E test process grow with the database size; the race detector
amplified that allocation. `TestPathFingerprintBoundedMemory` checks this exact
shared helper with a 64 MiB sparse file and a 1 MiB allocation ceiling, including
content-only mutation and sandbox-exclusion cases. It runs without the `e2e` tag,
so the ordinary quality gate protects the full-stack harness from this regression.

Separately from time, the suite's **peak memory** was profiled after CI flakily
**OOM-SIGTERM'd** `go test -race ./...` (exit 143) on the small 2-core/7 GB
GitHub runner. The cause was a *single allocation*, and the lesson generalizes:
**a test binary's peak RSS is `concurrency × per-test footprint`, and `-race`
only matters once that footprint is already large.**

**Profiling result.** `cmd/peasant` and `internal/ingest` peaked **4–6 GB under `-race`**
(every other package <100 MB). `-alloc_space` pinned it: **99.6 % of allocation
was `ingest.NewStagingBuffer`** — the harvest/pipeline tests each allocated the
production **2 GiB** arena (`DefaultArenaSizeBytes`). With `t.Parallel` at
`GOMAXPROCS=2`, two live 2 GiB arenas ≈ 4 GiB; on a 4-vcpu runner ≈ 8 GiB.

**Regression coverage.** A tiny test arena, via an env override that mirrors
`PEASANT_DB_POOL_SIZE`: `ingest.EnvArenaSizeBytes` (`PEASANT_INGEST_ARENA_BYTES`)
+ `resolveArenaSizeBytes`, set to **64 MiB** in the `cmd/peasant` and
`internal/ingest` `TestMain`s. The API test binary applies the same override for
its mounted ingest paths, and E2E TestMain supplies it to the harness and CLI
children. Result: `cmd/peasant -race` 2173–6267 MB →
**240 MB**, `ingest` 6185 → **274 MB**. The memory result stands; the time claim
that followed it did not. The ~26s once stated here was stale: at the point the
race/no-race partition landed, a full `make check -race` on a 32-thread box
measured **26m23.8s** for the race pass, **30.4s** for the no-race pass, and
**14.6s** of pre-test steps (calibration L=0.96). The gate prints the current
wall on every run, so read that output rather than a figure fixed here — the
suite is still being optimised, and a hardcoded number ages. Whether a 2-vcpu
runner can hold the per-PR job is a separate CI question and is not settled by
this paragraph.

### How memory was profiled (different tools than time)

- **Peak RSS** — sample `/proc/<pid>/VmHWM` of the test binary. **Not**
  `-memprofile`: it sees only the live Go heap (12 MB here at end — the arenas
  are freed by then), missing the transient/`-race`-shadow pages. `go help
  testflag` even notes `-benchmem` does not count C/off-heap allocations.
- **Pin `GOMAXPROCS` to the target runner's core count.** Peak RSS scales with
  `t.Parallel` concurrency (defaults to `GOMAXPROCS`), so an unpinned dev box
  (many cores) wildly overstates it. We measured at `=2` and `=4`.
- **Attribute it** with `-memprofile` + `go tool pprof -alloc_space -top` (names
  the dominant allocator). `GOMEMLIMIT` could *not* reclaim the 4 GB — it is
  *live* during the run (the arena is in use), not garbage.
- A counterintuitive tell we chased down: `-race` sometimes showed *less* peak
  than no-race for `cmd/peasant`, because `-race` serializes execution so fewer
  2 GiB arenas overlap at the peak instant (a 2.2–6.3 GB run-to-run swing).

**Rule.** A test that drives the ingest pipeline — or any production code that
pre-allocates a large buffer/pool sized for *throughput* — must inject a
**test-sized** value, not the production default. Use Go's `-memprofile` and
`pprof` commands above when investigating a regression locally; do not commit
machine-specific profiling captures.

### Test env-var overrides

Both knobs are **production** config env vars that `TestMain` shrinks to
test-sized values — the production code reads the env, the test sets it, so there
is **no test-only code path**. Both fall back to the production default when
unset, and both are set via `os.Setenv` (process-wide, so they do not mark any
test non-parallel).

| Env var | Constant | Production default | Test value | Why |
|---------|----------|--------------------|------------|-----|
| `PEASANT_DB_POOL_SIZE` | `store.EnvPoolSize` | `10` (`store.DefaultPoolSize`) | `1` cmd/peasant · `2` store | Avoid the default 10-connection pool (each re-parsing the schema) per `store.Open`. `internal/store` uses **2**, not 1 — pool=1 deadlocks its tests that take a 2nd connection while holding the 1st. |
| `PEASANT_INGEST_ARENA_BYTES` | `ingest.EnvArenaSizeBytes` | 2 GiB (`ingest.DefaultArenaSizeBytes`) | 64 MiB (`64*1024*1024`) | Avoid allocating the 2 GiB staging arena per pipeline run (the `-race` OOM). |
| `PEASANT_STORETEST_TMPDIR` | `storetest.EnvStoretestTmpDir` | (unset → `t.TempDir()` copies) | (unset) | Opt-in: route golden copies through a managed root (e.g. a macOS hdiutil RAM disk) with per-process shelves and dead-owner sweeping. `TMPDIR` already routes the default copies — `t.TempDir()` and `os.MkdirTemp("")` resolve through `os.TempDir()`, so `TMPDIR` alone moves every default copy. This override **additionally** scopes cleanup to storetest-owned `pid-*` shelves: they sit under a per-user, scheme-versioned managed root and a dead-owner sweep reclaims them on first use. The default `t.TempDir()` showed no measured copy-speed difference (the copy micro-measurement: 100 copies 36.6 ms vs 44.8 ms tmpfs — noise). |

Set in `cmd/peasant/main_test.go` (`PEASANT_DB_POOL_SIZE=1`, arena),
`internal/store/store_test.go` (`PEASANT_DB_POOL_SIZE=2`),
`internal/ingest/main_test.go` and `internal/api/main_test.go` (arena), and
`internal/e2e/main_test.go` (arena default). The resolvers (`store.resolvePoolSize`,
`ingest.resolveArenaSizeBytes`) take the env override only when it parses as a
positive integer, else the default.

The golden-template cache stamp keys on the schema version and a fingerprint of
the migration SQL. Connection pragmas and the salt-table DDL are outside that
fingerprint, so after editing one of them on a branch, delete `.testcache/`
(or bump `cacheScheme` in the helper) to keep a stale template from being
reused.

## Go WebSocket E2E (verified)

The project uses `github.com/coder/websocket` for Go E2E tests. Tests stand up a real `Hub` +
`httptest.Server`, connect via WebSocket, subscribe using `ChannelSubscription` structs, and
assert on the JSON payloads.

```go
// E2E test pattern — see internal/api/websocket_e2e_test.go for real examples
func TestHub_E2E_WebSocket(t *testing.T) {
    provider := &mockDataProvider{ /* ... */ }

    hub := NewHub(provider)
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go hub.Run(ctx)

    server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
    defer server.Close()

    // Connect via WebSocket
    wsURL := "ws://" + server.Listener.Addr().String()
    conn, _, err := websocket.Dial(ctx, wsURL, nil)
    require.NoError(t, err)
    defer conn.CloseNow()

    // Read "connected" message
    _, data, _ := conn.Read(ctx)

    // Subscribe using ChannelSubscription (typed struct, not bare channel name)
    conn.Write(ctx, websocket.MessageText, mustJSON(ClientMessage{
        Type: MsgSubscribe,
        Channels: []ChannelSubscription{
            {Topic: TopicQuality},
            {Topic: TopicAnnotations, Axis: AxisSession, ID: "session-123"},
        },
    }))
    _, data, _ = conn.Read(ctx)
    // Verify snapshot payload
}
```

Existing E2E tests in `internal/api/websocket_e2e_test.go`:
- `TestValidateSubscription` — fixture-driven cases loaded through the public schema module's `LoadAnnotationFixtures`
- `TestHub_Quality_EffectiveAnnotations` — quality channel round-trip asserting `effectiveAnnotations` wire format
- `TestHub_Annotations_InvalidSubscription` — error path for missing axis/id

## Committed fixture meta-tests (`make check`)

`internal/e2e/testdata/` holds synthetic, scrubbed transcript fixtures for Claude,
Codex, and Cursor. They are not copied from developer environments or user
transcripts. Each harness directory carries a `fixture-index.yaml` that declares
sessions, scrub pins, and (where applicable) slug-decode expectations.
Adding a new harness fixture is **data-first**: commit bytes + index YAML; the
generic meta-tests pick it up without new assertion helpers.

These tests are **untagged** — they run in `make check` on every build:

| Test | What it guards |
|------|----------------|
| `TestFixture_NoSecrets` | No tokens, emails, or non-synthetic home paths in any committed fixture file |
| `TestNoSecretsGate_DetectsKnownSecrets` | The no-secrets patterns are not vacuous |
| `TestFixture_StructureSanity` | Each `fixture-index.yaml` is complete; declared session files exist and match harness-specific shape pins |
| `TestFixtureIndex_CoverageFloors` | All three harnesses (`claude-code`, `codex`, `cursor`) and declared session kinds are represented |
| `TestFixture_SlugDecodeInvariants` | `fixture-index.yaml` `slug:` pins match `DecodeClaudeSlug` / `DecodeCursorSlug` |
| `TestFixture_CursorDiscover` | `CursorAdapter.Discover` on committed fixture bytes finds both sessions |
| `TestFixture_CursorIndexer` | Root session indexes with `tool_use` child entries |
| `TestFixture_MaximumDifferential` | Claude fixture: Standard leaves the code block in place and redacts inside it; Maximum additionally anonymizes the pinned identifier |

```bash
go test -race ./internal/e2e/ -run TestFixture   # all fixture meta-tests
```

**Cursor fixture note:** `cursor-fixture/` is covered by the meta-tests above
(including ingest bridge tests on committed bytes). It is also ingested in the
podman skip-gate harness (`make e2e`) alongside claude and codex. Provenance and
edit checklists: [`docs/e2e-fixture.md`](docs/e2e-fixture.md).

Harness-specific parsing (slug variants, tool-name casing, malformed-line skip)
stays in `internal/ingest` tests — not here.

## Cross-repo contract tests + gate-faithful expectations

When you change behavior that crosses the Peasant ↔
`github.com/peasant-labs/schema` ↔ Village contract (see
[`AGENTS.md`](AGENTS.md#data-and-contract-invariants)),
the tests must match the **system's real policy** and **couple the two repos** so they
can't drift. Lessons from prior end-to-end regressions:

- **Pin expectations to the gate, never fabricate data to hit a number.** The skip-gate
  e2e expects `push#1 == ExpectedPushTranscriptCount` (claude + codex = 4), **not** the
  ingest total (6): two cursor fixtures carry no `model`, so the client `ErrNoModel`
  gate correctly **holds** them. Giving them a placeholder model to force a "6" would
  *circumvent* the very gate the test should prove. Encode gate semantics in a **named
  constant** in `internal/e2e/fixture.go`, and **assert the held sessions are held for
  the right reason** (`assertCursorSessionsHeldForNoModel` checks the no-model error),
  not merely that the count dropped.

- **Couple cross-repo assertions on the error body, not just the status.** Two different
  rejections can share a status: the secret-scan 422 (`scanner.FormatScanErrors`) and a
  schema/enum 422 are both `422`. A peasant verdict that asserts only `http_status: 422`
  cannot tell them apart, so a regression that returns the *wrong* 422 still passes. Pin
  the peasant verdict's `error_contains` to the **exact** body string the village
  returns (e.g. `"value must be one of"`) — that one string is what keeps the producer
  test and the server behavior from drifting apart.

- **Don't `t.Skip` the safety net.** A test that `t.Skip`s when its validator/dependency
  is absent **self-disables exactly when the protection is gone**, and the skip reads as
  green. If the dependency is expected to exist in the test env (e.g. a `go:embed`-backed
  OpenAPI validator), `t.Fatal`/fail-closed instead, so a misconfiguration fails loudly.

- **Prove the test goes RED on the pre-fix tree.** An assertion can guard a *different*
  invariant than the bug you think it covers. (`assertAllSessionsHaveMetrics` guards
  "ingested ⇒ has metrics" — a real invariant, but the cursor sessions *did* get metrics;
  they were held later by the client `ErrNoModel` gate, so that assertion would **not**
  go red on a revert. The real regression guard was the `push#1` count + the held-reason
  assertion.) Revert the fix locally and confirm the new test fails before trusting it.

- **Reuse the existing fixture structure.** Add new expected values as named constants in
  `internal/e2e/fixture.go` / `fixture-index.yaml`; the generic meta-tests pick them up
  without new assertion helpers. Do not inline literals across test files.

## The publish-verdict corpus is canonical

The public `github.com/peasant-labs/schema` module embeds the single canonical
publish-verdict corpus and exposes it through `LoadPublishVerdictFixtures`. Each
case carries the request body, acceptance verdict, and the pinned error substring
for rejections. The module's own gates prove parity between generated and runtime
validation. Peasant's skip-gate end-to-end test consumes named cases through
`CaseByName`; it does not maintain a second copy.

Add or change a publish-validation case in the schema repository, run that
module's contract gates, publish a new module tag, and then update Peasant's exact
module pin. Do not hand-roll inline accept/reject tables or copy the corpus into
this repository, because either approach can drift from the validator Village
serves.

## Full-stack e2e (verified)

`internal/e2e/` also contains a **podman** harness, build-tagged `e2e`
(so it is OUT of `make check`). It provisions Postgres + RustFS (S3) + the **real
village `./cmd/server`** subprocess and drives the **real peasant CLI** in a
throwaway sandbox under `<resolved XDG_STATE_HOME>/peasant/test/e2e/<ts>` (the
real `~/.claude`, `~/.codex`, and `~/.local/share/peasant` are never touched —
that is asserted).

```bash
make e2e     # asserted: TestSkipGateE2E + TestPullRoundTripE2E + TestHarnessRefreshE2E
make demo    # TestSkipGateDemo only — unasserted, verbose ("watch it happen")
```

What `TestSkipGateE2E` proves end-to-end (claude + codex + cursor fixtures):
- ingest committed fixtures → `peasant annotate create` → `peasant village push`
  #1 publishes the full ingested batch + system annotations (K derived from the
  real ingest COMPUTE output, not hardcoded) → push #2 hits the server-manifest
  skip-gate → supersede one locally → push retracts the superseded annotation;
- **village-scan**: a DIRECT multipart `POST /api/v1/transcripts/publish` (bypassing
  the client's redaction so the check isn't vacuous) with a planted secret → **422**
  whose body is `scanner.FormatScanErrors`; a clean publish → 2xx.

`TestPullRoundTripE2E` (same tag, same prereqs) exercises auth-gated village pull,
annotations sync, and the pollution gate — see [`docs/e2e.md`](docs/e2e.md).

`TestHarnessRefreshE2E` seeds both Postgres and the object store, refreshes the harness-owned
warm stack, and proves the restarted Village can publish again with its
migration-owned license and governance-event reference rows intact.

Prerequisites:
- **podman** on `PATH` (the harness `t.Skip`s with guidance if absent);
- a **village checkout** providing `./cmd/server` + `./cmd/village-setup-demo` (a
  separate Go module, run as subprocess binaries);
- network access to pull the `quay.io/peasant-labs/postgres` and
  `ghcr.io/rustfs/rustfs` image (S3 operations use the in-process S3
  client).

Environment overrides:
- `VILLAGE_REPO` — village checkout (auto-discovered sibling checkout, or set
  explicitly; must provide `backend/cmd/server` and `backend/cmd/village-setup-demo`);
- `VILLAGE_BACKEND_DIR` — direct path to the village `backend/` module when layout differs;
- `VILLAGE_BIN` + `SETUP_DEMO_BIN` — pre-built binaries to skip the in-harness
  build (the checkout is still needed for the village-scan contract fixtures).

Full flow, sandbox/guards, env overrides, and fixture provenance:
[`docs/e2e.md`](docs/e2e.md) and [`docs/e2e-fixture.md`](docs/e2e-fixture.md).

## Guided TUI screenshot harness (manual prototype)

The repository contains an opt-in, manually invoked harness for visual review of the mounted
guided setup UI. It exercises the real production paths without replacing them with screenshot-only
test doubles: `settings.Flow` renders the guided settings sections, and `kickstart.Program` mounts
the selection states. The harness is a prototype, not an exhaustive terminal-page harness yet.

The capture source is the strict synthetic fixture at
`cmd/peasant-guided-screenshots/testdata/captures.yaml`. It supplies only scrubbed project and
session-shaped values, and the harness creates isolated temporary config files before mounting the
models. The loader rejects unknown fields, trailing YAML documents, duplicate or missing matrix
entries, and changed count declarations before any PNG is published. No local config, transcript,
repository, credential, or other user data is read into the evidence.

This harness is outside the default tests and builds. All command and test files are guarded by the
opt-in `guided_screenshots` build tag, so `go test ./...`, `make check`, normal `go build`/`make
build`, and the production Peasant binary do not compile or run it.

### Running the harness

Run the fixture and mounted-render tests explicitly:

```bash
make guided-screenshots-test
```

To generate PNG evidence, enter the Nix development shell first. It provides the `freeze` executable
through the `charm-freeze` package:

```bash
nix develop
make guided-screenshots
```

The normal capture requires a clean Peasant worktree so its directory name identifies the committed
source revision. During harness development, an intentionally disposable capture may be generated
from uncommitted changes with:

```bash
go run -tags=guided_screenshots ./cmd/peasant-guided-screenshots --allow-dirty
```

Dirty captures use a `-dirty` suffix in their directory name and must not be treated as commit
evidence. The command prints each output path and fails closed if the fixture, mounted render,
Freeze invocation, or PNG dimension check fails.

### Contact sheets and evidence review

The harness publishes six contact sheets under the commit-derived directory
`out/test/screenshots/peasant-guided-final-<commit>/`:

| File | PNG dimensions | Contents |
|------|----------------|----------|
| `guided-dark.png` | `1800x3420` | The six guided sections in the dark theme: auto-ingest, publication, privacy, license, destination, and retention. Each section includes `80x24` and `120x40` terminal renders. |
| `guided-light.png` | `1800x3420` | The same guided sections and terminal sizes in the light theme. |
| `selection.png` | `1800x6750` | The mounted selection view, including search, project/branch/session previews, and origin filtering. |
| `push.png` | `1800x6000` | The mounted publication wizard: start, selection, transcript preview, consent, and receipt. |
| `ingest-progress.png` | `1800x1200` | Local import progress after the guided configuration is saved. |
| `ingest-completion.png` | `1800x3420` | Local import completion with multiple actionable warnings at the top and bottom of the scrollable result, plus a no-warning control. Every state includes both themes and both terminal sizes. |

The selection fixture contains six synthetic sessions across two harnesses. One session is marked as
already ingested. One carries scrubbed transcript turns in the store. One carries a scrubbed harness
transcript that the harness wrote and the store does not hold. Its states exercise the mounted
hierarchy, global search, grouped repository and branch context, the stored session transcript
preview, and the preview of a session that Peasant has not imported yet, without depending on a
developer's repository or session history. The guided matrix contains 24 captures: six sections x
two themes x two terminal sizes.

The generated directory is local evidence and is ignored by git at `/out/test/screenshots/`; do not
commit the PNGs. Manually inspect every sheet after generation. Review both themes, both
terminal sizes, every guided section, and every selection state for clipped or overlapping content,
full-line styling, heading-first guidance, readable privacy before/after rows, usable selection
search, cursor-aligned project/branch/session previews, and blank or stale-looking panels. A passing
command proves fixture coverage and image dimensions; it does not replace this visual review.

Kickstart keeps nonfatal import warnings separate from error counts. Its completion view retains
the full session/location, reason, and remediation, sanitizes terminal controls, and keeps paging
and exit controls visible. The retained legacy receipt labels warning history across setup attempts;
an earlier warning may already be resolved. The CLI integration tests use synthetic native files
and a temporary SQLite store to prove the real runner forwards refusals without rewriting last-good
data; the screenshot harness proves the mounted presentation, not production-source discovery.

## TypeScript unit tests (verified)

TypeScript tests use Vitest and import from the same annotation fixture file (`web/src/test/fixtures/annotations.ts`):

```typescript
// web/src/types/messages.test.ts — subscription type system
import { subscriptionKey, acceptSubscription, subscribe, ChannelTopic } from './messages';

describe('subscriptionKey', () => {
  it('annotations key includes axis and id', () => {
    const sub = subscribe.annotations('session', 'sess-1');
    expect(subscriptionKey(sub)).toBe('annotations:session:sess-1');
  });
});

// web/src/lib/quality/types.test.ts — label derivation from annotation fixtures
import { HUMAN_OUTCOME_RESOLVED, AGENT_OUTCOME_RESOLVED } from '@/test/fixtures/annotations';
import { deriveLabels } from './types';

describe('deriveLabels', () => {
  it('extracts human label from annotations', () => {
    const { humanLabel } = deriveLabels([HUMAN_OUTCOME_RESOLVED]);
    expect(humanLabel).toBe('positive');
  });
});
```

Existing TypeScript test files:
- `web/src/types/messages.test.ts` — 25 tests: `subscriptionKey` (8), `subscribe` factory (8), `acceptSubscription` visitor dispatch (9)
- `web/src/lib/quality/types.test.ts` — 22 tests: `outcomeValueToLabel` (4), `deriveLabels` (9), `resolveOutcome` (8)

## Playwright/Puppeteer browser automation (unverified)

Prerequisites: `pnpm dlx playwright install` (or equivalent). No `web/e2e/` directory exists yet.

### Shell invocation

```bash
# Start server
./bin/peasant web start --port 9999 --foreground --no-browser &
sleep 2

# Run Playwright tests (assumes web/e2e/ test directory)
pnpm dlx playwright test web/e2e/

# Screenshot-based comparison
pnpm dlx playwright screenshot http://localhost:9999/ screenshots/dashboard.png
pnpm dlx playwright screenshot http://localhost:9999/sessions screenshots/sessions.png
pnpm dlx playwright screenshot "http://localhost:9999/sessions/detail?id=SOME_ID" screenshots/detail.png

./bin/peasant web stop --port 9999
```

### Example test patterns

These tests assume `data-testid` attributes exist on the frontend components. The actual
selectors will need to be verified against the Next.js source in `web/src/`.

```typescript
import { test, expect } from '@playwright/test';

test('dashboard loads real data via WebSocket', async ({ page }) => {
  await page.goto('http://localhost:9999/');
  // KPIs should populate from WS within 5s (ServerBroadcastTick)
  await expect(page.locator('[data-testid="total-sessions"]')).not.toHaveText('0', { timeout: 10_000 });
});

test('session detail shows trajectory', async ({ page }) => {
  await page.goto('http://localhost:9999/sessions');
  // Click first session
  await page.locator('tr').nth(1).click();
  // Trajectory timeline should render
  await expect(page.locator('[data-testid="transcript-timeline"]')).toBeVisible({ timeout: 10_000 });
});

test('quality scatter charts have colored points', async ({ page }) => {
  await page.goto('http://localhost:9999/');
  // Charts should have visible dots (not all default color)
  const dots = page.locator('.recharts-dot');
  await expect(dots.first()).toBeVisible({ timeout: 10_000 });
});
```

### What needs to happen first

1. Add `data-testid` attributes to key frontend components (KPI cards, chart containers,
   transcript timeline, session table rows)
2. Create `web/e2e/` directory with Playwright config
3. Verify the selectors against the actual DOM structure
4. Decide whether to run against real data or mock data (use `--mock-data-store` flags)
