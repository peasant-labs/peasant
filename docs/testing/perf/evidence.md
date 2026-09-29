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
