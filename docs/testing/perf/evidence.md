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
