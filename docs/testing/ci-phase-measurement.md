# CI phase measurement — the Go test lanes

The pipeline's wall-clock target is assessed on the **amd64 pool gate** (the
`check` job in `.github/workflows/tests.yml`). The **arm64 subset lane**
(`check-arm64`) is measured and reported but is **not** budget-gated: its
wall-clock variance on a 2-vCPU runner is not deterministically testable, and it
improves derivatively from the shared test-infrastructure work. The lane is
**release-only** (release PRs, release tags, and on-demand dispatches); ordinary
PRs do not run it, so its numbers come from release runs and dispatches. The
harvester
version guard and the CGO=0 job are not budget lanes and are out of scope here.

This document is the phase-measurement **plan and reading guide**. It exists so
the next real CI run can fill in the numbers below; it retires once CI exports
per-phase timings natively.

## Phase model

Each lane reports three phases separately:

| phase | what it covers | where it is observed |
|---|---|---|
| **pre-test** | job start through the last setup step, before the test command | `Phase clock start` stamp (`PHASE_START_S`) and the per-step `date` deltas written to `$GITHUB_STEP_SUMMARY` |
| **go test** | the test command itself | the test step's duration in the run timeline, and the step summary |
| **cache** | cache restore, then cache save | the `Restore Go build + modules cache (<arch>)` and `Save Go build + modules cache (<arch>)` step durations |

The test command additionally reports its own internals:

- **amd64 gate:** `make check` stamps `CHECK_START_NS` in the Makefile and the
  gate prints the in-`make check` pre-test wall (fmt / vet / ast-grep /
  release-guard), the plan (`go test -list`) wall, the per-pass walls, and the
  combined test wall. The CI step summary carries the outer job-level split.
- **arm64 lane:** the `make web-stub` step stamps `WEB_STUB_S`, so the Nix
  install, the web stub, and the gate's subset invocation
  (`cmd/testgate run -pkgs ./cmd/peasant/...,./internal/ingest/...`) can be
  separated. Both lanes upload their out dir — `report.json` and the
  per-package streams — as the `testgate-{amd64,arm64}-<run attempt>`
  workflow artifacts (7-day retention), so a failed or hung lane can be
  inspected offline (`cmd/testgate timing` reads a stream from stdin).

### Workflow changes that make the phases observable

- A `Phase clock start` step stamps `$GITHUB_ENV` at job start.
- Each cache use in the two budget lanes is split into an
  `actions/cache/restore` step and an `actions/cache/save` step, so each has its
  own named, timed step. The combined action's save is an untimed-by-name post
  step; `if: success() && steps.cache-go.outputs.cache-hit != 'true'` reproduces
  its `post-if: success()` save semantics exactly.
- The test steps write a phase table to `$GITHUB_STEP_SUMMARY`.

## Cache-key namespacing (a permanent fix)

`runner.os` is `Linux` on the amd64 pool, the arm64 runner, and the harvester
guard, and `actions/cache` `restore-keys` are **prefix** matches. The old shared
`${{ runner.os }}-go-` restore-key was therefore a prefix of
`Linux-go-arm64-<hash>` and `Linux-go-r1-<hash>`, so the amd64 gate could
restore an arm64 (or race-lane) tarball and then pay a **full cold rebuild** —
the restore phase reported a hit while the test phase absorbed the cost. Every
job now namespaces its key **and** its restore-key by job and architecture:

| job | before (key / restore-key) | after (key / restore-key) |
|---|---|---|
| `check` (amd64 pool gate) | `Linux-go-<hash>` / `Linux-go-` | `Linux-go-amd64-<hash>` / `Linux-go-amd64-` |
| `check-arm64` | `Linux-go-arm64-<hash>` / `Linux-go-` | `Linux-go-arm64-<hash>` / `Linux-go-arm64-` |
| `harvester-version-guard` | `Linux-go-r1-<hash>` / `Linux-go-` | `Linux-go-r1-<hash>` / `Linux-go-r1-` |
| `cgo0` | `Linux-go-cgo0-<hash>` / `Linux-go-cgo0-` | unchanged (already namespaced) |

**Measured delta: pending a real CI run.** The collision is a GitHub Actions
cache-namespace property; it cannot be reproduced on a single-architecture dev
box, so the before/after restore and cold-rebuild walls are only observable in
CI. What *is* provable here is the namespace logic: `Linux-go-amd64-` is not a
prefix of `Linux-go-arm64-` or `Linux-go-r1-`, and each restore-key now matches
only its own lane.

## Measured values

Provenance labels follow the repository's counting-method rule.

### CI (the numbers the budget is assessed against)

| value | lane / phase | provenance |
|---|---|---|
| ~9m30s | amd64 pool gate, whole `make check` at `RACE=0` | **carried, not re-verified here** (real pool runs) |
| 9m54s–10m31s | arm64 subset job wall on a 2-vCPU runner | **carried, not re-verified here** |
| 26–27 min | race-on whole suite on a **loaded 32-core dev box** | **carried**; explicitly **not** a pool/CI number |

### Local gate internals (loaded 32-core dev box; `RACE=0` runs)

These are the gate's own printed phase walls, **carried, not re-verified here**,
read from local gate reports. They are local, contention-inflated, and **not**
CI evidence.

| run | list (plan) wall | pre-test wall | combined test wall |
|---|---|---|---|
| `RACE=0`, 55 pkgs / 3642 tests | 2.677s | 5.275s | 512.5s (~8m33s) |
| `RACE=0`, 59 pkgs / 3671 tests | 7.532s | 10.957s | 625.8s (~10m26s) |
| `RACE=1` (context only) | 5.399s | 14.615s | 26m54s |

`RACE=1` is shown only to make the detector tax explicit; it is not comparable
to the pool numbers.

### Local cache state at the time of this report

- Go build cache (`$GOCACHE`) — 110 GB (shared across concurrent agent
  workloads on this box; not representative of CI).
- Go module cache (`$GOMODCACHE`) — 13 GB.

## Timeouts

| job | timeout | basis |
|---|---|---|
| `check` | **20 min** | measured ~9m30s amd64 pool gate + ~2x headroom; a wedging cap, not the budget check |
| `check-arm64` | **20 min** | measured 9m54s–10m31s + ~2x headroom; cap is wedging protection (lane is not budget-gated) |
| harvester guard, `cgo0`, notices, web, post-merge | unchanged | no measured value in this change; pending a real run |

The committed budget fixture (not the job timeout) is what fails a slow suite.

## Aggregate definition

> The **amd64 pool aggregate** is the workflow's wall from `determine-runner`
> start to the last amd64-pool job finishing, on an ordinary feature PR,
> excluding the release-only arm64 lane and the release-only jobs.

The amd64-pool jobs parallelize behind `determine-runner`, so the aggregate is
the **maximum** of their walls, not their sum. On a feature PR the gate is the
critical path; the license-notices, lockfile, and real-web-build jobs run
alongside it and finish sooner. The target is ≤5 min. The measured gate (~9m30s)
is currently **over** target; the test-suite performance work is what drives it
down, and this lane's phase report is the instrument that shows which phase
shrinks.

## Pending a real CI run

- cache restore and save walls, per lane, after the namespacing fix;
- go-test step wall per lane after the cache fix;
- pre-test step wall per lane;
- the end-to-end amd64 pool aggregate;
- the per-job walls of the non-gate pool jobs (notices, lockfile, web build).

This environment cannot trigger GitHub Actions, so **none** of the above is
invented or inferred here.

## Exit condition

The phase instrumentation (the clock steps and the step-summary tables) retires
once CI exports per-phase timings natively. The cache-key namespacing is
permanent.
