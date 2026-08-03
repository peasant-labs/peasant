# Local end-to-end harness — transcript + annotation skip-gate

This harness proves the **transcript + annotation skip-gate**, **retraction**, and
**server-side redaction scan** end-to-end against a **real village server**, real
**Postgres**, and real **MinIO (S3)** — all provisioned ephemerally — driven
through the **real peasant CLI**. It is the live demonstration for the full-stack
skip-gate and pull contracts.

> **See also:** [`TESTING.md`](../TESTING.md) — WebSocket E2E patterns, the
> always-on committed-fixture meta-tests (`make check`), and how this podman
> harness fits alongside them. Fixture provenance and edit checklists:
> [`docs/e2e-fixture.md`](e2e-fixture.md).

> For the architecture and authorization model this harness exercises, see
> [Village Pull Architecture](pull.md) and the [Peasant ↔ Village Auth
> Model](auth.md).

## What it does (`TestSkipGateE2E`)

1. **Sandbox** rooted at the resolved `XDG_STATE_HOME` (OS-compliant default, NOT
   hardcoded): `SANDBOX = <state>/peasant/test/e2e/<unix-ts>`. Every peasant
   subprocess runs with `XDG_{DATA,CONFIG,STATE}_HOME` under it. Startup prunes
   stale sandboxes at start; `t.Cleanup` removes the sandbox.
2. **Ephemeral infra** via `podman`: Postgres (`sslmode=disable`) + MinIO (random
   ports; bucket created and health-gated on a real S3 operation through the
   in-process minio-go client) + the **real village `./cmd/server`** subprocess wired `S3 → MinIO`,
   `DB → Postgres`, with a strong JWT.
3. **`village-setup-demo --local`** mints a demo user + API key → `credentials.json`
   in the sandbox config dir.
4. **A sandbox `config.yaml`** enables `claude-code`, `codex`, and `cursor` (each
   pointed at its own committed fixture), disables opencode, and sets
   `output.basePath` under the sandbox — so ingest reads ONLY the three fixtures
   and writes ONLY under the sandbox (the real `~/.claude`, `~/.codex`,
   `~/.cursor`, and `~/.local/share/peasant` are never touched). The codex source
   path is the fixture's `sessions/` dir ITSELF (`CodexFixtureSourcePath()`),
   since the codex adapter does a strict depth-4 `{root}/YYYY/MM/DD/rollout-*.jsonl`
   walk.
5. **`peasant ingest --include-active`** (the `--include-active` is load-bearing
   for determinism: a fresh checkout stamps the committed fixtures with a current
   mtime, so without it ingest debounces them under the 60s staleness hold and
   yields fewer than expected) → the committed fixtures become exactly the total
   pinned by `ExpectedTranscriptCount` in `internal/e2e/fixture.go`
   (`ExpectedClaudeTranscripts` + `ExpectedCodexTranscripts` +
   `ExpectedCursorTranscripts`). **`peasant annotate create`** adds a system-origin
   annotation per session; ingest's COMPUTE stage also generates system metric
   annotations.
6. **`peasant village push` ×3** — a **mixed claude+codex+cursor** manifest in
   one cycle:
   - push#1 → **ExpectedPushTranscriptCount transcripts + K annotations** (K derived
     from the CLI's report, not hardcoded). Cursor fixture sessions count toward
     `ExpectedTranscriptCount` but are held by the push `ErrNoModel` gate because
     they omit `message.model`,
   - push#2 → **0 transcripts + 0 annotations** (K skipped via the server manifest),
   - supersede one annotation locally → push → **exactly 1 retracted**.
7. **Village-scan** (direct multipart POST, bypassing client redaction): a clean
   `transcript_file` → 2xx; one carrying a planted secret → **422** whose body is
   `scanner.FormatScanErrors`.
8. **Guards**: the state-dir invariant and resolved DB path under the sandbox in the
   subprocess env) and the **real data dir asserted unchanged** (dir + `peasant.db`
   existence/mtime) setup→teardown.

Plus `TestFixture_MaximumDifferential` (untagged, in `make check`): redaction at
Standard leaves the claude fixture's code block in place and runs the rules over
it, so structure and the pinned identifier both survive and matched tokens are
replaced; Maximum additionally AST-anonymizes the identifier.

## Pull round-trip + pollution gate (`TestPullRoundTripE2E`)

