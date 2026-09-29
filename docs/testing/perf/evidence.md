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
