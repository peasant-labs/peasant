# Slow-test taxonomy: cost drivers and fix classes

Durable survey record for the race-enabled tests at or above 60 s of packed
occupancy, plus the retention test that is invisible to that lens. Every number
carries the exact command that produced it and the SHA it was run at, or is
labelled carried. Raw artifacts (gate streams, pprof files, focused-run logs)
stay under `.agents.local/` until the epoch closes; the commands below let any
number be re-derived.

## Protocol

Two lenses. **Lens P (packed occupancy):** per-test `Elapsed` from the
`go test -json` stream of a whole-package race run with `-parallel` unpinned;
it absorbs the CPU queue the test waited in, ranks the suite critical path, and
is not any test's own cost. Extracted by parsing `tests[]` from the four
`profile.json` files of the 2026-09-27 packed profile run and selecting
`wall_ms >= 60000` (30 rows) and `> 120000` (20 rows). **Lens F (focused wall):**
one test per process on a quiet box, after one discarded build-cache warmup:

```
<time> go test -race -count=1 -timeout=0 -run '^<Test>$' ./<pkg>
```

`<time>` is GNU time with `-v` (`/usr/bin/time` where present; this survey used
the system GNU time 1.10 — identical format). Record Elapsed (wall), User, and
System. Serial: one focused process at a time while quoting Class A walls.
`L` is the gate's printed calibration for the run window. `L > 4` means
inconclusive: discard and repeat. Never pin `-parallel`; record GOMAXPROCS. No
wall is quoted from a profile run (Class B): profiles classify, Class A quotes.

Stack profiles come from the built-in concurrent profiler:

```
go run ./cmd/testgate profile -pkgs ./internal/ingest,./cmd/peasant
```

(default: re-profile the slowest tests per package, semaphore-limited,
non-observing), then `go tool pprof -top -nodecount=300` per profile. A test
outside a package's top-N is hand-profiled only if needed.

**Rollup rule.** A committed family-share number comes from running
`scripts/perf/rollup.sh` on a `-top` text, never from reading a profile by eye.
The script implements the published matcher rule: detector and adjacent
families are flat-sums over exact symbol lists; the engine, driver, and
application families are flat-sums over literal module-prefix matchers (the
`...` form means the module tree including its root-package dot form);
runtime-other is the remaining `runtime.` rows; other is every unmatched row;
each share divides by the file's total sampled seconds. The script's output on
the carried top-5 ingest profile is pasted below; per-test rollup rows for the
measured set follow in the classification section.

**Compute cap.** Four race-mode heavy passes are budgeted epoch-wide: the base
sweep, the final integration gate, the wave-A registry-proof subset run, and
the api-scoped screen-proof run. Everything else is focused, profile-only, or
per-change before/after pairs.

## Inventory (31 rows)

Lens P is the carried 2026-09-27 packed extraction at the frozen base
`da7abd7f` (re-extracted and asserted: exactly 30 rows ≥ 60 s, exactly 20 rows
> 120 s; 16 `internal/ingest`, 13 `cmd/peasant`, 1 `internal/api`). Lens F is
measured in this survey unless marked inferred. Row 0 is the retention test:
its packed row reads 0 ms because its cost lives in parallel subtests (the
profiler records only top-level `Elapsed`), while this survey measures it at
229.9 s focused and alone (prior reference: 243.6 s carried) — the suite floor. A scripted scan of the 831 zero-wall packed rows (445
ingest / 259 cmd / 80 api / 47 store) plus a bounded code-read review found no
second heavy-bodied parallel-subtest parent; the five plausible-heaviest
parents were screened focused and all came in light (screen table below), so
the inventory stands at 31 rows with no promotion.

