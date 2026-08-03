# Testing Patterns

Code examples and strategies for testing the Peasant web dashboard and WebSocket protocol.
See `AGENTS.md` for the test package map, fixture tree, writing rules, and channel reference table.

### E2E documentation map

| Layer | Runs in | Entry point |
|-------|---------|-------------|
| WebSocket hub E2E | `make check` | [Go WebSocket E2E](#go-websocket-e2e-verified) (below) |
| Committed transcript fixtures (`internal/e2e/testdata/`) | `make check` | [Fixture meta-tests](#committed-fixture-meta-tests-make-check) + [`docs/e2e-fixture.md`](docs/e2e-fixture.md) |
| Full-stack skip-gate + pull round-trip (podman + village + real CLI) | `make e2e` only | [Full-stack e2e](#full-stack-e2e-verified) + [`docs/e2e.md`](docs/e2e.md) |

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
`internal/ingest` `TestMain`s. Result: `cmd/peasant -race` 2173–6267 MB →
**240 MB**, `ingest` 6185 → **274 MB**; full `make check -race` runs ~26s and
fits a **2-vcpu** runner (so the per-PR job stays a plain `make check`, no split).

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

Set in `cmd/peasant/main_test.go` (`PEASANT_DB_POOL_SIZE=1`, arena),
`internal/store/store_test.go` (`PEASANT_DB_POOL_SIZE=2`), and
`internal/ingest/main_test.go` (arena). The resolvers (`store.resolvePoolSize`,
`ingest.resolveArenaSizeBytes`) take the env override only when it parses as a
positive integer, else the default.

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
(so it is OUT of `make check`). It provisions Postgres + MinIO (S3) + the **real
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

`TestHarnessRefreshE2E` seeds both Postgres and MinIO, refreshes the harness-owned
warm stack, and proves the restarted Village can publish again with its
migration-owned license and governance-event reference rows intact.

Prerequisites:
- **podman** on `PATH` (the harness `t.Skip`s with guidance if absent);
- a **village checkout** providing `./cmd/server` + `./cmd/village-setup-demo` (a
  separate Go module, run as subprocess binaries);
- network access to pull the `postgres` and `minio/minio` images (S3 operations
  use the in-process minio-go client).

Environment overrides:
- `VILLAGE_REPO` — village checkout (auto-discovered sibling checkout, or set
  explicitly; must provide `backend/cmd/server` and `backend/cmd/village-setup-demo`);
- `VILLAGE_BACKEND_DIR` — direct path to the village `backend/` module when layout differs;
- `VILLAGE_BIN` + `SETUP_DEMO_BIN` — pre-built binaries to skip the in-harness
  build (the checkout is still needed for the village-scan contract fixtures).

Full flow, sandbox/guards, env overrides, and fixture provenance:
[`docs/e2e.md`](docs/e2e.md) and [`docs/e2e-fixture.md`](docs/e2e-fixture.md).

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
