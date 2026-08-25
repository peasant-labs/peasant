package store

import "zombiezen.com/go/sqlite/sqlitemigration"

// migrationV1 creates all 6 tables and indexes for the initial schema.
// Each CREATE TABLE statement is separated by semicolons.
var migrationV1 = createProjects + ";\n" +
	createHostSlugs + ";\n" +
	createSessions + ";\n" +
	createSessionsIndexes + ";\n" +
	createSessionMetrics + ";\n" +
	createDailySummary + ";\n" +
	createDailySummaryHarness

// migrationV2 adds the session_entries table and widens session_metrics to include v2 columns.
// Existing v1 data is preserved with compute_version=0.
//
// SQLite does not support ALTER COLUMN to change constraints, so we
// recreate session_metrics via ALTER RENAME → CREATE → INSERT → DROP.
var migrationV2 = createSessionEntries + ";\n" +
	createSessionEntriesIndexes + ";\n" +
	migrateSessionMetrics

// migrationV3 adds the ingest_log audit trail table.
// One row is written per peasant ingest run.
const migrationV3 = createIngestLog

// migrationV4 combines:
// - Current branch: adds pushed_at column to sessions + push_log table (village push)
// - WRT branch: adds full-depth indexing columns to session_entries (depth, parent_index, tool_input, tool_output)
var migrationV4 = migrateAddPushedAt + ";\n" +
	createPushLog + ";\n" +
	migrateSessionEntriesV4

// migrationV5 creates the session_entries_ext EAV table for known Extra keys
// and migrates existing known keys from the extra JSON column into ext rows.
var migrationV5 = createSessionEntriesExt + ";\n" +
	createSessionEntriesExtIndex + ";\n" +
	migrateExtraToExt

// migrationV6 creates the models reference table for model enrichment data.
var migrationV6 = createModels + ";\n" +
	createModelsIndex

// migrationV7 adds cost analytics columns to session_metrics and daily_summary.
const migrationV7 = addCostColumns

// migrationV8 creates the daily_summary_by_project table and adds acceptance_rate
// to daily_summary for S6v3 (per-project) and S8v3 (acceptance rate).
const migrationV8 = addDailySummaryByProject

// migrationV9 adds tags column to sessions and scope column to session_metrics
// for S7v3 (session tags + scope classification).
const migrationV9 = addTagsAndScope

// migrationV10 adds index_version/indexed_at tracking to sessions
// and creates the index_log audit trail table.
var migrationV10 = migrateAddIndexTracking + ";\n" +
	createIndexLog + ";\n" +
	createIndexLogIndexes

// migrationV11 renames tool_use_id → tool_call_id and adds tool_kind + stop_reason
// to session_entries for ACP alignment (Push v2).
const migrationV11 = migrateSessionEntriesV11

// migrationV12 creates the session_commits table for tracking git commits
// produced during each session. Composite PK (session_id, commit_hash) with
// FK to sessions(session_id). Indexes on author_email and commit_time.
var migrationV12 = createSessionCommits + ";\n" +
	createSessionCommitsIndexes

// migrationV13 creates the annotation system: 6 tables, 1 view, 11 indexes, and seed data.
//
// Tables: annotation_classes, annotation_families, annotation_types,
//
//	annotation_type_deps, annotators, annotations.
//
// View: annotations_with_target (derives target_kind, annotator_kind, class).
// Seed: 3 classes, 5 families, 4 system annotation types, 3 annotators, 1 dependency.
// Standards basis: ISO/IEC 15408-1:2022, ISO/IEC 11179:2023, ISO/IEC 5394:2024.
var migrationV13 = createAnnotationClasses + ";\n" +
	createAnnotationFamilies + ";\n" +
	createAnnotationFamiliesIndex + ";\n" +
	createAnnotationTypes + ";\n" +
	createAnnotationTypesIndexes + ";\n" +
	createAnnotationTypeDeps + ";\n" +
	createAnnotationTypeDepsIndex + ";\n" +
	createAnnotators + ";\n" +
	createAnnotatorsIndexes + ";\n" +
	createAnnotations + ";\n" +
	createAnnotationsIndexes + ";\n" +
	createAnnotationsView + ";\n" +
	seedAnnotationClasses + ";\n" +
	seedAnnotationFamilies + ";\n" +
	seedAnnotationTypes + ";\n" +
	seedAnnotators + ";\n" +
	seedAnnotationTypeDeps

// migrationV14 extends the annotation system (F1+F4+F5):
//   - F1: Adds target_project_hash TEXT (4th arm) + 4-arm exclusive arc CHECK
//   - F4: Adds is_primary INTEGER NOT NULL DEFAULT 0 to annotations
//   - F5: Adds priority_override INTEGER (nullable) to annotation_types
//
// The annotations table is rebuilt (SQLite table rebuild pattern) to update the
// CHECK constraint.  All 6 indexes and the annotations_with_target view are
// recreated.  annotation_types gets a simple ALTER TABLE for priority_override.
var migrationV14 = migrateAnnotationsV14 + ";\n" +
	recreateAnnotationsV14Indexes + ";\n" +
	recreateAnnotationsV14View + ";\n" +
	migrateAnnotationTypesV14