| # | lens P (s) | lens F (s) | pkg | test (file) | class | elig | fix | mark |
|---|---|---|---|---|---|---|---|---|
| 0 | 0.0 (parent) | 229.9 | ingest | `TestUnknownLocalRetentionBeyondTransferBudget` (`unknown_local_budget_test.go:31`) | T1 (see note) | A | one-line skip (before/after decides) | measured |
| 1 | 346.9 | 78.0 | ingest | `TestPiCapturedAdmission` (`pi_capture_test.go:77`) | T3 | A | ingest golden+skip | measured |
| 2 | 343.4 | 75.7 | ingest | `TestPublicationCaptureNormalIngestRecovery` (`publication_capture_test.go:105`) | T3 | A | ingest golden+skip | measured |
| 3 | 318.5 | 43.5 | ingest | `TestNormalIngestStoresAuthoritativeContent` (`content_capture_test.go:197`) | T3 | A | ingest golden+skip | measured |
| 4 | 313.9 | 41.1 | ingest | `TestResolveStoredOriginsWritesAVerdictIntoEveryRow` (`origin_backfill_test.go:481`) | T3 | S | ingest golden+skip + wave-A entry | measured |
| 5 | 294.0 | 31.0 | ingest | `TestOrdinaryHarvestSettlesStaleIndexSessions` (`stale_index_settling_test.go:86`) | T3 | A | ingest golden+skip | measured |
| 6 | 282.3 | 43.1 | ingest | `TestPiUnknownPersistence` (`pi_unknown_test.go:193`) | T3+T4 | A | ingest golden+skip + payload share | measured |
| 7 | 277.1 | 28.3 | ingest | `TestRetainedContentBackfill` (`content_backfill_test.go:25`) | T3 | A | ingest golden+skip (+ seed batching) | measured |
| 8 | 273.6 | 53.2 | cmd | `TestPiNativeRegistryProjection` (`pi_native_integration_test.go:187`) | T3+T4 | A | cmd golden+skip + payload share | measured |
| 9 | 269.5 | 106.6 | ingest | `TestNativeUnknownSourceToPublication` (`native_unknown_public_test.go:110`) | T3 | A | ingest golden+skip | measured |
| 10 | 262.4 | 22.3 | cmd | `TestPiHarvestCommonModes` (`pi_ingestion_test.go:85`) | T3+T4 | A | cmd golden+skip + payload share | measured |
| 11 | 249.4 | 17.1 | cmd | `TestMountedKickstartStoredGateAlignsViewerAndPush` (`cmd_kickstart_stored_gate_alignment_test.go:587`) | T3 | S | cmd golden+skip + wave-A entry | measured |
| 12 | 241.8 | 17.8 | ingest | `TestContentRecoveryScope` (`content_recovery_scope_test.go:148`) | T3 | A | ingest golden+skip | measured |
| 13 | 233.4 | 18.0 | ingest | `TestPipelineRetainedAdapterMaintenance` (`adapter_maintenance_test.go:108`) | T3 | A | ingest golden+skip | measured |
| 14 | 209.3 | 14.1 | cmd | `TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection` (`index_format_queries_test.go:81`) | T3 | S | cmd golden+skip + wave-A entry | measured |
| 15 | 194.2 | 14.0 | ingest | `TestUnknownPrivateEncoding` (`unknown_private_encoding_test.go:21`) | T3 | S | ingest golden+skip + wave-A entry | measured |
| 16 | 182.3 | 13.6 | cmd | `TestMountedLegacySelectedConversion_ConsentCancellationAndRerun` (`cmd_kickstart_selected_conversion_mount_test.go:261`) | T3 | S | cmd golden+skip + wave-A entry | measured |
| 17 | 174.4 | 14.7 | cmd | `TestLegacyOpenCodeSQLiteSourceInfoRecoveryValidatesManagedEnvelope` (`opencode_legacy_sqlite_recovery_test.go:397`) | T3 | A | cmd golden+skip | measured |
| 18 | 156.0 | 13.1 | ingest | `TestConcreteParserFailurePreservesOtherSessions` (`indexer_completion_pipeline_test.go:19`) | T3 | A | ingest golden+skip | measured |
| 19 | 132.8 | 35.4 | cmd | `TestPiDatabasePublicationThroughCLI` (`pi_database_publication_test.go:25`) | T4+T3 | A | cmd golden+skip + payload share | measured |
| 20 | 123.7 | 9.8 | cmd | `TestKickstartLocalIngestPreservesCommittedSelectionAtRunnerBoundary` (`cmd_kickstart_selection_runner_test.go:187`) | T3+T4 | A | cmd golden+skip | measured |
| 21 | 119.8 | — (inferred light) | cmd | `TestPublicationWizardAndReportUseDatabaseReadiness` (`publication_readiness_test.go:22`) | T3 | S | wave-A entry only | inferred |
| 22 | 107.9 | — (inferred light) | ingest | `TestWritePathDurability` (`write_path_durability_test.go:87`) | T1/T2/T3 | A | tail (no fix this epoch) | inferred |
| 23 | 78.1 | 10.2 | cmd | `TestModelsSync_500_StaticFallback` (`cmd_models_test.go:328`) | T2 | S | prepared-statement reuse + wave-A entry | measured |
| 24 | 74.2 | — (inferred light) | cmd | `TestLegacyOpenCodeSQLiteMountedHarvestCreatesManagedIndexedAnalyticsState` (`opencode_legacy_sqlite_mount_test.go:221`) | T3 | A | tail (no fix this epoch) | inferred |
| 25 | 70.0 | 26.4 | ingest | `TestNativeCoverageMatrix` (`native_coverage_test.go:192`) | T3 | S | one-line skip + wave-A entry | measured |
| 26 | 66.6 | — (inferred light) | ingest | `TestWritePathMirrorsInPagesOf256` (`write_path_columns_test.go:230`) | T1/T2/T6 | A | tail (no fix this epoch) | inferred |
| 27 | 66.3 | — (inferred light) | cmd | `TestOpenCodeSessionClockFixturesMountedHarvest` (`opencode_session_clock_mount_test.go:91`) | T3 | A | tail (no fix this epoch) | inferred |
| 28 | 64.2 | — (inferred light) | ingest | `TestControlRecordIngestExportAndPublication` (`control_record_ingest_test.go:67`) | T3 | A | tail (no fix this epoch) | inferred |
| 29 | 61.5 | — (inferred light) | api | `TestHelperGroupListingThroughRegisteredRoutes` (`helper_group_listing_test.go:176`) | T6 | A | packing proof point | inferred |
| 30 | 61.4 | — (inferred light) | cmd | `TestKickstartRescan_FallsBackWithoutCompatibleDatabase` (`cmd_kickstart_rescan_test.go:491`) | T3 | S | wave-A entry only (subject is the missing-DB fallback; never converted) | inferred |

