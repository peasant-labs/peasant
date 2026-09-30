# Performance evidence: before/after per fix class

Row shape (every row): test → class → exact command → before wall/CPU →
after wall/CPU → L (with the companion command that produced L). Walls are
Class A focused runs unless noted; no wall is quoted from a profile run. Base
SHA for the before column: `da7abd7f` unless a row says otherwise. Each fix
change appends only its own section; the integration change is the final editor.

Focused command (quiet box, one discarded warmup, serial):

```
<time> go test -race -count=1 -timeout=0 -run '^<Test>$' ./<pkg>
```

`<time>` is GNU time with `-v`. `L` is the gate's printed calibration for the
run window (`L > 4` discards the run). Never pin `-parallel`.

## Baseline (survey)

Lens-F baseline walls for the measured set, at the base SHA above. These are
the before column for every fix-class section that follows. Command per row:
`go test -race -count=1 -timeout=0 -run '^<Test>$' ./<pkg>` after one discarded
warmup, serial, L=0.957 (base-gate calibration), GOMAXPROCS=32.

| test | class | lens-F wall (s) | user (s) | sys (s) | L |
|---|---|---|---|---|---|
| `TestUnknownLocalRetentionBeyondTransferBudget` (ingest) | T1-shape | 229.9 | 378.8 | 26.6 | 0.957 |
| `TestNativeUnknownSourceToPublication` (ingest) | T3 | 106.6 | 96.4 | 3.5 | 0.957 |
| `TestPiCapturedAdmission` (ingest) | T3 | 78.0 | 73.9 | 2.2 | 0.957 |
| `TestPublicationCaptureNormalIngestRecovery` (ingest) | T3 | 75.7 | 68.2 | 2.1 | 0.957 |
| `TestPiNativeRegistryProjection` (cmd) | T3+T4 | 53.2 | 49.0 | 1.9 | 0.957 |
| `TestNormalIngestStoresAuthoritativeContent` (ingest) | T3 | 43.5 | 40.1 | 1.5 | 0.957 |
| `TestPiUnknownPersistence` (ingest) | T3+T4 | 43.1 | 40.2 | 1.6 | 0.957 |
| `TestResolveStoredOriginsWritesAVerdictIntoEveryRow` (ingest) | T3 | 41.1 | 38.0 | 1.3 | 0.957 |
| `TestPiDatabasePublicationThroughCLI` (cmd) | T4 | 35.4 | 33.4 | 1.6 | 0.957 |
| `TestOrdinaryHarvestSettlesStaleIndexSessions` (ingest) | T3 | 31.0 | 28.5 | 1.3 | 0.957 |
| `TestRetainedContentBackfill` (ingest) | T3 | 28.3 | 26.0 | 1.2 | 0.957 |
| `TestNativeCoverageMatrix` (ingest) | T3 | 26.4 | 21.9 | 1.2 | 0.957 |
| `TestPiHarvestCommonModes` (cmd) | T3+T4 | 22.3 | 19.8 | 1.5 | 0.957 |
| `TestPipelineRetainedAdapterMaintenance` (ingest) | T3 | 18.0 | 16.1 | 1.2 | 0.957 |
| `TestContentRecoveryScope` (ingest) | T3 | 17.8 | 15.8 | 1.0 | 0.957 |
| `TestMountedKickstartStoredGateAlignsViewerAndPush` (cmd) | T3 | 17.1 | 14.4 | 1.2 | 0.957 |
| `TestLegacyOpenCodeSQLiteSourceInfoRecoveryValidatesManagedEnvelope` (cmd) | T3 | 14.7 | 12.0 | 1.4 | 0.957 |
| `TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection` (cmd) | T3 | 14.1 | 12.4 | 1.1 | 0.957 |
| `TestUnknownPrivateEncoding` (ingest) | T3 | 14.0 | 12.2 | 1.0 | 0.957 |
| `TestMountedLegacySelectedConversion_ConsentCancellationAndRerun` (cmd) | T3 | 13.6 | 11.2 | 1.1 | 0.957 |
| `TestConcreteParserFailurePreservesOtherSessions` (ingest) | T3 | 13.1 | 11.6 | 0.9 | 0.957 |
| `TestModelsSync_500_StaticFallback` (cmd) | T2 | 10.2 | 8.9 | 1.1 | 0.957 |
| `TestKickstartLocalIngestPreservesCommittedSelectionAtRunnerBoundary` (cmd) | T3+T4 | 9.8 | 8.1 | 1.0 | 0.957 |

Carried reference (superseded by the measured baseline above): retention test
243.6 s; #1 93.0 s; #2 91.0 s; #3 51.9 s; #4 48.2 s; #8 61.4 s; #10 25.3 s;
#11 19.3 s (all focused-and-alone, race, prior reference measurement).

## T3 (+T4 #6) — ingest golden pre-migrated DB + skip conversions

After state: the fourteen ingest files named in this change route every
`store.Open` onto the golden template (`storetest.OpenWith` for whole-life
stores; `storetest.CopyGoldenDB` + `store.Open(path,
store.WithSkipMigrations(), ...)` for paths reopened across close/reopen; the
three already-golden opens gained the skip option). No assertion changed; the
`content_backfill_test.go` seed loop is untouched. Each after cell is one
discarded warmup + one measured Class A run, serial, GOMAXPROCS=32:

```
<time> go test -race -count=1 -timeout=0 -run '^<Test>$' ./internal/ingest
```

`<time>` here is `/run/current-system/sw/bin/time -v` (`/usr/bin/time` is
absent in this container). Before column = the survey baseline above
(L=0.957). L companions for this window: 0.945 at start, 0.951 at end
(`RACE=0 go run ./cmd/testgate run -pkgs
./internal/testkit/coveragemap,./cmd/testgate -race=false`).

