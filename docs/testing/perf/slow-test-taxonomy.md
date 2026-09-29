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
profiler records only top-level `Elapsed`), while it costs 243.6 s focused and
alone — the suite floor. A scripted scan of the 831 zero-wall packed rows (445
ingest / 259 cmd / 80 api / 47 store) plus a bounded code-read review found no
second heavy-bodied parallel-subtest parent; the five plausible-heaviest
parents were screened focused and all came in light (screen table below), so
the inventory stands at 31 rows with no promotion.

| # | lens P (s) | lens F (s) | pkg | test (file) | class | elig | fix | mark |
|---|---|---|---|---|---|---|---|---|
| 0 | 0.0 (parent) | FILL | ingest | `TestUnknownLocalRetentionBeyondTransferBudget` (`unknown_local_budget_test.go:31`) | T3 | A | ingest golden+skip | measured |
| 1 | 346.9 | FILL | ingest | `TestPiCapturedAdmission` (`pi_capture_test.go:77`) | T3 | A | ingest golden+skip | measured |
| 2 | 343.4 | FILL | ingest | `TestPublicationCaptureNormalIngestRecovery` (`publication_capture_test.go:105`) | T3 | A | ingest golden+skip | measured |
| 3 | 318.5 | FILL | ingest | `TestNormalIngestStoresAuthoritativeContent` (`content_capture_test.go:197`) | T3 | A | ingest golden+skip | measured |
| 4 | 313.9 | FILL | ingest | `TestResolveStoredOriginsWritesAVerdictIntoEveryRow` (`origin_backfill_test.go:481`) | T3 | S | ingest golden+skip + wave-A entry | measured |
| 5 | 294.0 | FILL | ingest | `TestOrdinaryHarvestSettlesStaleIndexSessions` (`stale_index_settling_test.go:86`) | T3 | A | ingest golden+skip | measured |
| 6 | 282.3 | FILL | ingest | `TestPiUnknownPersistence` (`pi_unknown_test.go:193`) | T3+T4 | A | ingest golden+skip + payload share | measured |
| 7 | 277.1 | FILL | ingest | `TestRetainedContentBackfill` (`content_backfill_test.go:25`) | T3 | A | ingest golden+skip (+ seed batching) | measured |
| 8 | 273.6 | FILL | cmd | `TestPiNativeRegistryProjection` (`pi_native_integration_test.go:187`) | T4+T3 | A | cmd golden+skip + payload share | measured |
| 9 | 269.5 | FILL | ingest | `TestNativeUnknownSourceToPublication` (`native_unknown_public_test.go:110`) | T3 | A | ingest golden+skip | measured |
| 10 | 262.4 | FILL | cmd | `TestPiHarvestCommonModes` (`pi_ingestion_test.go:85`) | T3+T4 | A | cmd golden+skip + payload share | measured |
| 11 | 249.4 | FILL | cmd | `TestMountedKickstartStoredGateAlignsViewerAndPush` (`cmd_kickstart_stored_gate_alignment_test.go:587`) | T3 | S | cmd golden+skip + wave-A entry | measured |
| 12 | 241.8 | FILL | ingest | `TestContentRecoveryScope` (`content_recovery_scope_test.go:148`) | T3 | A | ingest golden+skip | measured |
| 13 | 233.4 | FILL | ingest | `TestPipelineRetainedAdapterMaintenance` (`adapter_maintenance_test.go:108`) | T3 | A | ingest golden+skip | measured |
| 14 | 209.3 | FILL | cmd | `TestIndexFormatCommandsValidateScopedCandidatesBeforeProjection` (`index_format_queries_test.go:81`) | T3 | S | cmd golden+skip + wave-A entry | measured |
| 15 | 194.2 | FILL | ingest | `TestUnknownPrivateEncoding` (`unknown_private_encoding_test.go:21`) | T3 | S | ingest golden+skip + wave-A entry | measured |
| 16 | 182.3 | FILL | cmd | `TestMountedLegacySelectedConversion_ConsentCancellationAndRerun` (`cmd_kickstart_selected_conversion_mount_test.go:261`) | T3 | S | cmd golden+skip + wave-A entry | measured |
| 17 | 174.4 | FILL | cmd | `TestLegacyOpenCodeSQLiteSourceInfoRecoveryValidatesManagedEnvelope` (`opencode_legacy_sqlite_recovery_test.go:397`) | T3 | A | cmd golden+skip | measured |
| 18 | 156.0 | FILL | ingest | `TestConcreteParserFailurePreservesOtherSessions` (`indexer_completion_pipeline_test.go:19`) | T3 | A | ingest golden+skip | measured |
| 19 | 132.8 | FILL | cmd | `TestPiDatabasePublicationThroughCLI` (`pi_database_publication_test.go:25`) | T4+T3 | A | cmd golden+skip + payload share | measured |
| 20 | 123.7 | FILL | cmd | `TestKickstartLocalIngestPreservesCommittedSelectionAtRunnerBoundary` (`cmd_kickstart_selection_runner_test.go:187`) | T4+T3 | A | cmd golden+skip | measured |
| 21 | 119.8 | — (inferred light) | cmd | `TestPublicationWizardAndReportUseDatabaseReadiness` (`publication_readiness_test.go:22`) | T3 | S | wave-A entry only | inferred |
| 22 | 107.9 | — (inferred light) | ingest | `TestWritePathDurability` (`write_path_durability_test.go:87`) | T1/T2/T3 | A | tail (no fix this epoch) | inferred |
| 23 | 78.1 | FILL | cmd | `TestModelsSync_500_StaticFallback` (`cmd_models_test.go:328`) | T2 | S | prepared-statement reuse + wave-A entry | measured |
| 24 | 74.2 | — (inferred light) | cmd | `TestLegacyOpenCodeSQLiteMountedHarvestCreatesManagedIndexedAnalyticsState` (`opencode_legacy_sqlite_mount_test.go:221`) | T3 | A | tail (no fix this epoch) | inferred |
| 25 | 70.0 | FILL | ingest | `TestNativeCoverageMatrix` (`native_coverage_test.go:192`) | T1/T4/T2 | S | ingest golden+skip + wave-A entry | measured |
| 26 | 66.6 | — (inferred light) | ingest | `TestWritePathMirrorsInPagesOf256` (`write_path_columns_test.go:230`) | T1/T2/T6 | A | tail (no fix this epoch) | inferred |
| 27 | 66.3 | — (inferred light) | cmd | `TestOpenCodeSessionClockFixturesMountedHarvest` (`opencode_session_clock_mount_test.go:91`) | T3 | A | tail (no fix this epoch) | inferred |
| 28 | 64.2 | — (inferred light) | ingest | `TestControlRecordIngestExportAndPublication` (`control_record_ingest_test.go:67`) | T3 | A | tail (no fix this epoch) | inferred |
| 29 | 61.5 | — (inferred light) | api | `TestHelperGroupListingThroughRegisteredRoutes` (`helper_group_listing_test.go:176`) | T6 | A | packing proof point | inferred |
| 30 | 61.4 | — (inferred light) | cmd | `TestKickstartRescan_FallsBackWithoutCompatibleDatabase` (`cmd_kickstart_rescan_test.go:491`) | T3 | S | wave-A entry only (subject is the missing-DB fallback; never converted) | inferred |

Elig: S = no goroutines on the exercised production path (admits wave A);
A = production concurrency exercised but not the subject (keeps the detector
tax). Rows 0–20 plus 23/25 are measured; the tail is structural (code read +
carried walls + existing profiles).

### Zero-wall screen (no promotion)

FILL — screening table for the five plausible-heaviest zero-wall parents.

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
FILL-rollup-output
```

Reconciliation: 190.97 + 37.59 + 23.21 + 36.45 + 0.03 + 0.00 + 5.23 = 293.48 s
= 93.71 % of 313.19 s sampled. Detector boundary + adjacency = 72.98 % of
sampled CPU. `sqlitemigration.Migrate` 167.14 s / 53.37 % cum; `store.Open`
186.65 s / 59.60 % cum; `(*Conn).PrepareTransient` 57.41 s / 18.33 % cum vs
connection-cached `sqlitex.Execute` 1.70 s / 0.54 % cum. `encoding/json` and
the redaction engine: zero rows in the top 300.

### Per-test rollup (measured set)

FILL — pasted `rollup.sh` output over the 23 per-test `-top` texts.

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