Elig: S = no goroutines on the exercised production path (admits wave A);
A = production concurrency exercised but not the subject (keeps the detector
tax). Rows 0–20 plus 23/25 are measured; the tail is structural (code read +
carried walls + existing profiles).

### Focus checkpoint (lens F vs packed-derived 20)

No packed-derived-20 test exceeds 120 s focused: the highest measured lens-F
walls are #9 at 106.6 s, #1 at 78.0 s, #2 at 75.7 s; only the retention test
#0 (229.9 s) is above the line. No test outside the packed-derived 20 was
found above 120 s focused, so the union rule adds no member: the fix set is
the packed-derived 20 plus #0, unchanged. Sum of the 23 measured lens-F walls:
957.1 s (L=0.957; per-test user+sys recorded in the evidence baseline).

### Zero-wall screen (no promotion)

The five plausible-heaviest zero-wall parents, screened focused-and-alone
under race (same Class A command, serial, base `da7abd7f`, L=0.957):

| test | lens-F wall (s) | verdict |
|---|---|---|
| `TestPipelineHarvesterTargets` (ingest) | 7.57 | light — no promotion |
| `TestMetadataChildCompatibility` (ingest) | 5.10 | light — no promotion |
| `TestPipeline_CommitHistoryCapture` (ingest) | 5.33 | light — no promotion |
| `TestMountedSelectionPushChooserPipelineAndPruneKeepTheCloneBoundary` (cmd) | 8.51 | light — no promotion |
| `TestHarvestHarvesterSummary` (cmd) | 4.66 | light — no promotion |

Screen logs under `.agents.local/` (sidecar, re-derivable). The other
zero-wall candidates were ruled out by code read: skipped tests, golden-DB
users, MemFS/stub-store tests, and unit/matrix tests with no store or pipeline
work. The inventory stands at 31 rows.

## Classification

