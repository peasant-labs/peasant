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
