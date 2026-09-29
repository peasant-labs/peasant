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

After state: the fourteen ingest files named in the slice route every
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
| `TestPiCapturedAdmission` (ingest) | T3 | 78.0 / 73.9 / 2.2 | 82.0 / 76.0 / 2.2 (rerun 82.0) | 0.945–0.951 |
| `TestPublicationCaptureNormalIngestRecovery` (ingest) | T3 | 75.7 / 68.2 / 2.1 | 79.2 / 71.1 / 2.4 (rerun 78.8) | 0.945–0.951 |
| `TestNormalIngestStoresAuthoritativeContent` (ingest) | T3 | 43.5 / 40.1 / 1.5 | 47.2 / 41.7 / 1.7 (rerun 45.9) | 0.945–0.951 |
| `TestPiUnknownPersistence` (ingest) | T3+T4 | 43.1 / 40.2 / 1.6 | 41.7 / 38.8 / 1.5 | 0.945–0.951 |
| `TestResolveStoredOriginsWritesAVerdictIntoEveryRow` (ingest) | T3 | 41.1 / 38.0 / 1.3 | 44.3 / 39.8 / 1.4 (rerun 42.9) | 0.945–0.951 |
| `TestOrdinaryHarvestSettlesStaleIndexSessions` (ingest) | T3 | 31.0 / 28.5 / 1.3 | 33.3 / 29.9 / 1.4 (rerun 32.7) | 0.945–0.951 |
| `TestRetainedContentBackfill` (ingest) | T3 | 28.3 / 26.0 / 1.2 | 28.0 / 26.2 / 1.2 | 0.945–0.951 |
| `TestNativeCoverageMatrix` (ingest) | T3 | 26.4 / 21.9 / 1.2 | 24.9 / 21.1 / 1.1 | 0.945–0.951 |
| `TestPipelineRetainedAdapterMaintenance` (ingest) | T3 | 18.0 / 16.1 / 1.2 | 18.4 / 16.4 / 1.1 | 0.945–0.951 |
| `TestContentRecoveryScope` (ingest) | T3 | 17.8 / 15.8 / 1.0 | 17.9 / 15.9 / 0.9 | 0.945–0.951 |
| `TestUnknownPrivateEncoding` (ingest) | T3 | 14.0 / 12.2 / 1.0 | 15.2 / 13.3 / 0.9 (rerun 15.3) | 0.945–0.951 |
| `TestConcreteParserFailurePreservesOtherSessions` (ingest) | T3 | 13.1 / 11.6 / 0.9 | 13.5 / 11.8 / 0.9 | 0.945–0.951 |
| `TestContentStageDetectsTornPair` (ingest) | T3 | — (no baseline row; converted with its file) | 4.3 / 3.3 / 0.7 | 0.945–0.951 |

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
| `TestPiNativeRegistryProjection` (cmd) | T3+T4 | 53.2 / 49.0 / 1.9 | 53.95 / 49.52 / 1.77 | 0.928–0.952 |
| `TestPiDatabasePublicationThroughCLI` (cmd) | T4+T3 | 35.4 / 33.4 / 1.6 | 37.62 / 35.76 / 1.31 | 0.928–0.952 |
| `TestPiHarvestCommonModes` (cmd) | T3+T4 | 22.3 / 19.8 / 1.5 | 22.31 / 19.72 / 1.43 | 0.928–0.952 |
| `TestMountedKickstartStoredGateAlignsViewerAndPush` (cmd) | T3 | 17.1 / 14.4 / 1.2 | 16.55 / 14.20 / 1.04 | 0.928–0.952 |
| `TestLegacyOpenCodeSQLiteSourceInfoRecoveryValidatesManagedEnvelope` (cmd) | T3 | 14.7 / 12.0 / 1.4 | 14.29 / 11.65 / 1.35 | 0.928–0.952 |
| `TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection` (cmd) | T3 | 14.1 / 12.4 / 1.1 | 14.35 / 12.55 / 0.96 | 0.928–0.952 |
| `TestMountedLegacySelectedConversion_ConsentCancellationAndRerun` (cmd) | T3 | 13.6 / 11.2 / 1.1 | 13.48 / 11.30 / 1.03 | 0.928–0.952 |
| `TestKickstartLocalIngestPreservesCommittedSelectionAtRunnerBoundary` (cmd) | T3+T4 | 9.8 / 8.1 / 1.0 | 9.88 / 8.32 / 0.91 | 0.928–0.952 |

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
| `TestPublicationFullCaptureEligibilityAndBundle` (store) | T3 | `go test -race -count=1 -timeout=0 -run '^TestPublicationFullCaptureEligibilityAndBundle$' ./internal/store` (one discarded warmup at 33.8 s, serial, GNU `time -v`) | 32.4 wall / 29.08 user / 1.25 sys | 34.5 wall / 31.00 user / 1.13 sys | 0.983 → 0.955 |