Primary class follows the measured attribution under the rollup rule: T3 when
the migration root (`sqlitemigration.Migrate`) cum ≥ 25 %; T1 when the detector
flat-sum (boundary + adjacency) ≥ 60 % of samples and the test has no
concurrency subject; T2 when `(*Conn).PrepareTransient` cum ≥ 8 % with the
migration root below the T3 threshold, or named static-text call sites sum ≥
5 % flat; T4 when test-helper fixture construction sums ≥ 10 % flat (excluding
payloads that are the invariant); T6 when achieved package parallelism is far
below core count with wall ≈ CPU. Ties above 25 % record both with the larger
projected share primary; measured attribution beats the structural read, with
the discrepancy noted.

### Worked example (carried top-5 ingest profile, via `scripts/perf/rollup.sh`)

```
== <sidecar>/20260929T010208Z-ingest-top5-c30e66da/top300.txt
detector          7 rows    190.97 s  60.98%
adjacent          3 rows     37.59 s  12.00%
runtime-other    13 rows     23.21 s   7.41%
engine          156 rows     36.45 s  11.64%
driver           16 rows      0.03 s   0.01%
application      48 rows      0.00 s   0.00%
other             9 rows      5.23 s   1.67%
accounted       252 rows    293.48 s  93.71%  (total sampled 313.19s)
```

Reconciliation: 190.97 + 37.59 + 23.21 + 36.45 + 0.03 + 0.00 + 5.23 = 293.48 s
= 93.71 % of 313.19 s sampled. Detector boundary + adjacency = 72.98 % of
sampled CPU. `sqlitemigration.Migrate` 167.14 s / 53.37 % cum; `store.Open`
186.65 s / 59.60 % cum; `(*Conn).PrepareTransient` 57.41 s / 18.33 % cum vs
connection-cached `sqlitex.Execute` 1.70 s / 0.54 % cum. `encoding/json` and
the redaction engine: zero rows in the top 300.

### Per-test rollup (measured set)

Measured classification outcome (rules above applied to the pasted table):

- **T3 primary (20 tests):** every measured test except #0, #19, #23 — migration
  root cum 26.0–74.1 % (lowest: #9 at 26.0 %, #6 at 29.5 %; `store.Open` cum
  tracks it within a few points). Nearly all carry a T2 secondary
  (`PrepareTransient` cum 11–26 %) and a T1 secondary (detector boundary +
  adjacency 72–82 % flat, deferred with wave B).
