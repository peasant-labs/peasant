## Before / after evidence

**Summary:** Rebased onto current `develop` (`be2bc57f`, was 15 behind) with no conflicts — new head `d1f78111`, diff unchanged (15 files, +279/−28), pushed with `--force-with-lease`. On the same scoped gate run both sides, the single no-race pass wall fell from ~195s to ~75s (median of 2, ≈2.6× faster) with near-identical total CPU (~15s), the basis flipped from `serialized-per-invocation` to the pinned `pass-level-aggregate`, and the four-rule screen still proves every test ran exactly once on both sides. **Noise caveat:** these runs shared a 2-CPU VM with 42 other evidence jobs (load 20–36; the gate itself printed `INCONCLUSIVE — L=31 … this box is too loaded to quote a budget against`), so treat the absolute walls as noisy — the direction and the attribution/screen evidence are the load-independent parts.

### Scope and method

- `make check`'s full gate was impractical here, so both sides ran the gate exactly as TESTING.md documents for subset runs: `testgate run -race=false -pkgs <6 packages>` — the RACE=0 single no-race pass, plan + exactly-once screen included, `-p` = GOMAXPROCS = 2.
- Packages (identical both sides, none of them touched by this PR): `./internal/testkit/coveragemap`, `./internal/testkit/teststream`, `./internal/testkit/fsdecorator`, `./internal/metrics`, `./internal/auth`, `./internal/defaults` (6 of 92 repo packages; a subset run, not a full-suite gate result).
- Binaries were built once per side from the exact ref (`develop` vs the rebased PR head) and the testgate-reported **pass wall** is quoted (plan/list wall is recorded separately by the gate).
- BEFORE ref: `develop` `be2bc57f`. AFTER ref: rebased head `d1f78111`.

### Measured

| run | pass wall | per-class basis | CPU (user+sys) | screen | result |
|---|---|---|---|---|---|
| BEFORE rep 1 | 3m12.831s | `serialized-per-invocation` | 10.004s + 4.653s | all four rules passed | PASS |
| BEFORE rep 2 | 3m17.514s | `serialized-per-invocation` | 10.52s + 6.345s | all four rules passed | PASS |
| AFTER rep 1 | 1m17.494s | `pass-level-aggregate` | 9.705s + 5.240s | all four rules passed | PASS |
| AFTER rep 2 | 1m12.864s | `pass-level-aggregate` | 9.654s + 4.929s | all four rules passed | PASS |

Median pass wall: **BEFORE 195.2s → AFTER 75.2s**. Total CPU is the same work (~15s) either way — on `develop` the class wall equals the *sum* of the six per-invocation walls (the pass is serialized by `SerialPassB` even for RACE=0); on the PR head the class wall equals the *pass* wall while the per-invocation walls sum to ~152s, i.e. the invocations overlapped.

Attribution today (BEFORE): `cmd/testgate` hardcodes the no-race pass as `Serialized: true`, so a RACE=0 run labels its CPU `serialized-per-invocation` — per-invocation attribution for a pass that this PR makes concurrent. AFTER: `AttributionPasses` pins RACE=0 to `pass-level-aggregate` and keeps the RACE=1 partition pass at `serialized-per-invocation`, which the fixture in `testdata/attribution_modes.yaml` and `TestAttributionPassesMatchModes` lock in.

### Overlap proof (functional, load-independent)

The PR's rendezvous probe, run through the gate on the PR head:

```
testgate run -race=false -pkgs ./internal/testkit/testgate/overlapprobe/left,./internal/testkit/testgate/overlapprobe/right
→ single no-race wall: 3.568s · screen: all four rules passed · testgate: PASS
→ rendezvous dir contains both `left` and `right`
```

Each probe test waits for the other package's file and fails after 20s if it is not running concurrently, so a serialized single pass cannot pass this. The same rendezvous is exercised by `TestRunner_RACE0OverlapsAndAttributesPassLevel` in the package tests below.

### Tests on the rebased head

- `gofmt -l` on changed trees: clean. `go vet ./cmd/testgate ./internal/testkit/testgate ./internal/ingest`: exit 0.
- `go test -race -count=1 ./cmd/testgate`: **ok** (107.638s).
- `go test -race -count=1 -timeout 40m ./internal/testkit/testgate`: **ok** — includes `TestAttributionPassesMatchModes` and `TestRunner_RACE0OverlapsAndAttributesPassLevel`. (A first attempt with the default 10m test timeout panicked on timeout under VM load with the rendezvous test mid-run; the rerun with the same `-timeout 40m` the PR description uses passes.)
- Focused ingest tests for the files this PR touches (`TestOpenCodeUnknownNativeAndRetainedPersistence`, `TestCodexUnknownNativePersistence`, `TestPairRepairReingestsFromNative`, `TestProjectRetainedUnknownPrecedence`): **pass**. `TestOpenCodeNativeCLI` failed its first run only on its internal 3-minute "build Peasant" deadline under load (its own message: "Check build-cache availability and runner load, then rerun"); with the build cache warmed it **passes** on rerun. The full `go test -race ./internal/ingest` suite (771s unloaded per the PR description) was not re-run.
- Not run: the full `make check` / full-suite gate (13–15 min unloaded; not feasible under the parallel evidence load on this VM).

### Video and logs

![PR 758 evidence tour](GIF_URL)

- Video (asciinema cast + MP4): [pr758.cast](CAST_URL) · [pr758.mp4](MP4_URL)
- Full gate logs: [before-rep1.log](B1_URL) · [before-rep2.log](B2_URL) · [after-rep1.log](A1_URL) · [after-rep2.log](A2_URL) · [overlap.log](OV_URL) · [own-tests.log](OT_URL)

Media lives on branch `evidence/pr-758` in this repo.