A second `e2e`-tagged harness reuses the same provisioning machinery
(podman Postgres + MinIO + the real village `./cmd/server` + the real peasant CLI
in throwaway XDG sandboxes) and adds a **second village user** to prove the
auth-gated `peasant village transcripts {pull,list,context}` +
`village annotations sync` surface end-to-end. The second user is minted by the
**sibling-mint path** (`internal/e2e/village_users.go`): `users` + `api_keys`
rows inserted directly into ephemeral Postgres through `database/sql` and the pgx
stdlib driver, with every value carried by a `$N` bind parameter. The API key uses
the exact Village production format
(`peasant_`+hex(32), sha256 hash, 8-char prefix) so the village's
`AuthRequired`/`GetAPIKeyByHash` accepts it identically. **DRIFT WARNING:** this
format duplicates the village's `auth.GenerateAPIKey` (a separate, non-importable
module); if the village ever changes its key prefix/length/hash, `generateVillageAPIKey`
must be updated in lockstep — otherwise `AuthRequired` silently 401s every minted key
and the pull/sync/list phases fail at the village boundary (a 401, not an obvious
format error). A mint-time format assertion guards against accidental local drift.
The sibling Village checkout is never modified. Group sharing and the public
visibility seed both drive the real authenticated Village API as the transcript
owner; after each visibility mutation, the harness reads the trigger-written
governance event and asserts `visibility_changed`, the resulting visibility, and
`changed_by` equal to the owner's UUID. The one governed direct-SQL exception is
the pre-envelope legacy transcript that the current publish API cannot represent;
it runs in an owner-attributed transaction and has its audit actor asserted. Group
sharing must happen before user2 annotates because `CreateTranscriptAnnotation`
gates on `canViewTranscript`.

It drives the anchored validation set through the real CLI in two sandboxes:

- **logged-out** pull / remote list / annotations sync ⇒ non-zero exit + an
  actionable error naming `peasant village login`, NOTHING written locally;
  `list --local` and `context` WORK offline;
- **unauthenticated** raw API ⇒ **401** on each `/api/v1/pull` route;
- **pull by bare UUID and by pasted web URL** ⇒ `village-pulls/{host}/{id}/`
  (transcript + metadata + pull-manifest) + the V34 `pulled_transcripts` /
  `pulled_annotations` rows; **re-pull** ⇒ `up-to-date` with no content rewrite;
  **--force** re-downloads; group-shared pull succeeds; a **PUBLIC-but-unshared**
  transcript ⇒ not-found, no local writes (MVP excludes public from pull);
- **annotations sync** surfaces user2's annotation on user1's transcript,
  foreign-marked WITH author identity, EXCLUDING user1's own;
- the **pollution gate** (table-level): user1's `sessions` / `session_entries` /
  `session_metrics` / `annotations` are row-identical before/after all pulls, the
  `peasant-sync/` tree is byte-identical, and `push --dry-run`'s candidate set is
  unchanged — pulled artifacts never re-enter ingest/analytics/push;
- **sandbox isolation** asserted (local harness and XDG directories untouched).

It runs as part of `make e2e` (same build tag, same prereqs).

## Warm-stack refresh (`TestHarnessRefreshE2E`)

The refresh regression publishes into a non-empty harness-owned Postgres and
MinIO stack, resets mutable application state, restarts Village, and publishes
again. It proves refresh preserves migration-owned reference data such as the
license and governance-event menus while clearing transcript rows and objects.
An explicit table classification fails closed when a future migration adds an
unclassified public table.

The staged pull flow, the idempotency/304 fast-paths, and the
compensation/cache doctrine this harness asserts are documented in
[Village Pull Architecture](pull.md) (§2–§4, §6.3 storage invariants); the
logged-out/offline auth-gate cells and the `canPullTranscript` divergences are in
the [Peasant ↔ Village Auth Model](auth.md) (§3, §4).

## Prerequisites

- **podman** on `PATH` (`t.Skip`s with guidance if absent).
- A **village checkout** providing `./cmd/server` + `./cmd/village-setup-demo`
  (a separate Go module — run as subprocess binaries). `t.Skip`s if absent/unbuilt.
- Network access to pull the `postgres` and `minio/minio` images. (S3 operations
  run in-process via the **minio-go** client — no `minio/mc` container is pulled.)

## Running

```bash
make e2e    # asserted: skip-gate, pull round-trip, and warm-stack refresh
make demo   # TestSkipGateDemo only, unasserted and verbose
```