- **T2 primary (#23):** migration root only 15.0 % cum while
  `PrepareTransient` reaches 45.9 % cum — the models-sync prepared-reuse fix.
- **T4 primary (#19):** migration root 10.5 % cum; cost sits in the harvest CLI
  + push pipeline + redaction over the 4200-repetition payload (`RedactJSON`
  13.0 % cum, `Detect` 8.4 % cum, `store.Open` 10.9 % cum), detector-taxed at
  82.0 % flat. The payload-share fix applies; T3 is secondary.
- **T1-shape (#0, discrepancy noted):** detector flat 82.3 % with migration
  root 0.0 % cum and `store.Open` 0.6 % cum — the golden copies sit at head, so
  no migration script runs and the skip branch projects ~0 % of sampled CPU.
  The cost is detector-taxed retained-unknown JSON scanning
  (`CollectRetainedUnknown` 8.1 % cum, `ScanRawEvidenceDocument` 7.0 % cum,
  `encoding/json` + `regexp` frames in other at 8.1 % flat). The planned
  one-line skip conversion still gets its before/after pair; if it shows no
  gain it is recorded measured-inapplicable and the row stays T1 (partition
  deferred).

Past `rollup.sh` output over the 23 per-test `-top` texts (`==` headers carry the test name; full paths in the sidecar copy):

```
== TestUnknownLocalRetentionBeyondTransferBudget
detector          6 rows    326.11 s  82.30%
adjacent          0 rows      0.00 s   0.00%
runtime-other     6 rows     23.56 s   5.95%
engine            9 rows      0.57 s   0.14%
driver            4 rows      0.00 s   0.00%
application      66 rows      0.00 s   0.00%
other            33 rows     31.92 s   8.06%
accounted       124 rows    382.16 s  96.45%  (total sampled 396.24s)
== TestPiCapturedAdmission
detector          7 rows     48.87 s  60.52%
adjacent          3 rows     10.69 s  13.24%
runtime-other     6 rows      4.07 s   5.04%
engine          166 rows     10.45 s  12.94%
driver           14 rows      0.02 s   0.02%
application      45 rows      0.00 s   0.00%
other             5 rows      1.25 s   1.55%
accounted       246 rows     75.35 s  93.31%  (total sampled 80.75s)
== TestPublicationCaptureNormalIngestRecovery
detector          7 rows     45.49 s  60.39%
adjacent          3 rows      9.82 s  13.04%
runtime-other     7 rows      3.52 s   4.67%
engine          168 rows     10.01 s  13.29%
driver           15 rows      0.02 s   0.03%
application      47 rows      0.00 s   0.00%
other             5 rows      1.38 s   1.83%
accounted       252 rows     70.24 s  93.24%  (total sampled 75.33s)
== TestNormalIngestStoresAuthoritativeContent
detector          7 rows     26.39 s  61.26%
adjacent          3 rows      6.39 s  14.83%
runtime-other     6 rows      2.04 s   4.74%
engine          162 rows      5.61 s  13.02%
driver           16 rows      0.03 s   0.07%
application      39 rows      0.00 s   0.00%
other             1 rows      0.00 s   0.00%
accounted       234 rows     40.46 s  93.92%  (total sampled 43.08s)
== TestResolveStoredOriginsWritesAVerdictIntoEveryRow
detector          7 rows     23.22 s  60.79%
adjacent          3 rows      5.39 s  14.11%
runtime-other     6 rows      2.19 s   5.73%
engine          157 rows      4.79 s  12.54%
driver           13 rows      0.01 s   0.03%
application      25 rows      0.00 s   0.00%
other             8 rows      0.52 s   1.36%
accounted       219 rows     36.12 s  94.55%  (total sampled 38.20s)
== TestOrdinaryHarvestSettlesStaleIndexSessions
detector          7 rows     16.59 s  59.63%
adjacent          3 rows      3.74 s  13.44%
runtime-other     6 rows      1.36 s   4.89%
engine          174 rows      4.13 s  14.85%
driver           13 rows      0.00 s   0.00%
application      42 rows      0.00 s   0.00%
other             4 rows      0.23 s   0.83%
accounted       249 rows     26.05 s  93.64%  (total sampled 27.82s)
== TestPiUnknownPersistence
detector          7 rows     29.36 s  69.05%
adjacent          3 rows      3.14 s   7.38%
runtime-other     4 rows      2.00 s   4.70%
engine          101 rows      2.74 s   6.44%
driver           14 rows      0.02 s   0.05%
application      67 rows      0.01 s   0.02%
other            34 rows      1.67 s   3.93%
accounted       230 rows     38.94 s  91.58%  (total sampled 42.52s)
== TestRetainedContentBackfill
detector          7 rows     12.35 s  48.95%
adjacent          3 rows      7.99 s  31.67%
runtime-other     4 rows      0.95 s   3.77%
engine          183 rows      1.90 s   7.53%
driver           16 rows      0.01 s   0.04%
application      79 rows      0.00 s   0.00%
other             8 rows      0.26 s   1.03%
accounted       300 rows     23.46 s  92.98%  (total sampled 25.23s)
== TestNativeUnknownSourceToPublication
detector          7 rows     70.49 s  69.53%
adjacent          3 rows      6.89 s   6.80%
runtime-other    14 rows      5.83 s   5.75%
engine           94 rows      5.74 s   5.66%
driver           14 rows      0.02 s   0.02%
application      58 rows      0.00 s   0.00%
other            33 rows      3.92 s   3.87%
accounted       223 rows     92.89 s  91.63%  (total sampled 101.38s)
== TestContentRecoveryScope
detector          7 rows      7.11 s  48.93%
adjacent          3 rows      4.76 s  32.76%
runtime-other     6 rows      0.70 s   4.82%
engine          145 rows      1.33 s   9.15%
driver           13 rows      0.00 s   0.00%
application      34 rows      0.00 s   0.00%
other             3 rows      0.08 s   0.55%
accounted       211 rows     13.98 s  96.21%  (total sampled 14.53s)
== TestPipelineRetainedAdapterMaintenance
detector          7 rows      9.14 s  61.55%
adjacent          3 rows      2.05 s  13.80%
runtime-other    10 rows      0.60 s   4.04%
engine          166 rows      1.82 s  12.26%
driver           13 rows      0.01 s   0.07%
application      36 rows      0.00 s   0.00%
other             7 rows      0.23 s   1.55%
accounted       242 rows     13.85 s  93.27%  (total sampled 14.85s)
== TestUnknownPrivateEncoding
detector          7 rows      7.07 s  62.73%
adjacent          3 rows      1.60 s  14.20%
runtime-other     5 rows      0.49 s   4.35%
engine          167 rows      1.48 s  13.13%
driver           16 rows      0.00 s   0.00%
application      16 rows      0.00 s   0.00%
other             4 rows      0.16 s   1.42%
accounted       218 rows     10.80 s  95.83%  (total sampled 11.27s)
== TestConcreteParserFailurePreservesOtherSessions
detector          7 rows      5.96 s  60.20%
adjacent          3 rows      1.32 s  13.33%
runtime-other     5 rows      0.58 s   5.86%
engine          163 rows      1.34 s  13.54%
driver           12 rows      0.00 s   0.00%
application      38 rows      0.00 s   0.00%
other            11 rows      0.07 s   0.71%
accounted       239 rows      9.27 s  93.64%  (total sampled 9.90s)
== TestNativeCoverageMatrix
detector          7 rows     15.99 s  58.94%
adjacent          3 rows      3.72 s  13.71%
runtime-other     6 rows      1.45 s   5.34%
engine          166 rows      3.67 s  13.53%
driver           18 rows      0.00 s   0.00%
application      14 rows      0.00 s   0.00%
other             4 rows      0.47 s   1.73%
accounted       218 rows     25.30 s  93.25%  (total sampled 27.13s)
== TestPiNativeRegistryProjection
detector          7 rows     30.61 s  63.04%
adjacent          3 rows      5.88 s  12.11%
runtime-other     6 rows      2.85 s   5.87%
engine          155 rows      5.73 s  11.80%
driver           13 rows      0.00 s   0.00%
application      14 rows      0.00 s   0.00%
other             7 rows      0.71 s   1.46%
accounted       205 rows     45.78 s  94.28%  (total sampled 48.56s)
== TestPiHarvestCommonModes
detector          7 rows     11.94 s  62.06%
adjacent          3 rows      2.39 s  12.42%
runtime-other     5 rows      0.76 s   3.95%
engine          173 rows      2.53 s  13.15%
driver           13 rows      0.00 s   0.00%
application      50 rows      0.00 s   0.00%
other            12 rows      0.26 s   1.35%
accounted       263 rows     17.88 s  92.93%  (total sampled 19.24s)
== TestMountedKickstartStoredGateAlignsViewerAndPush
detector          7 rows      8.69 s  58.32%
adjacent          3 rows      2.17 s  14.56%
runtime-other     5 rows      0.61 s   4.09%
engine          160 rows      2.18 s  14.63%
driver           14 rows      0.00 s   0.00%
application       8 rows      0.00 s   0.00%
other             5 rows      0.30 s   2.01%
accounted       202 rows     13.95 s  93.62%  (total sampled 14.90s)
== TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection
detector          7 rows      8.09 s  61.94%
adjacent          3 rows      2.03 s  15.54%
runtime-other     6 rows      0.56 s   4.29%
engine          155 rows      1.58 s  12.10%
driver           13 rows      0.01 s   0.08%
application      16 rows      0.00 s   0.00%
other             7 rows      0.20 s   1.53%
accounted       207 rows     12.47 s  95.48%  (total sampled 13.06s)
== TestMountedLegacySelectedConversion_ConsentCancellationAndRerun
detector          7 rows      6.44 s  57.65%
adjacent          3 rows      1.59 s  14.23%
runtime-other     6 rows      0.71 s   6.36%
engine          168 rows      1.59 s  14.23%
driver           13 rows      0.00 s   0.00%
application      12 rows      0.01 s   0.09%
other             4 rows      0.17 s   1.52%
accounted       213 rows     10.51 s  94.09%  (total sampled 11.17s)
== TestLegacyOpenCodeSQLiteSourceInfoRecoveryValidatesManagedEnvelope
detector          7 rows      7.00 s  57.47%
adjacent          3 rows      1.82 s  14.94%
runtime-other    11 rows      0.62 s   5.09%
engine          168 rows      1.49 s  12.23%
driver           14 rows      0.02 s   0.16%
application      69 rows      0.00 s   0.00%
other            12 rows      0.24 s   1.97%
accounted       284 rows     11.19 s  91.87%  (total sampled 12.18s)
== TestPiDatabasePublicationThroughCLI
detector          7 rows     28.35 s  80.13%
adjacent          2 rows      0.65 s   1.84%
runtime-other     5 rows      1.75 s   4.95%
engine           24 rows      0.47 s   1.33%
driver           10 rows      0.01 s   0.03%
application      20 rows      0.00 s   0.00%
other            31 rows      2.02 s   5.71%
accounted        99 rows     33.25 s  93.98%  (total sampled 35.38s)
== TestKickstartLocalIngestPreservesCommittedSelectionAtRunnerBoundary
detector          7 rows      4.76 s  65.84%
adjacent          3 rows      0.91 s  12.59%
runtime-other     6 rows      0.36 s   4.98%
engine          152 rows      0.77 s  10.65%
driver           13 rows      0.00 s   0.00%
application      26 rows      0.00 s   0.00%
other             7 rows      0.10 s   1.38%
accounted       214 rows      6.90 s  95.44%  (total sampled 7.23s)
== TestModelsSync_500_StaticFallback
detector          7 rows      3.28 s  42.43%
adjacent          3 rows      2.99 s  38.68%
runtime-other     5 rows      0.37 s   4.79%
engine          155 rows      0.60 s   7.76%
driver           14 rows      0.00 s   0.00%
application       9 rows      0.00 s   0.00%
other            11 rows      0.16 s   2.07%
accounted       204 rows      7.40 s  95.73%  (total sampled 7.73s)
```

## Fix classes and their closure

- **T1 — detector tax on bulk bytes (wave A only).** Nine STRONG-eligible tests
  move to the no-race pass with a written short-form soundness argument each
  (subject; concurrency actually exercised; why the detector is not this test's
  oracle; retained race coverage; residual risk). No registry contract change;
  pipeline partition is deferred. Arguments: (section filled by the partition
  change — nine entries, one per moved test).
- **T2 — SQL statement and seed volume.** `Store.SyncModels` prepared reuse;
  static-text transient → cached `sqlitex.Execute` at the named constant-text
  call sites; seed-loop batching in `content_backfill_test.go`. Dynamic-text
  sites (`BulkLookupSessionLocations`, `selectSessionIDs`,
  `ListStaleIndexSessions`, `validateIndexFormatQueryOnConn`) are
  measured-inapplicable this epoch (text varies per call; constant-text guard).
  `preparePragmas` excluded (once per connection; caching buys nothing).
  `applyV23DataMigration` is T3-covered (non-skip branch only).
- **T3 — per-test DB setup (dominant).** Fresh opens → golden pre-migrated DB +
  `WithSkipMigrations` (owned store for its whole life), or golden copy + skip
  where the path must be stable across close/reopen. The CLI cannot skip
  migrations: the cmd win is zero pending migrations on a prepared path.
  Never converted: migration/creation-behavior subjects (#30), per-case
  NotExist assertions, open-time refusal assertions (`store.go:409`).
- **T4 — fixture/payload construction.** Build once per test, share read-only:
  4200-repetition payload (#19), fixed large metadata strings (#8), `longText`
  case (#10), padding strings (#6). No invariant-sized fixture is reduced.
  Invariant sizes before/after live in `evidence.md`.
- **T5 — subprocess.** Inapplicable: no subprocess use drives any of the 31
  rows (structural line; no fix).
- **T6 — packing.** Proof point `helper_group_listing_test.go:176` (26
  independent cases, already golden); further `t.Parallel()` only where the
  post-cut measurement shows an admissible CPU-bound residual, the
  parallel-unsafe guard does not name it, and no process-global sink exists.
  Never pin `-parallel`.
- **T7 — test-structure breadth.** Structural only: giant fixture tables stay
  whole this epoch; breadth is recorded per row, not split (no fix).
- **T8 — engine/dependency.** Inapplicable on the pure-Go stack: the SQLite
  engine is transpiled Go under the same detector; no CGO swap, no checkptr
  change (structural line; no fix).