// migrationV15 seeds the "human-web" annotator so the web dashboard can create
// human annotations via POST /api/v1/annotations.
const migrationV15 = seedHumanWebAnnotator

// migrationV16 refactors the annotation schema:
// - 5 INT lookup tables for enums
// - UUID PKs for all annotation entity tables
// - TPT (Table-per-Type) child tables for annotation targets
// - AllowedTargetKinds junction table
// - Span targeting with half-open [start, end) on entry targets
// - Content hash column for push dedup
// Built by Go function to embed deterministic UUIDs.
var migrationV16 = buildMigrationV16()

// migrationV17 adds the scale_kinds lookup table (Stevens 1946 measurement levels)
// and a nullable scale_kind_id FK column to annotation_types.
// The 4 seed annotation types are assigned their semantic scale kinds.
var migrationV17 = createScaleKinds + ";\n" +
	seedScaleKinds + ";\n" +
	addScaleKindIDToAnnotationTypes + ";\n" +
	updateSeedAnnotationTypeScaleKinds

// migrationV18 seeds 2 entry-level annotation types (quality.frustration_signal,
// quality.resolution_evidence), 2 rule-based annotators, and allowed target kinds.
// Built by Go function to embed deterministic UUIDs.
var migrationV18 = buildMigrationV18()

// migrationV19 seeds annotation_type_deps linking entry-level evidence classifiers
// to their session-level counterparts:
//   - session_outcome depends on resolution_evidence (required)
//   - user_frustration depends on frustration_signal (required)
const migrationV19 = seedEntryClassifierDeps

// migrationV20 seeds the quality.annotation_approval meta-annotation type.
// This type annotates other annotations (target_kind=3) and records human
// approval/rejection of auto-detected entry-level annotations from the web UI.
var migrationV20 = buildMigrationV20()

// migrationV21 adds the pending_annotations table for TUI annotation drafts.
// Pending annotations are created locally in the TUI before being committed
// to the backend via HTTP POST. No FK on session_id (TUI may reference sessions
// not yet ingested into the local SQLite DB).
const migrationV21 = createPendingAnnotations

// migrationV22 creates the session_files and file_familiarity tables for the
// codebase familiarity feature. session_files is a session-file junction;
// file_familiarity aggregates per-project familiarity depth.
var migrationV22 = createSessionFiles + ";\n" +
	createSessionFilesIndex + ";\n" +
	createFileFamiliarity

// migrationV23 is the SQL-only phase of the V23 opaque-PK migration.
//
// This phase creates empty v2 tables (host_slugs_v2, projects_v2, sessions_v2,
// daily_summary_by_project_v2, file_familiarity_v2) with the new schema.
//
// The Go-driven phase (HMAC computation + data population + rename) is
// performed by applyV23DataMigration, called from Open() after this SQL runs.
// See schema_v23.go for the full migration design.
var migrationV23 = buildMigrationV23()

// migrationV24 adds the session_commands table for recording slash-command
// invocations detected in transcripts.
// Each row references the session_entries row (session_id, entry_index) where
// the command was found; ON DELETE CASCADE keeps the table clean on prune.
// An index on command_name supports efficient command-frequency queries.
var migrationV24 = createSessionCommands + ";\n" +
	createSessionCommandsIndex

// migrationV25 adds research annotation infrastructure:
// research class + episode_friction family + research.friction_episode type.
// Deprecates existing annotation types so only the research type is active.
// See schema_v25.go for the full migration SQL.

// migrationV27 updates the friction episode taxonomy from v1 (16 subtypes) to v2 (3 types).
// See schema_v27.go for details.

// migrationV28 creates the lessons table for agent memory construction.
// See schema_v28.go for details.

// migrationV29 drops FK on lessons.episode_annotation_id.
// See schema_v29.go for details.

// migrationV30 creates memory_injection_log for tracking inject on/off events.
// See schema_v30.go for details.

// migrationV31 deduplicates lessons and adds UNIQUE(topic, rule, failure_mode).
// See schema_v31.go for details.

// migrationV32 creates the lesson_sources provenance table.
// See schema_v32.go for details.

// migrationV33 updates model_harness CHECK constraints and data to use
// bestiary.Harness identifiers (claude→claude-code, gemini→gemini-cli).
// See schema_v33.go for details.

// migrationV34 creates the pulled_transcripts and pulled_annotations tables
// (a derived index of the on-disk village-pulls/ manifests) backing
// `peasant village transcripts pull`. See schema_v34.go for details.

// migrationV35 adds an external-content FTS5 virtual table over session_entries
// (session_entries_fts) plus its three sync triggers for full-text transcript
// search. See schema_v35.go for details.

// migrationV36 seeds the user.custom_label free-text entry annotation type so
// the per-turn label picker can save arbitrary strings (G8).
// See schema_v36.go for details.

