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
.` exits 0 (the enforcement rule file itself lands with the enforcement leaf).