| test | class | before wall / user / sys (s) | after wall / user / sys (s) | L |
|---|---|---|---|---|
| `TestUnknownLocalRetentionBeyondTransferBudget` (ingest) | T3 | 229.9 / 378.8 / 26.6 | 210.1 / 347.3 / 25.3 | 0.945–0.951 |
| `TestNativeUnknownSourceToPublication` (ingest) | T3 | 106.6 / 96.4 / 3.5 | 99.3 / 90.7 / 3.3 | 0.945–0.951 |
| `TestPiCapturedAdmission` (ingest) | T3 | 78.0 / 73.9 / 2.2 | **warm 19.06 / 16.05 / 1.39** (L1 3.22; cold record: 82.0 / 76.0 / 2.2 (rerun 82.0) — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestPublicationCaptureNormalIngestRecovery` (ingest) | T3 | 75.7 / 68.2 / 2.1 | **warm 33.88 / 27.76 / 1.79** (L1 4.88; cold record: 79.2 / 71.1 / 2.4 (rerun 78.8) — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestNormalIngestStoresAuthoritativeContent` (ingest) | T3 | 43.5 / 40.1 / 1.5 | **warm 16.30 / 13.01 / 1.09** (L1 5.10; cold record: 47.2 / 41.7 / 1.7 (rerun 45.9) — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestPiUnknownPersistence` (ingest) | T3+T4 | 43.1 / 40.2 / 1.6 | 41.7 / 38.8 / 1.5 | 0.945–0.951 |
| `TestResolveStoredOriginsWritesAVerdictIntoEveryRow` (ingest) | T3 | 41.1 / 38.0 / 1.3 | **warm 8.95 / 6.80 / 0.92** (L1 6.41; cold record: 44.3 / 39.8 / 1.4 (rerun 42.9) — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestOrdinaryHarvestSettlesStaleIndexSessions` (ingest) | T3 | 31.0 / 28.5 / 1.3 | **warm 7.95 / 6.34 / 0.92** (L1 5.65; cold record: 33.3 / 29.9 / 1.4 (rerun 32.7) — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestRetainedContentBackfill` (ingest) | T3 | 28.3 / 26.0 / 1.2 | **warm 12.12 / 11.03 / 0.89** (L1 4.85; cold record: 28.0 / 26.2 / 1.2 — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestNativeCoverageMatrix` (ingest) | T3 | 26.4 / 21.9 / 1.2 | 24.9 / 21.1 / 1.1 | 0.945–0.951 |
| `TestPipelineRetainedAdapterMaintenance` (ingest) | T3 | 18.0 / 16.1 / 1.2 | **warm 5.44 / 4.07 / 0.90** (L1 4.71; cold record: 18.4 / 16.4 / 1.1 — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestContentRecoveryScope` (ingest) | T3 | 17.8 / 15.8 / 1.0 | **warm 4.85 / 3.65 / 0.73** (L1 4.59; cold record: 17.9 / 15.9 / 0.9 — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestUnknownPrivateEncoding` (ingest) | T3 | 14.0 / 12.2 / 1.0 | **warm 5.52 / 4.34 / 0.74** (L1 4.34; cold record: 15.2 / 13.3 / 0.9 (rerun 15.3) — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestConcreteParserFailurePreservesOtherSessions` (ingest) | T3 | 13.1 / 11.6 / 0.9 | **warm 3.62 / 2.47 / 0.73** (L1 3.90; cold record: 13.5 / 11.8 / 0.9 — cache state changed (cold -> warm)) | 0.945–0.951 |
| `TestContentStageDetectsTornPair` (ingest) | T3 | — (no baseline row; converted with its file) | **warm 2.60 / 1.63 / 0.71** (L1 3.77; cold record: 4.3 / 3.3 / 0.7 — cache state changed (cold -> warm)) | 0.945–0.951 |

Reading the table. Every converted test passes focused race runs (hence
validates the conversion) with goleak clean via the package `TestMain`. The
many-open tests improve (#0 −9%, #9 −7%, #25 −6%, #6 −3%); the few-open tests
sit within ±8% of the survey-day baseline with same-day repeats stable to
±2% — i.e. migration-apply on an empty DB is cheap in lens F and the per-test
lens-F effect of T3 is small in both directions. No case was reverted: nothing
regressed beyond inter-day variance, and the class's aggregate win (many
parallel opens across the suite) is measured at integration, not here.

T4 #6 row: the `TestPiUnknownPersistence` padding strings (15 cases ×
`paddingBytes: 12000`, all identical) are built once (`strings.Repeat("z",
max)`) and sliced read-only per case; Go strings are immutable so no case can
mutate another's padding. Invariant sizes unchanged: 12000 B per case before
and after; the per-case JSON marshal and source replacement stay per case.

Assertion audit (no assertion changed — the conversion diff touches only
store-open lines, imports, and the padding share): the `stale_index_settling`
freshness assertions (producer revision, capture state/format/failure-code,
stored entries, second-harvest "no parser work, no reads, no diagnostics")
all pass on the golden store; `publication_capture` timing/provenance
assertions (`Ingested`, CWD, project/host identity derived from the same
store's installation salt — the salt-consistency check) pass; no converted
file asserts a migration-path-only refusal or a store-path `NotExist`; the two
`IsNotExist` hits in the converted files concern managed-output sidecars, not
store paths. Migration coverage stays in the dedicated `internal/store`
migration tests, untouched by this change.
Warm re-measure (cache state changed (cold -> warm)): the bold warm cells above were
re-measured on integration head `8e623e39` (template cache and store-open seam
conversions merged), same command shape, one discarded warmup per test, serial,
nproc 32; `L1` is the 1-minute `/proc/loadavg` before the measured run. The
prior value in each cell is kept as the cold record.

## Prepared-path conversion (cmd suites)

Change: dry-run fixtures and every remaining cmd-suite database setup start
from a copy of the pre-migrated golden database on the exact path the command
opens, so the version check finds no pending migrations; test-owned opens
take the skip-migrations option on the at-head copy, production command opens
are untouched. Large fixture payloads are built once per test and shared
read-only across subtests (no fixture size reduced). No assertion changed; no
NotExist expectation seeded.

After-SHA: `4e21be8a` (survey merge over the conversion commits).
After command per row (quiet box, one discarded warmup, serial,
GOMAXPROCS=32):
`nix develop --command /run/current-system/sw/bin/time -v go test -race
-count=1 -timeout=0 -run '^<Test>$' ./cmd/peasant`.
L companion
(`nix develop --command go run ./cmd/testgate run -pkgs
./internal/testkit/coveragemap,./cmd/testgate -race=false`):
L=0.928 at window start, L=0.952 at window end.

| test | class | before wall / user / sys (s) | after wall / user / sys (s) | L |
|---|---|---|---|---|
| `TestPiNativeRegistryProjection` (cmd) | T3+T4 | 53.2 / 49.0 / 1.9 | **warm 9.60 / 7.79 / 1.16** (L1 3.45; cold record: 53.95 / 49.52 / 1.77 — cache state changed (cold -> warm)) | 0.928–0.952 |
| `TestPiDatabasePublicationThroughCLI` (cmd) | T4+T3 | 35.4 / 33.4 / 1.6 | **warm 33.46 / 31.85 / 1.27** (L1 3.08; cold record: 37.62 / 35.76 / 1.31 — cache state changed (cold -> warm)) | 0.928–0.952 |
| `TestPiHarvestCommonModes` (cmd) | T3+T4 | 22.3 / 19.8 / 1.5 | **warm 6.46 / 4.81 / 1.17** (L1 2.83; cold record: 22.31 / 19.72 / 1.43 — cache state changed (cold -> warm)) | 0.928–0.952 |
| `TestMountedKickstartStoredGateAlignsViewerAndPush` (cmd) | T3 | 17.1 / 14.4 / 1.2 | **warm 4.13 / 2.39 / 0.86** (L1 2.86; cold record: 16.55 / 14.20 / 1.04 — cache state changed (cold -> warm)) | 0.928–0.952 |
| `TestLegacyOpenCodeSQLiteSourceInfoRecoveryValidatesManagedEnvelope` (cmd) | T3 | 14.7 / 12.0 / 1.4 | **warm 6.48 / 4.31 / 1.26** (L1 4.19; cold record: 14.29 / 11.65 / 1.35 — cache state changed (cold -> warm)) | 0.928–0.952 |
| `TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection` (cmd) | T3 | 14.1 / 12.4 / 1.1 | **warm 3.28 / 2.14 / 0.82** (L1 3.85; cold record: 14.35 / 12.55 / 0.96 — cache state changed (cold -> warm)) | 0.928–0.952 |
| `TestMountedLegacySelectedConversion_ConsentCancellationAndRerun` (cmd) | T3 | 13.6 / 11.2 / 1.1 | **warm 3.49 / 2.02 / 0.85** (L1 3.86; cold record: 13.48 / 11.30 / 1.03 — cache state changed (cold -> warm)) | 0.928–0.952 |
| `TestKickstartLocalIngestPreservesCommittedSelectionAtRunnerBoundary` (cmd) | T3+T4 | 9.8 / 8.1 / 1.0 | **warm 3.30 / 2.14 / 0.86** (L1 3.63; cold record: 9.88 / 8.32 / 0.91 — cache state changed (cold -> warm)) | 0.928–0.952 |

Before column = the survey baseline above (base `da7abd7f`, L=0.957).
All eight converted tests pass focused race runs; every delta is within
run-to-run noise (worst +6.3 %, best −3.2 %).

T4 invariant sizes (before → after, unchanged; each pinned by the named
assertion):

| payload | size | pinning assertion |
|---|---|---|
| publication repetitions | 4200 → 4200 | shared-payload repetition count in `TestPiDatabasePublicationThroughCLI` |
| publication payload text | 84085 B → 84085 B | redaction + tail containment over the published parts |
| native boundary metadata | 51175 B, 65537 B → unchanged | selected-subtree byte assertion per case |
| native boundary padding | 1404 B → unchanged | same selected-subtree assertion |
| selected metadata subtree | 52620 B → unchanged | `SelectedMetadataBytes` equality per case |
| harvest long-text expansion | 84000 B (4000 reps) → unchanged | per-case overlay repetition count |

Mechanism check (profile run of `TestPiDatabasePublicationThroughCLI`,
Class B, wall not quoted): `go tool pprof -top` shows no
`sqlitemigration.Migrate` node in the top 100 — the per-open migration
replay is gone. `store.Open` totals 3.63 s cum (10.21 %), of which 3.47 s
is the single per-process golden-template build (`storetest.ensureGolden`);
all remaining opens (harvest + read-back) share ~0.16 s. The focused delta
stays ~neutral because the template build replaces the per-open replays
inside one process; the persistent-template cache now in design is what
removes that remaining one-per-process cost. The detector still dominates
the profile (`racecall` 38.64 % flat), as expected with the partition
deferred.
Warm re-measure (cache state changed (cold -> warm)): the bold warm cells above were
re-measured on integration head `8e623e39` (template cache and store-open seam
conversions merged), same command shape, one discarded warmup per test, serial,
nproc 32; `L1` is the 1-minute `/proc/loadavg` before the measured run. The
prior value in each cell is kept as the cold record.

## Screening addition: store publication test (T3 seam conversion, new territory)

`TestPublicationFullCaptureEligibilityAndBundle` (`internal/store`, 20 fixture
cases) copied the golden DB per subtest but opened it without
`store.WithSkipMigrations()` (one site). Fix: inline skip added,
`WithPoolSize(1)` preserved verbatim; no assertion changed. Before: screening
warm re-measure on the epoch tree (not the `da7abd7f` base above). After:
measured on a box shared with concurrent validation windows (provisional —
final warm pair lands after the template cache).

| test | class | exact command | before wall/CPU | after wall/CPU | L |
|---|---|---|---|---|---|
| `TestPublicationFullCaptureEligibilityAndBundle` (store) | T3 | `go test -race -count=1 -timeout=0 -run '^TestPublicationFullCaptureEligibilityAndBundle$' ./internal/store` (one discarded warmup at 33.8 s, serial, GNU `time -v`) | 32.4 wall / 29.08 user / 1.25 sys | **warm 4.96 / 3.07 / 0.89** (L1 3.85; cold record: 34.5 wall / 31.00 user / 1.13 sys — cache state changed (cold -> warm)) | 0.983 → 0.955 |

Wall-neutral within load noise: the test already opened golden copies, so the
removed per-subtest cost was the migration-state check only (no replay). The
`-cpuprofile` mechanism check confirms it — the remaining `store.Open` cost
(22.3 s cum, 72.8% of samples) sits entirely under
`storetest.ensureGolden → sqlitemigration.Migrate` (21.2 s cum), i.e. the
once-per-process template build that the template cache removes, not this
test's opens (profile run, not quoted as a wall). The
conversion's durable value is enforcement-rule cleanliness (skip-bearing open,
no suppression). T4 payload share: measured-inapplicable, not applied — the
read-only mutation audit found no in-place entry writes (writer path reads
`EntryIndex` only; the backfill already copies before mutating; sequential
subtests; per-subtest fresh DBs), but the expected win is string-build only
while per-subtest DB indexing dominates. The warm cell reconciles with the
template-cache section below (row 4, B-storepub: `ensureGolden` 0.01 s cum; row 5
is a concurrent cold-cache run, not a focused warm pair) — one warm number, not a
duplicate row.

Warm re-measure (cache state changed (cold -> warm)): the bold warm cells above were
re-measured on integration head `8e623e39` (template cache and store-open seam
conversions merged), same command shape, one discarded warmup per test, serial,
nproc 32; `L1` is the 1-minute `/proc/loadavg` before the measured run. The
prior value in each cell is kept as the cold record.

## Screening additions: partition and fix-set pointers

The remaining four confirmed >30 s screening findings. Walls in this section
were taken on the epoch tree (`09d9e93c`), not the `da7abd7f` survey base; the
row shape above still holds (test → class → exact command → before wall/CPU →
after wall/CPU → L). After-columns point to their owning section; the store
publication test's conversion pair is recorded in the section
above and is not repeated, and its post-cache warm pair is the 4.96 s cell
there.

`TestLargeRecordsAreHandledUniformlyAcrossHarnesses` (ingest) is T1,
eligibility STRONG: the recorded read shows synchronous in-process byte work
(filter + `IndexTranscriptBytes` + adapter extraction, no store open, no SQL,
no goroutines/channels/atomics on the exercised path; the `t.Parallel` is
suite scheduling), and the fixture sizes are the invariant (10 MiB whole-record
cases across five harnesses; the 256 MiB production limit), not reduced. The
committed rollup over the `-top -nodecount=300` profile puts the detector at
81.47% flat with zero store/SQL/engine rows — past the 60% bar. The pair
(preceded by screen 52.9 s, warm 48.6 s) is 8.9x; race CPU ≈ wall, the
detector serializing single-threaded bytes. The five-part soundness argument
and the registry cost travel with the partition entry, whose change is the
registry's single writer.

| test | class | exact command | before wall/CPU | after wall/CPU | L |
|---|---|---|---|---|---|
| `TestLargeRecordsAreHandledUniformlyAcrossHarnesses` (ingest) | T1 | `go test -race -count=1 -timeout=0 -run '^TestLargeRecordsAreHandledUniformlyAcrossHarnesses$' ./internal/ingest` (before with `-race`; after without; one discarded warmup each, serial, GNU `time -v`, epoch tree, GOMAXPROCS=32) | 46.56 wall / 43.14 user / 3.75 sys | 5.22 wall / 5.59 user / 0.83 sys | 0.961 → 0.967 |
| `TestOpenCodePrivateExecutionGuardCoversFixtureOwnedBuildTopology` (ingest) | partition, toolchain/static-analysis character | `go test -race -count=1 -timeout=0 -run '^TestOpenCodePrivateExecutionGuardCoversFixtureOwnedBuildTopology$' ./internal/ingest` (warm serial re-measure, GNU `time -v`, epoch tree) | screen 45.0 / warm 42.72 wall (73.44 user / 30.52 sys) | 31.4 wall / 94.5 cpu (partition registry cost pair) | not recorded (screening re-measure; the partition entry carries the pair) |
| `TestOpenCodePrivateExecutionGuardRejectsFixtureOwnedBuildTaggedBypasses` (ingest) | partition, toolchain/static-analysis character | `go test -race -count=1 -timeout=0 -run '^TestOpenCodePrivateExecutionGuardRejectsFixtureOwnedBuildTaggedBypasses$' ./internal/ingest` (warm serial re-measure, GNU `time -v`, epoch tree) | screen 33.0 / warm 31.94 wall (54.17 user / 22.57 sys) | 23.4 wall / 69.0 cpu (partition registry cost pair) | not recorded (screening re-measure; the partition entry carries the pair) |
| `TestHelperGroupListingThroughRegisteredRoutes` (api) | T6 | `go test -race -count=1 -timeout=0 -run '^TestHelperGroupListingThroughRegisteredRoutes$' ./internal/api` (warm serial re-measure, GNU `time -v`, epoch tree) | screen 63.1 / warm 52.35 wall (47.87 user / 1.47 sys) | 8.01 wall / 14.94 user / 1.18 sys (post-cache focused pair) | load 2.1→2.6 (post-cache run; the T6 section carries it) |
## T2 — SQL statement and seed reductions

Change: `SyncModels` prepares its 15-column upsert once and rebinds it per
model; sixteen constant-text sites move from per-call prepare to the
connection-cached statement; the backfill seed test commits one metadata
batch and one entry write set instead of one transaction per session. No
assertion changed. Focused command per row (one discarded warmup, serial):
`go test -race -count=1 -timeout=0 -run '^<Test>$' ./<pkg>`, timed with GNU
`time -v`; the before column is the survey baseline above (base `da7abd7f`,
L=0.957). The after column ran on a box shared with a concurrent validation
window — provisional walls may be inflated, which only understates wins. L
from the documented companion (`RACE=0 go run ./cmd/testgate run -pkgs
./internal/testkit/coveragemap,./cmd/testgate -race=false`): 0.952 at window
start, 0.951 at window end. GOMAXPROCS=32. Before/after columns read
wall / user / sys, seconds.

| test | class | exact command | before wall/CPU | after wall/CPU | L |
|---|---|---|---|---|---|
| `TestModelsSync_500_StaticFallback` (cmd) | T2 | `go test -race -count=1 -timeout=0 -run '^TestModelsSync_500_StaticFallback$' ./cmd/peasant` | 10.2 / 8.9 / 1.1 | 6.70 / 5.59 / 0.89 | 0.952 |
| `TestRetainedContentBackfill` (ingest) | T2 (seed batching) | `go test -race -count=1 -timeout=0 -run '^TestRetainedContentBackfill$' ./internal/ingest` | 28.3 / 26.0 / 1.2 | 25.78 / 24.47 / 1.03 | 0.952 |
| `TestResolveStoredOriginsWritesAVerdictIntoEveryRow` (ingest) | T2 (origin conversions; migration-dominated) | `go test -race -count=1 -timeout=0 -run '^TestResolveStoredOriginsWritesAVerdictIntoEveryRow$' ./internal/ingest` | 41.1 / 38.0 / 1.3 | 41.51 / 38.56 / 1.19 | 0.952 |

Behavior proof: `go test -race -count=1 ./internal/store/` PASS — wall
397.68 s, user 543.42 s, sys 11.37 s. That suite covers the converted call
sites (models sync/get, origin list/update, session insert, evidence
load/save, index-state read, seq-cursor lookup/upsert).
`go test -race -count=1 ./internal/salt/` PASS — wall 1.60 s, user 0.71 s,
sys 0.27 s (covers the four `salt.Load` conversions).

Disposition, no code change (each verified by reading the call site):
- `BulkLookupSessionLocations` (`reader.go`) builds `IN (?,?,…)` with arity
  from the input length — variable text, excluded by the constant-text
  guard, no fallback.
- `selectSessionIDs` (`harvester_selection.go`) executes caller-supplied
  text — excluded, no fallback.
- `ListStaleIndexSessions` (`index_state_writer.go`) builds a `WHERE …
  OR …` chain from the targets map — excluded, no fallback.
- `validateIndexFormatQueryOnConn` (`index_format_reader.go`) builds a
  per-batch `IN` list sized to the final batch — excluded, no fallback.
- `preparePragmas` (`store.go`) runs once per connection via
  `PoolOptions.PrepareConn` — caching buys nothing; dropped.
- `applyV23DataMigration` runs only on the migration path — T3-covered.

Reading of the rows: the models-sync loop sheds ~34% of its focused wall;
the backfill seed sheds ~9%; the origin verdict path is flat (+1%,
shared-box noise on a migration-dominated test) — the origin conversions
are kept as zero-risk parse savings and recorded here rather than forced
into a win.
## Store-open seam defaults (test-surface migration-open conversion)

Per-package race suites after converting 53 `store.Open` sites in 47 files to
the golden-plus-skip seam (`storetest.Open/OpenWith`, `CopyGoldenDB`/`CopyGoldenTo`
plus `store.WithSkipMigrations`), with 10 per-call suppressions carrying class
reasons (3× fresh-open creation/idempotence, 6× open-time format-registration,
1× benchmark setup outside the measured section). Command per row:
`go test -race -count=1 ./<pkg>`, serial, one package at a time. `<time>` is
GNU time 1.10 with `-v` at `/run/current-system/sw/bin/time` (`/usr/bin/time`
is absent on this box). L from the documented companion
(`RACE=0 go run ./cmd/testgate run -pkgs
./internal/testkit/coveragemap,./cmd/testgate -race=false`): 0.922 at window
start, 0.949 at window end.

| package | result | wall (s) | user (s) | sys (s) | conversions |
|---|---|---|---|---|---|
| `internal/store` | PASS | 407.68 | 548.37 | 12.62 | 20 converted, 10 suppressed |
| `internal/api` | PASS | 225.89 | 415.69 | 11.36 | 11 converted |
| `internal/metrics` | PASS | 5.69 | 9.24 | 0.82 | 8 converted |
| `internal/push` | PASS | 56.14 | 90.50 | 5.47 | 6 converted |
| `internal/transcript` + `internal/export` | PASS | 56.70 combined (transcript 54.631, export 9.838 per go) | 97.91 | 6.01 | 3 converted |
| `internal/e2e` (untagged unit tests) | PASS | 5.47 | 9.10 | 2.23 | 0 (edited files are `e2e`-tagged) |

E2E disposition: the 5 edited e2e files carry `//go:build e2e` and need the
full harness (podman Postgres + RustFS + a village checkout providing
`./cmd/server` + `./cmd/village-setup-demo`, wired via `VILLAGE_BIN` /
`VILLAGE_REPO`); that infra is not provisioned in this window, so tagged-e2e
execution is N/A — not a skip of runnable work. The edited files are proven
instead by `go vet -tags e2e ./internal/e2e/` (clean) plus the untagged
package suite above (PASS).

Scan delta (scratch config of the validated rule, same tree): 209 flagged
sites at the branch point → 147 after this change (−62 = 29 store + 33
api/metrics/push/e2e/transcript/export); the remainder belongs to
`cmd/peasant`, `internal/ingest`, the migration suite, and the already
skip-bearing sites owned by other leaves. `ast-grep scan --config sgconfig.yml
.` exits 0 (the enforcement rule file itself lands with the store-open seam enforcement change).

## T3 focus restoration: storetest template cache

The `storetest` template was built once per process (61 migrations under
`ensureGolden`), so a focused run paid a full migration pass on top of the
test's own work. The cache (`internal/store/storetest/golden.go` rewritten;
new `internal/filelock` leaf; `store.SchemaFingerprint()` stamp;
tmpfs-preferred managed copy root; read-once template buffer) makes the
template reusable across processes. A tree is the epoch integration branch at
`764aa87f` (with the seam-default conversions), B tree is the template-cache worktree at
`7aa9a7f0`; the three family test files are byte-identical between the trees,
and the only other A/B delta is `storetest` itself, so the pairs are
attributable to the cache. Serial, quiet box, one discarded build-cache
warmup per tree per test, GOMAXPROCS=32 (nproc), GNU `time -v`. L from the
documented companion
(`RACE=0 go run ./cmd/testgate run -pkgs
./internal/testkit/coveragemap,./cmd/testgate -race=false`): 0.951 at window
start (A), 0.982 at window end (B), 0.952 bracketing the final-code B re-runs
— all far below the `L > 4` discard bar.

Design evidence rows:

- Row 1 (carried): #3 base-tree focused — 45.27 s wall / 41.46 s user (from
  the T3 diagnostic, labelled).
- Row 2 (carried + re-measured): converted tree, cache cold — 58.37 s
  carried from the diagnostic (the regression this change fixes); re-measured
  47.03 s mean on the current integration head (A1 46.13 / A2 47.92 below).
  The 11 s gap is tree drift between the diagnostic and the seam-converted
  head, recorded here rather than hidden.
- Row 3 (the fix's proof): converted tree, cache warm — #3 B-final mean
  15.84 s (B1 14.59 / B2 17.09), i.e. roughly the base wall minus the test's
  own migration pass plus the template build (row 4 decomposes it).
- Row 4 (mechanism): warm-run `-cpuprofile` (`-top -nodecount=300`) shows
  `sqlitemigration.Migrate` absent on B in all three contexts (B-ingest3,
  B-storepub with `ensureGolden` at 0.01 s cum, B-cmdpub with `store.Open` at
  0.18 s cum); cold A runs keep it at 27.85 s / 25.31 s / 3.78 s cum
  (ingest/store/cmd). No residual migrating open on the warm path.
- Row 5 (concurrency): two focused store-publication processes started
  together on a cold cache — both pass (4.73 s / 4.77 s), exactly one stamped
  file, no corruption, no leftover build dir.
- Row 6 (copy path, no bar): 100 warm production copies of the 800 KiB
  template take 44.8 ms to the tmpfs managed root (0.45 ms/copy) vs 36.6 ms
  to `t.TempDir()` (0.37 ms/copy), `-race`, same process; the cold-process
  first copy (adoption + first-use sweep + buffer fill + one copy) takes
  5.4 ms. The destination effect is in the noise next to the ~25–30 s
  migration saving, and tmpfs shows no speed advantage on this box — the
  keep/drop decision rests on hygiene (dead-owner sweep, no `/tmp` pile), not
  speed. The first-use sweep cost is reported, not amortized away.
- Row 7 (hygiene): `storetest-golden-*` count under `/tmp` is 195 before and
  195 after every template-cache run — no new `TMPDIR` litter (managed roots only; the
  195 are other trees' per-process builds, still unconverted).
- Row 7b (killed-run proof): SIGKILL of a race test process ~1 s into a cold
  template build (`signal: killed`, orphaned `build-*` dir + reused lock
  file as the only trace) — the next run passes (4.81 s), the orphaned
  build dir is reclaimed under the exclusive build lock, exactly one stamped
  file exists, no `-shm`/`-wal` beside it, `/tmp` count unchanged. Honest
  limit: the `t.TempDir()` fallback path still relies on `t.Cleanup` under
  SIGKILL; the managed root is the improvement where available. The
  dead-owner age rule itself (dead AND ≥10 min) is proven by unit test with
  backdated shelves, not by waiting.

Family A/B pairs (wall / user / sys, seconds; exact command per row:
`go test -race -count=1 -timeout=0 -run '^<Test>$' ./<pkg>`):

| test | A1 | A2 | B1-final (warm) | B2-final (warm) | L |
|---|---|---|---|---|---|
| `TestNormalIngestStoresAuthoritativeContent` (ingest) | 46.13 / 41.52 / 1.78 | 47.92 / 41.95 / 1.80 | 14.59 / 13.00 / 1.40 | 17.09 / 14.91 / 1.51 | 0.951–0.982 |
| `TestPublicationFullCaptureEligibilityAndBundle` (store) | 35.14 / 31.26 / 1.36 | 33.65 / 30.29 / 1.32 | 5.49 / 3.68 / 1.08 | 5.20 / 3.64 / 1.07 | 0.951–0.982 |
| `TestPiDatabasePublicationThroughCLI` (cmd) | 38.97 / 36.12 / 1.58 | 37.11 / 33.80 / 1.58 | 37.15 / 32.23 / 1.52 | 36.21 / 32.93 / 1.62 | 0.951–0.982 |

Reading: the store publication test sheds ~29.0 s (−84%, A-mean 34.40 to
B-mean 5.35 — better than the ~12 s estimate); #3 sheds ~31.2 s (−66%,
47.03 to 15.84); the cmd publication test sheds ~1.4 s (−4%, 38.04 to
36.68), consistent with its small template build (row 4: 3.78 s Migrate).
B-side repeats are flat — no per-test sweep inflation (sweeps run only on a
successful build and once per process at first use; the copy path takes no
cross-process lock). One pre-fix B run (`B2-warm` store) failed to BUILD
(`[build failed]`) because a `go test` invocation compiled the tree between
two of the worker's own edits; it was re-run clean on the final code and is
recorded here, not hidden. Pre-fix warm Bs (14.22/16.21, 4.30, 33.12/35.17)
agree with the final-code Bs within load noise.

On the ~6x template-build magnitude (same schema, different contexts): the
A-side profiles decompose `store.Open` into Migrate-internal plus eager pool
open — ingest (pool 10) carries ~8.9 s outside Migrate, store (pool 2)
~1.4 s, cmd (pool 1) ~0.2 s, matching pool sizes; Migrate-internal itself
measures 27.85 s / 25.31 s / 3.78 s for the identical 61-migration list
(pure-Go SQLite under `-race`/checkptr, binary-context-sensitive; root cause
not isolated). Moot for the epoch either way: the cache removes the build in
all three contexts, and every warm profile shows Migrate absent.

Validation-found deviations from the design letter (mechanism unchanged):
validation runs against a scratch copy inside the cache dir via the
sanctioned `store.SchemaVersionAt` (never the shared file) because a probe
through this repo's own driver showed a read-only open of a WAL template
creates `-shm`/`-wal` beside the file while `immutable=1` creates none; the
scratch removes its own sidecars, and the under-lock reclaim plus the
`build-*` age sweep cover a SIGKILLed validator. The managed root is
UID-suffixed and scheme-versioned (`peasant-storetest-v1-u<uid>`); sharing
one override root across PID namespaces is unsupported and documented.
Standing template: `golden-v1-61-3f6c0858cc34.db`, 819200 bytes (800 KiB).

Not run: the retention test as a fourth pair — a 230 s-class test needs ~6
runs (~25 min) for one more instance of the identical template mechanism the
three pairs already span across three packages; stated, not silently
dropped.

## T3 focus restoration, copy-root decision: tmpfs default dropped

Follow-up decision on the section above: the tmpfs-preferred managed copy
root is dropped as the default. The isolated micro (row 6: 100 warm
production copies of the 800 KiB template, `-race`, same process) measured
44.8 ms to the tmpfs root vs 36.6 ms to `t.TempDir()` — noise next to the
~25–30 s migration saving, with no speed edge for tmpfs on this box. The
default copy root is therefore `t.TempDir()` again (status-quo semantics,
Go-owned cleanup, OS tmpfiles under SIGKILL); `PEASANT_STORETEST_TMPDIR`
stays as the opt-in override, routing copies through the managed root
(per-user scheme subdirectory, first-use dead-owner sweep under the pinned
conservative rules) when set and writable. The tmpfs auto-probe and its test
seam are removed; the template cache itself (stamp, lock, sweeps, space
guard, read-once buffer) is unchanged, as are the `!unix` age-only sweep and
the override validation. Copy hygiene without tmpfs rests on the template
cache's own sweeps plus OS tmpfiles for the per-test copies; the `/tmp`
litter row above (195 → 195 across every template-cache run) already measured the
default path. The family A/B walls stand as quoted: they measured the cache
effect, and the copy destination contributed ~0.4 ms per copy either way.

## Store-open seam conversions — internal/ingest

- Scan (`ast-grep scan -r <no-migrating-store-open-in-tests rule> internal/ingest`): **36 flagged
  sites in 28 files before → 0 after**. No suppressions were needed: no remaining `internal/ingest`
  open is a migration, fresh-open creation, idempotence, close, or open-time-refusal subject.
- Converted (all to `storetest.CopyGoldenDB(t)` + `store.WithSkipMigrations()`, existing options
  such as `WithPoolSize(1)`, `WithIndexFormats`, `WithGenerationArtifacts` kept verbatim):
  captured_source, claude_evidence_cache (`openEvidenceStore`), control_record_ingest (close/reopen
  on the same path), deferred_pair_repair, harvester_pipeline, index_format_output,
  index_input_pipeline, metadata_child_compatibility, metadata_read_policy, metrics_refresh,
  native_refresh_repair (`nativeRepairStore`; its callers in codex_unknown_native and
  opencode_unknown now pass a golden copy), opencode_acquired_cursor, opencode_selection
  (close/reopen), opencode_unknown_legacy, orphan_pipeline (close/reopen), pair_repair,
  pair_snapshot, permanent_refusal_steady_state, pipeline_hostslug_redaction,
  pipeline_opencode_synthetic_reindex, publication_capture_child, rebuild, refusal_churn,
  retained_unknown, write_path_columns.
- Already-skipping spread options inlined so the rule sees the skip: native_unknown_public,
  unknown_local_budget.
- opencode_native_cli: the reader of the database the harvest binary created now opens with
  `WithSkipMigrations` (the production binary still migrates on create).
- Audit: the golden is schema + salt only, with no seed rows, so "no rows" assertions still hold.
  No converted test asserts on a per-database salt or on DB file timestamps.
- Smokes (goleak `VerifyTestMain` active):
  - `go test -count=1 -run '^(<46 tests in the converted files>)$' ./internal/ingest/` → ok, 38.6 s (42 s wall)
  - `go test -race -count=1 -run '^(TestClaudeAdapter_EvidenceCacheSkipsUnchangedTranscripts|TestControlRecordIngestExportAndPublication|TestCanonicalOpenCodeSelection*|TestOpenCodeContentAwareCanonicalSelection)$' ./internal/ingest/` → ok, 11.5 s (27 s wall)
## Store-open seam conversions — cmd/peasant

Scan: `ast-grep scan -r <no-migrating-store-open-in-tests rule> cmd/peasant` (rule
not committed here). **Before: 77 flagged sites in 40 files. After: 0** (no suppressions).
The dry-run seeding family (`seedClosedStore*`) was already routed through
`storetest.CopyGoldenTo` and is untouched.

Mechanism: one test helper, `openPreparedStore(t, path, opts...)`
(`cmd/peasant/prepared_store_test.go`). A missing path is prepared as a golden copy
(MkdirAll + `CopyGoldenTo`) so commands later find zero pending migrations; an existing
path (created by a command's production open, or by an earlier seed) is reopened as is.
Both open with `store.WithSkipMigrations()` plus the caller's options verbatim;
`EnvPoolSize=1` from TestMain stays in force.

Converted files: cmd_annotate_import, cmd_annotate_prune, cmd_annotate,
cmd_kickstart_conversion_mount, cmd_kickstart_mount, cmd_kickstart_rescan,
cmd_kickstart_selection_command, cmd_prune, cmd_push_disclosures, cmd_push_hook_advice,
cmd_push_hook_ergonomics, cmd_push_repository, cmd_push_scope_reuse, cmd_push,
cmd_redact, cmd_sessions_context, cmd_sessions_list, cmd_sessions, cmd_village_stub,
cmd_village_transcripts, harvest_index_selection, harvest_metadata_diagnostics,
ingested_publication, interrupted_pair_install, kickstart_evidence_cache,
kickstart_metadata_diagnostics, mounted_selection_safety, opencode_canonical_persistence,
opencode_cwd_wiring, opencode_legacy_sqlite_mount, opencode_session_clock_mount,
pi_kickstart_journey, prune_exact, publication_readiness, push_door_redaction,
readonly_run, redaction_policy, source_harness_flag, strike_ingestion,
zz_two_run_kickstart_demo (all `_test.go`).

Suppressions: none. No site in this package has migration, creation, or open-time
refusal as its subject.

Intentional fresh-install coverage (a command runs against an empty root and performs the
production migrating open; the test's own open happens afterwards on the existing file,
so these stay seed-free):
- strike_ingestion_test.go: TestStrikeIngestCommandPersistsSessionDetail,
  TestStrikeIngestCommandAddsChildAfterParentSourceDisappears,
  TestStrikeIngestKeepsARecordOverTheRetiredPerLineLimit
- source_harness_flag_test.go: TestSourceHarnessFlagMounted
- pi_kickstart_journey_test.go: TestPiKickstartMountedDiscoveryThroughStoredImport
- kickstart_metadata_diagnostics_test.go: TestKickstartLocalIngestForwardsMetadataDiagnostics
- opencode_canonical_persistence_test.go: TestCanonicalOpenCodeRealStoreDetailAndAnalytics
- opencode_cwd_wiring_test.go: TestOpenCodeCurrentSQLiteEntersMountedProductionThroughManagedProjection
- opencode_legacy_sqlite_mount_test.go: TestLegacyOpenCodeSQLiteMountedHarvestCreatesManagedIndexedAnalyticsState
- opencode_session_clock_mount_test.go: TestOpenCodeSessionClockFixturesMountedHarvest
- ingested_publication_test.go: TestIngestedPublicationThroughCLIAndRegisteredShare
- redaction_policy_test.go: callers of readRecordedSlug (harvest first, then inspect)

No-database cases (`os.Stat` NotExist in TestPiHarvestCommonModes) open nothing and are
unchanged.

Smoke (every Test func in the 40 converted files, 264 names, non-race, same box):
`go test -count=1 -run "^(<264 names>)$" ./cmd/peasant/...` → ok, 17.98 s package /
19 s wall after; 20.49 s / 24 s wall on the unconverted base (single runs, noisy box).

## Store-open seam enforcement (ast-grep ban on migrating test opens)

Rule: `ast-grep/no-migrating-store-open-in-tests.yml`, auto-loaded through
`sgconfig.yml` `ruleDirs`, so the `ast-grep scan --config sgconfig.yml .` step of
`make check` (and therefore CI) enforces it. It flags any `store.Open(...)` call in a
`*_test.go` file whose arguments do not contain `store.WithSkipMigrations()`, and
ignores `internal/store/migrations_test.go`, `internal/store/migration*_test.go`
(the migration suite) and `internal/store/storetest/**` (the seam implementation).

**Binary version matters.** The pinned devShell binary is ast-grep 0.45.0, which
honours `// ast-grep-ignore: <rule-id> -- <reason>` (trailing reason text). The
host's `/run/current-system/sw/bin/ast-grep` 0.42.1 does not: it treats the
reason as part of the rule id, reports every suppression as unused, and fails
the scan with one error per suppression. Run the scan inside the devShell
(`nix develop`), as `make check` and CI do.

### Scan counts

| point | flagged sites | files |
|---|---|---|
| branch point (validated rule, scratch config) | 209 | 134 |
| after all conversions (committed rule) | 0 | 0 |

Before, by package: `cmd/peasant` 92, `internal/ingest` 55, `internal/store` 29,
`internal/api` 11, `internal/metrics` 8, `internal/push` 6, `internal/e2e` 5,
`internal/transcript` 2, `internal/export` 1.

Final scan (ast-grep 0.45.0, worktree root):

```
$ ast-grep scan --config sgconfig.yml .
$ echo $?
0
```

No output, no findings, no unused-suppression hints. The scoped
`ast-grep scan --config sgconfig.yml cmd internal` gives the same result.

### Rule checks (scratch tree under /tmp/opencode, not committed)

| case | result |
|---|---|
| `store.Open(p)` in `b_test.go` | fires |
| `store.Open(p, store.WithSkipMigrations())` | silent |
| `store.Open(p, store.WithPoolSize(1), store.WithSkipMigrations())` | silent |
| same skip-less open in `prod.go` (not a test file) | silent |
| skip-less open in `internal/store/migrations_test.go`, `internal/store/migration_v5_test.go`, `internal/store/storetest/x_test.go` | silent (ignored) |
| skip-less open in `internal/store/other_test.go` | fires |
| suppression line above one call, a second unsuppressed call directly after it | first silent, second fires |
| suppression forms: preceding line with `-- reason`, bare, reason on its own earlier comment line, trailing same-line | all silent on 0.45.0 |

### Exception manifest (per-call suppressions)

Each suppressed open is one where the migrating open is itself what the test
checks. A golden copy with the skip would bypass the code under test.

| file:line | class | exception |
|---|---|---|
| `internal/store/store_test.go:94` | B | a fresh open creates the database and schema from nothing |
| `internal/store/store_test.go:140` | B | first open of the idempotent-reopen pair creates the database |
| `internal/store/store_test.go:161` | B | second open must replay the migrating path on an existing file |
| `internal/store/mixed_index_formats_test.go:99` | C | custom formats and conversion edges registered at open (persist/convert/rollback) |
| `internal/store/mixed_index_formats_test.go:514` | C | pipeline upgrade case registers its declaring harness format at open |
| `internal/store/index_input_transactions_test.go:81` | C | conditional conversion transactions register a fault-scoped handler and edge at open |
| `internal/store/publication_projection_test.go:66` | benchmark setup | `storetest` takes `*testing.T`, so it cannot serve a `*testing.B`; the one open runs outside the timed section |

Class A (migration suite) is exempt by the rule's ignores, not by suppressions.
Class E (first-run CLI cases) never shows up in the scan, because the command does
the open, not the test. That list is in the `cmd/peasant` conversion section
above.

The `storetest` package doc (`internal/store/storetest/golden.go`) says that the
package is the only sanctioned way for tests to open a store, and it names this
rule.

### Focused smoke (one converted test per package, `-race -count=1`)

| package | test | go-reported time |
|---|---|---|
| `internal/store` | `TestArtifactMirrorStopsOnOuterTransactionLoss` | ok 3.06 s |
| `internal/ingest` | `TestPipelineRetainedAdapterMaintenance` | ok 4.35 s (goleak `VerifyTestMain` clean) |
| `cmd/peasant` | `TestCLI_AnnotateImportRoundTrip` | ok 1.47 s |
| `internal/api` | `TestActiveSnapshotSharePublicationConverges` | ok 3.07 s |
| `internal/metrics` | `TestMetricsRecomputesChangedInputAndReusesEqualProof` | ok 1.18 s |
| `internal/push` | `TestDatabasePublicationWithoutSourcesOrSidecars` | ok 5.66 s |
| `internal/e2e` (`-tags=e2e`) | `TestGitHooksSubstrate_SeedsRepositoryIdentity` | PASS 2.27 s, ok 3.33 s, 9.2 s wall |
| `internal/transcript` | `TestPiProjectionSQLiteOutbound` | ok 13.28 s |
| `internal/export` | `TestGenerationDetailBarrier` | ok 2.67 s |

Command shape: `go test -count=1 -race -run '^<Test>$' ./<pkg>`, in the devShell.

### Taxonomy note

"Store-open seam" is the name used in this document for test-only work. That
work routes test database opens through the pre-migrated golden template
(`storetest`) and bans the migrating open with the ast-grep rule. Exception classes:

- A: migration suite
- B: fresh-open creation and idempotence
- C: open-time format registration or refusal
- D: no-database cases
- E: first-run CLI coverage
- F: e2e fixture builders

Production opens (CLI, server, prune) are unchanged and still migrate.
## T6 — packing proof: helper-group listing route cases

Admissibility: `TestHelperGroupListingThroughRegisteredRoutes`
(`internal/api/helper_group_listing_test.go`) is CPU-bound and its 26 fixture
cases are independent — each opens its own golden-copy store
(`storetest.Open`), starts its own port-0 server, and seeds only that store.
The test is not named in `internal/testutil/testdata/parallel_unsafe_sites.yaml`,
and neither the test nor `internal/api` production code touches a
process-global sink (`slog.SetDefault`, `os.Stdin`, `os.Stderr`, `Setenv`,
`Chdir`). Change: `t.Parallel()` on each case subtest; the top-level test stays
serial; `-parallel` is not pinned.

Effective settings: `-p` default, `-parallel` default (= GOMAXPROCS),
GOMAXPROCS=32 (32 CPUs). Commands, serial, one discarded warmup, GNU
`/run/current-system/sw/bin/time -v`; load is the 1-minute load average
(`uptime`) around each measured run. Before = branch point (golden
template cache present); after = tip with the change.

| test / package | class | exact command | before wall / user / sys (s) | after wall / user / sys (s) | load |
|---|---|---|---|---|---|
| `TestHelperGroupListingThroughRegisteredRoutes` (api) | T6 | `go test -race -count=1 -timeout=0 -run '^TestHelperGroupListingThroughRegisteredRoutes$' ./internal/api` | 14.37 / 11.59 / 1.06 | 8.01 / 14.94 / 1.18 | 7.5→7.1 before; 2.1→2.6 after |
| `internal/api` package | T6 | `go test -race -count=1 -timeout=0 ./internal/api` | 102.19 / 293.83 / 9.30 | 96.34 / 296.87 / 9.16 | 9.1→8.2 before; 2.6→5.5 after |

The earlier 52.4 s warm focused record predates the golden template cache;
the cache alone took the focused wall to 14.4 s, and packing takes it to
8.0 s (below the 30 s bar either way). The package gain is small because the
package already runs other tests in parallel around this one; the before
package run saw higher load, so its delta is an upper bound. The
parallel-unsafe guard (`go test ./internal/testutil`) stays green.

## T1 — no-race partition: single-threaded byte tests

Change: the nine tests below move to the gate's no-race pass as `single-threaded-bytes`
registry entries; each entry carries the short-form argument (subject; concurrency actually
exercised per a bounded code read; why the race detector is not this test's oracle; retained
race coverage; residual risk) in `no-race-partition.yaml`, repeated in the taxonomy's
detector-tax section. The two build-topology guards and the large-record harness test joined
the same partition pass earlier. No test code changed.

Measurement: focused Class A, one discarded warmup per pass, serial; `wall / user / sys` in
seconds from GNU `time -v`; the before column ran with `-race`, the after column is the
command shown per row; L is the 1-minute load average before each measured run (32 cores).
The box ran other work concurrently; every pair improved anyway.

| test | after command | before wall/user/sys | after wall/user/sys | L |
|---|---|---|---|---|
| `TestResolveStoredOriginsWritesAVerdictIntoEveryRow` | `go test -count=1 -timeout=0 -run '^TestResolveStoredOriginsWritesAVerdictIntoEveryRow$' ./internal/ingest` | 9.25 / 7.11 / 0.90 | 2.75 / 1.47 / 0.62 | 3.15 -> 3.43 |
| `TestUnknownPrivateEncoding` | `go test -count=1 -timeout=0 -run '^TestUnknownPrivateEncoding$' ./internal/ingest` | 5.87 / 4.36 / 0.83 | 1.59 / 1.39 / 0.54 | 3.48 -> 4.63 |
| `TestNativeCoverageMatrix` | `go test -count=1 -timeout=0 -run '^TestNativeCoverageMatrix$' ./internal/ingest` | 9.32 / 4.95 / 0.97 | 3.87 / 1.37 / 0.58 | 5.46 -> 6.50 |
| `TestMountedKickstartStoredGateAlignsViewerAndPush` | `go test -count=1 -timeout=0 -run '^TestMountedKickstartStoredGateAlignsViewerAndPush$' ./cmd/peasant` | 4.45 / 2.49 / 0.90 | 2.04 / 1.35 / 0.67 | 6.62 -> 7.69 |
| `TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection` | `go test -count=1 -timeout=0 -run '^TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection$' ./cmd/peasant` | 3.60 / 2.32 / 0.80 | 1.66 / 1.41 / 0.65 | 8.12 -> 8.19 |
| `TestMountedLegacySelectedConversion_ConsentCancellationAndRerun` | `go test -count=1 -timeout=0 -run '^TestMountedLegacySelectedConversion_ConsentCancellationAndRerun$' ./cmd/peasant` | 4.87 / 2.34 / 0.99 | 2.45 / 1.30 / 0.75 | 10.57 -> 13.53 |
| `TestPublicationWizardAndReportUseDatabaseReadiness` | `go test -count=1 -timeout=0 -run '^TestPublicationWizardAndReportUseDatabaseReadiness$' ./cmd/peasant` | 3.37 / 2.00 / 0.78 | 1.53 / 1.24 / 0.64 | 17.33 -> 16.43 |
| `TestModelsSync_500_StaticFallback` | `go test -count=1 -timeout=0 -run '^TestModelsSync_500_StaticFallback$' ./cmd/peasant` | 6.52 / 5.40 / 0.89 | 1.34 / 1.33 / 0.66 | 14.66 -> 13.96 |
| `TestKickstartRescan_FallsBackWithoutCompatibleDatabase` | `go test -count=1 -timeout=0 -run '^TestKickstartRescan_FallsBackWithoutCompatibleDatabase$' ./cmd/peasant` | 2.77 / 1.61 / 0.78 | 1.46 / 1.27 / 0.57 | 13.16 -> 12.35 |

Canonical subset proof: `RACE=1 go run ./cmd/testgate run -pkgs ./internal/ingest,./cmd/peasant`
ran the moved entries of these packages in the no-race pass (6 `internal/ingest`, 7
`cmd/peasant`), with none missing or duplicated. Screen and result:

    all four rules passed: every test ran exactly once across the passes
    testgate: PASS

race pass 12m18.461s, no-race pass 1m35.015s, calibration L=0.952.

Cost drift: focused re-measurements of the registry's other entries (same command shape, one
discarded warmup) refreshed the pairs below where the wall moved by more than 10 % of the
recorded value; sub-second entries were measured three times and refreshed from the median
(their focused walls include process startup):

| entry | mode | old wall/cpu (ms) | new wall/cpu (ms) | wall delta |
|---|---|---|---|---|
| `TestRedactionModuleBoundary` | no-race | 634 / 1014 | 860 / 1290 | +35.6% |
| `TestRedactionBoundaryFixtureStrictDecoding` | no-race | 583 / 934 | 810 / 1240 | +38.9% |
| `TestRedactionScannerHoldsALineAtTheRecordLimit` | no-race | 895 / 1240 | 1110 / 1500 | +24.0% |
| `TestRedactionEngineHandlesRecordOverTheOldLimit` | no-race | 25700 / 26013 | 22870 / 23290 | -11.0% |
| `TestBuildSnapshotDetailBytesSerializesInsideSnapshotCallback` | no-race | 773 / 1331 | 900 / 1370 | +16.4% |
| `TestOpenCodeNativeCLI` | race (protected) | 39216 / 53162 | 17210 / 16300 | -56.1% |

Unchanged within 10 %: the e2e seed-unset build, the capabilities matrix, both
build-topology guards, and the large-record test. `TestOpenCodeNativeCLI` stays race-covered
(protected); its pair was refreshed from its focused `-race` run.

## Epoch close — final gate, class aggregates, and collateral screen

This section closes the record. It adds only sums over the rows above, the final
gate's own numbers, and the collateral screen; every per-row number keeps its
exact command and SHA in its own section.

### Final gate and its base pair

Final gate, head `fc7d9c95`, 2026-09-29: `make check RACE=1` exited 0, the
four-rule screen printed `all four rules passed: every test ran exactly once
across the passes`, and the gate printed `testgate: PASS`. The budget line is
warn-only — normalized test wall **16m42.353s** (15m52.881s combined, `L`=0.951)
against the 120s reference → `WARN (non-blocking)`; `make check` stays green by
construction (`budget.yaml` `enforcement: warn`). Capture:
`.agents.local/testgate/20260929T222011Z/` (`report.json` plus the per-package
race/no-race `go test -json` streams). The run reported no failed test, no
screen finding, and no invocation error.

Base pair (survey S2), head `da7abd7f`, 2026-09-29: capture
`gate/20260929T052214Z-base-da7abd7f/` under the survey sidecar in
`.agents.local/`.

| window | race pass | no-race pass | combined | tests (race / no-race) | L | normalized |
|---|---|---|---|---|---|---|
| base `da7abd7f` | 1192.5 s | 60.2 s | 1252.8 s | 3713 / 7 | 0.957 | 21m49s |
| final `fc7d9c95` | 823.1 s | 129.6 s | 952.9 s | 3711 / 19 | 0.951 | 16m42s |
| delta | −369.4 s (−31.0%) | +69.4 s | −299.9 s (−23.9%) | | | −23.4% |

The no-race pass grows by design: twelve detector-taxed tests moved into it
(the seven pre-existing partition entries become nineteen). The race pass falls
by more than that, because the moved tests were among its detector-taxed bulk.

### Class aggregates (focused, Class A — sums of the per-row walls quoted above)

| class | measured set | before | after | L companion |
|---|---|---|---|---|
| T3 — DB-setup conversion (golden + skip) | 22 converted tests (#0, #1–#20, #25; #19 is T4-primary and #6 carries a T4 share, kept here because their opens were converted) | 946.7 s | 563.9 s (−40.4%) | 0.945–0.951 (ingest window), 0.928–0.952 (cmd window) |
| T1 — no-race partition | 12 moved entries: the T1 table's nine plus the large-record test and the two build-topology guards (#31/#32/#33) | 171.2 s (race) | 78.7 s (no-race) | 0.952 (partition subset-proof run); per-row 1-min loadavg |
| T2 — SQL statement / seed volume | 3 rows (#23, #7, #4) | 79.6 s | 74.0 s (−7.0%) | 0.952 / 0.951 |
| T4 — fixture/payload construction | 6 invariant payloads (no wall sum; construction is the invariant) | unchanged | unchanged | rows in the T3/T4 tables (0.945–0.952) |
| T6 — packing | #29 and the `internal/api` package | 14.37 s / 102.19 s | 8.01 s / 96.34 s | 0.949 (api subset run) |

Fix-set lens-F aggregate: the 23 tests that carry a lens-F baseline sum
**956.9 s** (the baseline cells above, `L`=0.957; the survey states 957.1 s
rounded) before and **570.6 s** on their current after values — a 40.4% drop.
Eighteen of those cells are warm after the template
cache; the five cold cells (#0, #6, #9, #23, #25) keep their post-conversion
cold records. This is a sum of serial focused runs, not a synchronized suite
run; the suite-level pair is the gate table above. The five extended-bar
additions sit outside those 23: #29 (T6 below), the large-record test and the
two build-topology guards (T1), and the store publication test (T3 section).

### Fixture invariant sizes (unchanged)

The T4 rule is "build once, share read-only; never reduce an invariant-sized
fixture". Sizes before → after are in the T4 table above:

- publication repetitions 4200 → 4200; publication payload text 84085 B → 84085 B;
- native boundary metadata 51175 B, 65537 B → unchanged; boundary padding 1404 B → unchanged;
- selected metadata subtree 52620 B → unchanged; harvest long-text expansion 84000 B (4000 reps) → unchanged;
- #6 padding strings 12000 B per case → unchanged (15 identical cases share one built string);
- #31 record sizes are the invariant: 10 MiB whole-record cases across five harnesses against the 256 MiB production limit.

### Retention test (#0) before / after

`TestUnknownLocalRetentionBeyondTransferBudget` (`internal/ingest`): carried
prior reference **243.6 s**; measured at the base `da7abd7f` **229.9 s** wall
(378.8 user / 26.6 sys, `L`=0.957); after its store-open conversion **210.1 s**
wall (347.3 user / 25.3 sys, `L`=0.945–0.951) — an 8.6% drop, and still the
suite's largest single test. It was not run as a fourth template-cache pair:
the test is a 230 s-class run and the cache mechanism is already spanned by the
three family pairs across three packages (stated in that section, not dropped);
its cost is detector-dominated (82.3% detector flat), so the cache is not its
lever. The remaining lever is the deferred pipeline-body partition.

### L companions (every window)

| window | L (or load) companion |
|---|---|
| survey baseline (lens F) | 0.957 (base-gate calibration) |
| T3 ingest conversion | 0.945 start – 0.951 end; companion `RACE=0 go run ./cmd/testgate run -pkgs ./internal/testkit/coveragemap,./cmd/testgate -race=false` |
| prepared-path cmd conversion | 0.928 start – 0.952 end (same companion) |
| store publication screening | 0.983 → 0.955 |
| partition/fix-set screening rows | large-record 0.961 → 0.967; the three >30 s screen rows are warm serial re-measures with no gate-`L` recorded (1-min loadavg noted per row) |
| T2 SQL/seed | 0.952 start / 0.951 end |
| store-open seam defaults | 0.922 start – 0.949 end |
| template cache A/B | 0.951 (A) – 0.982 (B), 0.952 bracketing the final-code B re-runs |
| partition T1 pairs | per-row 1-min loadavg (3.15–17.33); subset-proof run calibration 0.952 |
| T6 packing | per-row load 7.5→7.1 (before) / 2.1→2.6 (after); api subset run `L`=0.949 |
| warm re-measure batch | per-row 1-min loadavg (`L1`), integration head `8e623e39` |
| final gate / base gate | 0.951 / 0.957 |

### Collateral screen (base vs final, per-test `Elapsed` / `L`)

The screen compares the base and final per-package race streams by per-test
normalized wall (`Elapsed` / that run's printed `L`) and flags a test whose
normalized wall grew more than 20% **and** more than 5 s over a non-zero
baseline. Five flags, each re-measured focused and resolved as load noise:

| test | base reading | focused re-measure | verdict |
|---|---|---|---|
| `cmd/peasant` sessions-list invalid-harness | 9.96 s | 4.00 s | load (the 28.5 s gate reading was the queue) |
| `internal/push` invalid-license | 11.37 s | 4.16 s | load |
| `internal/push` individual-method-error | 10.55 s | 4.03 s | load |
| `internal/store` open-twice-idempotent | 2.72 s | 3.96 s | +1.24 s, under the 5 s absolute threshold |
| `internal/testkit` registry validator | 3.83 s | 5.92 s | +2.09 s, under the threshold (it now walks 20 entries vs 8) |

No focused re-measure grew by 5 s or more. State it as a screen, not a proof:
moving tests out of the race pass changes the packing of the tests that remain
in the same package.