// migrationV37 adds the sessions.license_id column (per-transcript license,
// mirrors the village licenses.id menu seeded in village migration 026).
// The publish/pull producer path is wired alongside this migration; the village
// INGEST half landed in village migration 026 (transcripts.license_id +
// governance audit).
// See schema_v37.go for details.

// migrationV38 adds the pulled_transcripts.license_id column — the PULL-side
// license mirror (see V37 for the push side). Refreshed on every re-pull
// (overwrite, mirroring server truth). See schema_v38.go for details.

// migrationV39 seeds 2 entry-level annotation types (quality.turn_outcome
// {good,neutral,bad}, quality.turn_flag {none,error,retry_loop,revert,highlight})
// backing the restored per-turn labeling modal. Data-only
// (INSERT OR IGNORE), adds no tables or indexes. See schema_v39.go for details.

// migrationV40 creates the durable session-to-observed-commit association
// ledger. Existing session_commits rows are backfilled with opaque IDs without
// rebuilding any table, preserving the V37/V38 license CHECK mirrors.

// migrationV41 adds the normalized association target arm for local
// annotations. The target stores only a durable association ID; the ledger
// remains the authority for the enclosing session and observed commit hash.

// migrationV42 widens the sessions and daily_summary_harness model_harness
// CHECK constraints to admit Strike. See schema_v42.go for details.

// migrationV46 adds sessions.session_origin (the three-value origin menu,
// default 'unknown') and sessions.origin_version (an unchecked watermark), and
// widens claude_transcript_evidence with an origin column whose CHECK also
// admits the empty string, marking a cache row mined before this field existed.
// Plain ALTER TABLE ADD COLUMN, no table rebuild. See schema_v46.go.

// dbSchema is the sqlitemigration schema applied on Open().
var dbSchema = sqlitemigration.Schema{
	Migrations: []string{
		migrationV1,
		migrationV2,
		migrationV3,
		migrationV4,
		migrationV5,
		migrationV6,
		migrationV7,
		migrationV8,
		migrationV9,
		migrationV10,
		migrationV11,
		migrationV12,
		migrationV13,
		migrationV14,
		migrationV15,
		migrationV16,
		migrationV17,
		migrationV18,
		migrationV19,
		migrationV20,
		migrationV21,
		migrationV22,
		migrationV23,
		migrationV24,
		migrationV25,
		migrationV26,
		migrationV27,
		migrationV28,
		migrationV29,
		migrationV30,
		migrationV31,
		migrationV32,
		migrationV33,
		migrationV34,
		migrationV35,
		migrationV36,
		migrationV37,
		migrationV38,
		migrationV39,
		migrationV40,
		migrationV41,
		migrationV42,
		migrationV43,
		migrationV44,
		migrationV45,
		migrationV46,
	},
	// V16 rebuilds annotation tables with new FKs; disable FK checking during
	// the migration transaction so renamed/recreated tables don't cause violations.
	// V23 creates v2 tables; data migration runs via applyV23DataMigration in Open().
	// DisableForeignKeys is needed for V23 because the v2 tables reference each other
	// and the rename/drop sequence requires FK checking to be off.
	MigrationOptions: []*sqlitemigration.MigrationOptions{
		nil, nil, nil, nil, nil, nil, nil, nil, // V1-V8
		nil, nil, nil, nil, nil, nil, nil, // V9-V15
		{DisableForeignKeys: true}, // V16
		nil,                        // V17
		nil,                        // V18
		nil,                        // V19
		nil,                        // V20
		nil,                        // V21
		nil,                        // V22
		{DisableForeignKeys: true}, // V23
		nil,                        // V24
		nil,                        // V25
		nil,                        // V26
		nil,                        // V27
		nil,                        // V28
		nil,                        // V29
		nil,                        // V30
		nil,                        // V31
		nil,                        // V32
		{DisableForeignKeys: true}, // V33: recreates sessions + daily_summary_harness
		nil,                        // V34: creates pulled_transcripts + pulled_annotations (no FKs)
		nil,                        // V35: FTS5 virtual table + sync triggers (no FK targets)
		nil,                        // V36: seed user.custom_label annotation type (no schema/FK change)
		nil,                        // V37: ALTER TABLE sessions ADD COLUMN license_id (plain column add, no FK)
		nil,                        // V38: ALTER TABLE pulled_transcripts ADD COLUMN license_id (plain column add, no FK)
		nil,                        // V39: seed quality.turn_outcome + quality.turn_flag annotation types (no schema/FK change)
		nil,                        // V40: durable session-commit association ledger (new table + backfill)
		nil,                        // V41: association annotation target table + view recreation
		{DisableForeignKeys: true}, // V42: recreates sessions + daily_summary_harness
		nil,                        // V43: publication receipts and attempt diagnostics
		nil,                        // V44: Claude discovery evidence cache (new table, no FKs)
		nil,                        // V45: OpenCode change cursor (new table, no FKs)
		nil,                        // V46: ALTER TABLE ADD COLUMN on sessions + claude_transcript_evidence (no FKs)
	},
}