Both are **out of `make check`** (build-tagged `e2e`), but the **always-on
fixture meta-tests** over ALL committed fixtures (claude, codex, **and cursor**)
DO run in `make check` — see [`TESTING.md` → Committed fixture meta-tests](../TESTING.md#committed-fixture-meta-tests-make-check).

### Crash cleanup and CI concurrency

> **Counter regression note.** An earlier version of this
> doc claimed "per-run **uniqueness** — not the reaper — is the flake fix." That
> was **wrong**. The actual failure was a **counter bug**: the seeded-baseline
> S3 count line-counted `mc`'s `CombinedOutput()` (stdout **+ stderr**), and CI
> podman 4.9.3 emits ~8 cgroup-manager stderr warnings per `podman run`, so an
> empty bucket counted as 8 and `assertSeededBaselineBeforePush` failed (want 0).
> It was fixed by counting via the in-process **minio-go** client (typed
> `ObjectInfo`, no text parsing) — see [Podman version parity](#podman-version-parity-local--ci).
> Per-run uniqueness, the Go reaper, and the Pdeathsig/killpg village teardown are
> **orthogonal hygiene** (real leftover/orphan protection) — independently correct,
> but they neither caused nor fixed this flake.

Per-run **uniqueness** remains load-bearing hygiene: every run gets a unique
sandbox path and S3 bucket/container name, so a live sibling run does not share
state with this run. The Go reaper is only a garbage collector for already-stopped
leftovers from hard-crashed runs; it must never be the mechanism that makes active
runs safe.

The discriminators for deleting podman infra are terminal status plus age, not PR
number or workflow run id. PR/run scoping is the wrong axis because stale
containers can outlive the run that created them, while an unrelated live run can
briefly create a same-prefix container. The bash workflow cleanup therefore only
removes terminal `exited`/`stopped` containers (NOT `dead` — podman 4.9.3 rejects
that Docker-only state, which broke the cleanup; see Podman version parity below).
Age gating belongs in the Go reaper
(`staleE2ETTL`, 24h), where the parser sees podman status and generated timestamp
together; duplicating that policy in bash would create a second cleanup contract.

Cleanup safety does not rely on a particular hosted-runner reuse model. A newly
created sibling container is not stale, so cleanup never treats `status=created`
as removable.

### Podman version parity (local ↔ CI)

The CI runner (`blacksmith-4vcpu-ubuntu-2404-arm`, Ubuntu 24.04) runs **podman
4.9.x**. The e2e workflow **pins** this with a `Verify and pin podman version` step
that hard-fails if the runner's podman major.minor drifts from `4.9` — forcing a
lockstep update of this doc when the runner image changes.

Parity matters because the counter bug was **only**
reproducible under podman 4.9.x: that build emits ~8 cgroup-manager **stderr**
warning lines per `podman run` (rootless, no systemd user session). The original
`transcriptBucketObjectCount` line-counted `mc`'s `CombinedOutput()` (stdout **+
stderr**), so an empty bucket counted as 8 and the seeded baseline failed (want 0).
**Local podman 5.x emits none of those lines**, so it counted 0 and passed — the
exact false-confidence (10× green locally) that hid the bug. The same 4.9.x-vs-5.x
gap hid the `status=dead` filter break. The real fix is in-process **minio-go**
(typed `ObjectInfo`, no text parsing); parity is the backstop so a future CI-only
divergence is reproducible locally instead of only on a view-only Blacksmith console.

**One-command local repro at CI parity** (run from the repo root; substitute your
village checkout). It fails fast if your local podman is not 4.9.x — a 5.x box will
NOT reproduce the cgroup-stderr shape:

```bash
podman version --format '{{.Client.Version}}' | grep -q '^4\.9\.' \
  && VILLAGE_REPO=/path/to/village make e2e \
  || echo "local podman is not 4.9.x; install podman 4.9.x (e.g. an Ubuntu 24.04 box/VM) to reproduce the CI podman shape"
```

To obtain a matching podman without a 24.04 host, run the suite on an Ubuntu 24.04
VM/container (whose apt `podman` is 4.9.x). The in-process minio-go count is
stderr-immune regardless of podman version, so on a 4.9.x box the focused
`TestTranscriptBucketObjectCountMatchesKnownPuts` and the seeded baseline both stay
green with the fix and (the baseline) RED without it.

### Environment overrides

| Variable | Default | Meaning |
|----------|---------|---------|
| `VILLAGE_REPO` | auto-discovered sibling checkout, otherwise unset | Village checkout; the harness builds `backend/./cmd/{server,village-setup-demo}`. It searches nearby `village/` and `village/develop` sibling checkouts; if none exist, it `t.Skipf`s with guidance. Set this explicitly on machines with a different layout. |
| `VILLAGE_BACKEND_DIR` | _(unset)_ | Direct path to the village `backend/` module. Useful when the checkout layout is not `<repo>/backend`. |
| `VILLAGE_BIN` + `SETUP_DEMO_BIN` | _(unset)_ | Pre-built binaries; both set skips the in-harness build (the checkout is still needed for the village-scan contract fixtures). |

## Notes / scope

- **Verified green** on a full-stack box (podman 5.8.2): mixed claude+codex+cursor
  batch to MinIO, K system annotations, push#2 skip, retraction=1, village-scan
  422 + clean 2xx, plus an unknown-harness direct publish rejected by schema.
- Fixture bytes and provenance — see [`docs/e2e-fixture.md`](e2e-fixture.md). The
  **claude** fixture is fully synthetic; the other provider-shaped samples are
  sanitized and guarded against secrets and personal paths.
- CI runs all three asserted E2E tests against the pinned Village checkout and
  fails if any asserted test skips or lacks a positive PASS line.