Wall-neutral within load noise: the test already opened golden copies, so the
removed per-subtest cost was the migration-state check only (no replay). The
`-cpuprofile` mechanism check confirms it — the remaining `store.Open` cost
(22.3 s cum, 72.8% of samples) sits entirely under
`storetest.ensureGolden → sqlitemigration.Migrate` (21.2 s cum), i.e. the
once-per-process template build owned by the in-flight persistent-template
amendment, not this test's opens (profile run, not quoted as a wall). The
conversion's durable value is enforcement-rule cleanliness (skip-bearing open,
no suppression). T4 payload share: measured-inapplicable, not applied — the
read-only mutation audit found no in-place entry writes (writer path reads
`EntryIndex` only; the backfill already copies before mutating; sequential
subtests; per-subtest fresh DBs), but the expected win is string-build only
while per-subtest DB indexing dominates.

## Screening additions: partition and fix-set pointers

The remaining four confirmed >30 s screening findings. Walls in this section
were taken on the epoch tree (`09d9e93c`), not the `da7abd7f` survey base; the
row shape above still holds (test → class → exact command → before wall/CPU →
after wall/CPU → L). After-columns marked pending are quoted by their owning
change — the partition entries and the fix-set pair — and are not duplicated
here. The store publication test's conversion pair is recorded in the section
above and is not repeated; its final warm pair lands after the
persistent-template amendment rebuilds the once-per-process template build.

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
| `TestOpenCodePrivateExecutionGuardCoversFixtureOwnedBuildTopology` (ingest) | partition, toolchain/static-analysis character | `go test -race -count=1 -timeout=0 -run '^TestOpenCodePrivateExecutionGuardCoversFixtureOwnedBuildTopology$' ./internal/ingest` (warm serial re-measure, GNU `time -v`, epoch tree) | screen 45.0 / warm 42.72 wall (73.44 user / 30.52 sys) | pending — partition entry (after-wall quoted there) | not recorded (screening re-measure; the entry pair carries L) |
| `TestOpenCodePrivateExecutionGuardRejectsFixtureOwnedBuildTaggedBypasses` (ingest) | partition, toolchain/static-analysis character | `go test -race -count=1 -timeout=0 -run '^TestOpenCodePrivateExecutionGuardRejectsFixtureOwnedBuildTaggedBypasses$' ./internal/ingest` (warm serial re-measure, GNU `time -v`, epoch tree) | screen 33.0 / warm 31.94 wall (54.17 user / 22.57 sys) | pending — partition entry (after-wall quoted there) | not recorded (screening re-measure; the entry pair carries L) |
| `TestHelperGroupListingThroughRegisteredRoutes` (api) | T6 | `go test -race -count=1 -timeout=0 -run '^TestHelperGroupListingThroughRegisteredRoutes$' ./internal/api` (warm serial re-measure, GNU `time -v`, epoch tree) | screen 63.1 / warm 52.35 wall (47.87 user / 1.47 sys) | pending — fix-set pair (after-wall quoted there) | not recorded (screening re-measure; the pair carries L) |
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

## T3 focus restoration: storetest template cache

The `storetest` template was built once per process (61 migrations under
`ensureGolden`), so a focused run paid a full migration pass on top of the
test's own work. The cache (`internal/store/storetest/golden.go` rewritten;
new `internal/filelock` leaf; `store.SchemaFingerprint()` stamp;
tmpfs-preferred managed copy root; read-once template buffer) makes the
template reusable across processes. A tree is the epoch integration branch at
`764aa87f` (with the seam-default conversions), B tree is the L6 worktree at
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
  195 after every L6 run — no new `TMPDIR` litter (managed roots only; the
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
