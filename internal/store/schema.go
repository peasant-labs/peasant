// Package store provides the data access layer for Peasant.
// Store manages SQLite database connections, schema migrations,
// and query execution for analytics and metric retrieval.
package store

import "fmt"

// DDL constants for all 6 tables in migration v1.
// Schema is BCNF-normalized — see proposal Section 3.1/3.2.
const (
	// createProjects defines the projects dimension table.
	// V23+: project_hash is HMAC-SHA256(salt, canonical_remote) — opaque 64-char hex.
	// canonical_cwd: shortest observed filesystem path for this project.
	// canonical_remote: normalized git remote URL (e.g. github.com/user/repo).
	createProjects = `CREATE TABLE projects (
    project_hash     TEXT PRIMARY KEY,
    canonical_cwd    TEXT,
    canonical_remote TEXT
) STRICT`

	// createHostSlugs defines the host_slugs dimension table.
	// V23+: opaque_id is HMAC-SHA256(salt, canonical_remote) — 64-char hex PK.
	// host_slug is retained as a human-readable display/path column (NOT NULL).
	createHostSlugs = `CREATE TABLE host_slugs (
    opaque_id        TEXT PRIMARY KEY,
    host_slug        TEXT NOT NULL,
    git_remote       TEXT,
    canonical_remote TEXT
) STRICT`

	// createSessions defines the sessions fact table.
	// V23+: opaque_host_id replaces host_slug as FK (references host_slugs.opaque_id).
	// Columns pushed_at, tags, index_version, indexed_at are added by migrations V4, V9, V10.
	// Do NOT add those columns here — the incremental ALTER TABLE ADD COLUMN migrations must run.
	createSessions = `CREATE TABLE sessions (
    session_id     TEXT PRIMARY KEY,
    parent_id      TEXT REFERENCES sessions(session_id),
    model_harness  TEXT NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike')),
    model_id       TEXT NOT NULL,
    opaque_host_id TEXT NOT NULL REFERENCES host_slugs(opaque_id),
    project_hash   TEXT NOT NULL REFERENCES projects(project_hash),
    start_ms       INTEGER NOT NULL,
    end_ms         INTEGER NOT NULL,
    ingested_ms    INTEGER NOT NULL,
    source_path    TEXT NOT NULL,
    source_format  TEXT NOT NULL CHECK (source_format IN ('jsonl','json')),
    schema_version INTEGER NOT NULL DEFAULT 1,
    git_branch     TEXT,
    git_worktree   TEXT,
    git_tracking   TEXT,
    tool_version   TEXT
) STRICT`

	// createSessionsIndexes defines indexes for the sessions table.
	createSessionsIndexes = `CREATE INDEX idx_sessions_start    ON sessions(start_ms);
CREATE INDEX idx_sessions_harness  ON sessions(model_harness);
CREATE INDEX idx_sessions_project  ON sessions(project_hash);
CREATE INDEX idx_sessions_host     ON sessions(opaque_host_id);
CREATE INDEX idx_sessions_parent   ON sessions(parent_id) WHERE parent_id IS NOT NULL`

	// createSessionMetrics defines the session_metrics table.
	// FD: session_id → all metrics  (session_id is PK)
	createSessionMetrics = `CREATE TABLE session_metrics (
    session_id      TEXT PRIMARY KEY REFERENCES sessions(session_id),
    turn_count      INTEGER NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    subagent_count  INTEGER NOT NULL DEFAULT 0,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL GENERATED ALWAYS AS (tokens_in + tokens_out) STORED
) STRICT`

	// createDailySummary defines the daily_summary aggregate table.
	// FD: date_utc → all aggregates  (date_utc is PK)
	createDailySummary = `CREATE TABLE daily_summary (
    date_utc        TEXT PRIMARY KEY,
    session_count   INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL    NOT NULL DEFAULT 0,
    avg_turns       REAL    NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0
) STRICT`

	// createDailySummaryHarness defines the per-model_harness daily aggregates.
	// FD: (date_utc, model_harness) → all aggregates  (composite PK)
	createDailySummaryHarness = `CREATE TABLE daily_summary_harness (
    date_utc        TEXT NOT NULL,
    model_harness   TEXT NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike')),
    session_count   INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL    NOT NULL DEFAULT 0,
    avg_turns       REAL    NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (date_utc, model_harness)
) STRICT, WITHOUT ROWID`

	// createSessionEntries defines the session_entries table for transcript indexing.
	// Composite PK (session_id, entry_index). STRICT mode enforced.
	// All optional fields use NULL (not empty string) to distinguish absent from empty.
	createSessionEntries = `CREATE TABLE session_entries (
    session_id      TEXT NOT NULL REFERENCES sessions(session_id),
    entry_index     INTEGER NOT NULL,
    provider        TEXT NOT NULL,
    entry_type      TEXT NOT NULL CHECK (entry_type IN ('text','tool_use','tool_result','thinking','system','error','result')),
    role            TEXT NOT NULL CHECK (role IN ('user','assistant','tool','system')),
    timestamp_ms    INTEGER,
    content_preview TEXT,
    tokens_in       INTEGER,
    tokens_out      INTEGER,
    has_tool_use    INTEGER NOT NULL DEFAULT 0 CHECK (has_tool_use IN (0,1)),
    tool_names_csv  TEXT,
    has_thinking    INTEGER NOT NULL DEFAULT 0 CHECK (has_thinking IN (0,1)),
    is_error        INTEGER NOT NULL DEFAULT 0 CHECK (is_error IN (0,1)),
    raw_byte_length INTEGER,
    tool_use_id     TEXT,
    entry_id        TEXT,
    parent_entry_id TEXT,
    extra           TEXT,
    PRIMARY KEY (session_id, entry_index)
) STRICT`

	// createSessionEntriesIndexes defines composite indexes for the session_entries table.
	// All include session_id as the leading column to enable efficient per-session lookups.
	createSessionEntriesIndexes = `CREATE INDEX idx_session_entries_role ON session_entries(session_id, role);
CREATE INDEX idx_session_entries_type ON session_entries(session_id, entry_type);
CREATE INDEX idx_session_entries_error ON session_entries(session_id, is_error) WHERE is_error = 1`

	// migrateSessionMetrics widens session_metrics from v1 to v2 schema:
	// - v1 token/tool/duration columns renamed to v2 names with unit conversion:
	//     tokens_in → input_tokens (direct copy)
	//     tokens_out → output_tokens (direct copy)
	//     tool_call_count → tool_calls (direct copy)
	//     duration_ms → duration_minutes (÷60000.0 unit conversion)
	// - tokens_total GENERATED column dropped (replaced by computed total_tokens in v2)
	// - Retained v1 columns: turn_count, subagent_count (no v2 equivalent)
	// - v2 QualitySession + M-series columns added (all nullable)
	// - computed_at + compute_version added
	// - Existing v1 rows preserved with compute_version=0
	//
	// SQLite does not support ALTER COLUMN to change constraints, so we
	// recreate session_metrics via ALTER RENAME → CREATE → INSERT → DROP.
	migrateSessionMetrics = `ALTER TABLE session_metrics RENAME TO _session_metrics_v1;

CREATE TABLE session_metrics (
    session_id       TEXT PRIMARY KEY REFERENCES sessions(session_id),
    -- Retained v1 columns (no v2 equivalent; independently useful)
    turn_count       INTEGER,
    subagent_count   INTEGER,
    -- v2 QualitySession columns
    title            TEXT,
    outcome          TEXT CHECK(outcome IN ('resolved','partial','failed')),
    total_tokens     INTEGER,
    input_tokens     INTEGER,
    output_tokens    INTEGER,
    tool_calls       INTEGER,
    files_touched    INTEGER,
    lines_changed    INTEGER,
    duration_minutes REAL,
    retry_loops      INTEGER,
    retry_tokens_wasted INTEGER,
    within_session_reverts INTEGER,
    signal_density   REAL,
    spec_quality_score REAL,
    exploration_ratio REAL,
    scope_breadth    INTEGER,
    discovery_turns  INTEGER,
    -- M-series (v2 — stubbed)
    m2_token_outcome_ratio    REAL,
    m3_unique_tool_count      INTEGER,
    m4_error_recovery_count   INTEGER,
    m4_consecutive_error_max  INTEGER,
    m5_context_utilization_pct REAL,
    m5_peak_context_tokens    INTEGER,
    m5_avg_message_tokens     INTEGER,
    m6_output_survival_pct    REAL,
    m6_lines_survived         INTEGER,
    m6_lines_total            INTEGER,
    m7_spec_word_count        INTEGER,
    m7_spec_has_examples      INTEGER NOT NULL DEFAULT 0,
    m7_spec_has_constraints   INTEGER NOT NULL DEFAULT 0,
    -- Metadata
    computed_at      INTEGER,
    compute_version  INTEGER NOT NULL DEFAULT 0
) STRICT;

INSERT INTO session_metrics (
    session_id, turn_count, subagent_count,
    input_tokens, output_tokens, tool_calls, duration_minutes,
    compute_version
) SELECT
    session_id, turn_count, subagent_count,
    tokens_in, tokens_out, tool_call_count,
    CAST(duration_ms AS REAL) / 60000.0,
    0
FROM _session_metrics_v1;

DROP TABLE _session_metrics_v1`

	// migrateSessionEntriesV4 adds full-depth indexing columns to session_entries.
	// depth=0 rows are message-level (v1 default), depth=1 rows are content parts.
	migrateSessionEntriesV4 = `ALTER TABLE session_entries ADD COLUMN depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE session_entries ADD COLUMN parent_index INTEGER;
ALTER TABLE session_entries ADD COLUMN tool_input TEXT;
ALTER TABLE session_entries ADD COLUMN tool_output TEXT;
CREATE INDEX idx_session_entries_depth ON session_entries(session_id, depth);
CREATE INDEX idx_session_entries_parent ON session_entries(session_id, parent_index) WHERE parent_index IS NOT NULL`

	// createSessionEntriesExt defines the session_entries_ext EAV table
	// for known keys promoted from the Extra JSON column.
	// Known keys: tokens_reasoning (int), cache_read (int), cache_write (int), model_id (text).
	createSessionEntriesExt = `CREATE TABLE session_entries_ext (
    session_id   TEXT NOT NULL,
    entry_index  INTEGER NOT NULL,
    key          TEXT NOT NULL,
    value_text   TEXT,
    value_int    INTEGER,
    value_real   REAL,
    PRIMARY KEY (session_id, entry_index, key),
    FOREIGN KEY (session_id, entry_index)
      REFERENCES session_entries(session_id, entry_index) ON DELETE CASCADE
) STRICT`

	// createSessionEntriesExtIndex creates an index on the key column
	// for efficient lookups by key type.
	createSessionEntriesExtIndex = `CREATE INDEX idx_entries_ext_key ON session_entries_ext(key)`

	// migrateExtraToExt migrates known keys from the extra JSON column
	// into session_entries_ext rows. Known keys are then removed from extra.
	migrateExtraToExt = `INSERT INTO session_entries_ext (session_id, entry_index, key, value_int)
SELECT session_id, entry_index, 'tokens_reasoning', json_extract(extra, '$.tokens_reasoning')
FROM session_entries
WHERE extra IS NOT NULL AND json_extract(extra, '$.tokens_reasoning') IS NOT NULL;

INSERT INTO session_entries_ext (session_id, entry_index, key, value_int)
SELECT session_id, entry_index, 'cache_read', json_extract(extra, '$.cache_read')
FROM session_entries
WHERE extra IS NOT NULL AND json_extract(extra, '$.cache_read') IS NOT NULL;

INSERT INTO session_entries_ext (session_id, entry_index, key, value_int)
SELECT session_id, entry_index, 'cache_write', json_extract(extra, '$.cache_write')
FROM session_entries
WHERE extra IS NOT NULL AND json_extract(extra, '$.cache_write') IS NOT NULL;

INSERT INTO session_entries_ext (session_id, entry_index, key, value_text)
SELECT session_id, entry_index, 'model_id', json_extract(extra, '$.model_id')
FROM session_entries
WHERE extra IS NOT NULL AND json_extract(extra, '$.model_id') IS NOT NULL;

UPDATE session_entries
SET extra = CASE
    WHEN json_remove(extra, '$.tokens_reasoning', '$.cache_read', '$.cache_write', '$.model_id') = '{}' THEN NULL
    ELSE json_remove(extra, '$.tokens_reasoning', '$.cache_read', '$.cache_write', '$.model_id')
END
WHERE extra IS NOT NULL`

	// createModels defines the models reference table for model enrichment data
	// from models.dev. Composite PK (model_id, provider_key) supports provider-scoped IDs.
	// No FK from session_entries or session_metrics — enrichment-only JOIN.
	createModels = `CREATE TABLE models (
    model_id                 TEXT NOT NULL,
    provider_key             TEXT NOT NULL,
    display_name             TEXT NOT NULL,
    family                   TEXT,
    context_window           INTEGER,
    max_output               INTEGER,
    reasoning                INTEGER NOT NULL DEFAULT 0,
    tool_call                INTEGER NOT NULL DEFAULT 0,
    cost_input_per_mtok      REAL,
    cost_output_per_mtok     REAL,
    cost_reasoning_per_mtok  REAL,
    cost_cache_read_per_mtok REAL,
    cost_cache_write_per_mtok REAL,
    release_date             TEXT,
    last_synced              TEXT NOT NULL,
    PRIMARY KEY (model_id, provider_key)
) STRICT`

	// createModelsIndex creates indexes for the models table.
	createModelsIndex = `CREATE INDEX idx_models_family ON models(family) WHERE family IS NOT NULL`

	// addCostColumns adds cost analytics columns to session_metrics and daily_summary.
	// v3 migration: cost-per-session from models.dev pricing data.
	addCostColumns = `ALTER TABLE session_metrics ADD COLUMN cost_input_usd REAL;
ALTER TABLE session_metrics ADD COLUMN cost_output_usd REAL;
ALTER TABLE session_metrics ADD COLUMN cost_reasoning_usd REAL;
ALTER TABLE session_metrics ADD COLUMN cost_cache_read_usd REAL;
ALTER TABLE session_metrics ADD COLUMN cost_cache_write_usd REAL;
ALTER TABLE session_metrics ADD COLUMN cost_total_usd REAL;
ALTER TABLE session_metrics ADD COLUMN cost_model_id TEXT;
ALTER TABLE daily_summary ADD COLUMN total_cost_usd REAL;
ALTER TABLE daily_summary ADD COLUMN avg_cost_per_session_usd REAL`

	// addDailySummaryByProject creates the per-project daily summary table
	// and adds acceptance_rate to both daily_summary and daily_summary_by_project.
	addDailySummaryByProject = `CREATE TABLE daily_summary_by_project (
    date_utc       TEXT NOT NULL,
    project_hash   TEXT NOT NULL,
    project_path   TEXT,
    session_count  INTEGER NOT NULL DEFAULT 0,
    total_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tool_calls INTEGER NOT NULL DEFAULT 0,
    total_duration_minutes REAL NOT NULL DEFAULT 0,
    resolved_count INTEGER NOT NULL DEFAULT 0,
    failed_count   INTEGER NOT NULL DEFAULT 0,
    partial_count  INTEGER NOT NULL DEFAULT 0,
    avg_spec_quality REAL,
    acceptance_rate REAL,
    total_cost_usd REAL,
    PRIMARY KEY (date_utc, project_hash)
) STRICT;
CREATE INDEX idx_daily_project_hash ON daily_summary_by_project(project_hash);
ALTER TABLE daily_summary ADD COLUMN acceptance_rate REAL`

	// addTagsAndScope adds tags column to sessions and scope column to session_metrics.
	addTagsAndScope = `ALTER TABLE sessions ADD COLUMN tags TEXT;
ALTER TABLE session_metrics ADD COLUMN scope TEXT`

	// createIngestLog defines the ingest_log audit trail table.
	// AUTOINCREMENT PK ensures monotonic ordering for audit.
	// One row is written per peasant ingest run (at end of pipeline.Run()).
	createIngestLog = `CREATE TABLE IF NOT EXISTS ingest_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at        INTEGER NOT NULL,
    finished_at       INTEGER,
    sessions_new      INTEGER NOT NULL DEFAULT 0,
    sessions_updated  INTEGER NOT NULL DEFAULT 0,
    sessions_unchanged INTEGER NOT NULL DEFAULT 0,
    sessions_error    INTEGER NOT NULL DEFAULT 0,
    indexed_count     INTEGER NOT NULL DEFAULT 0,
    computed_count    INTEGER NOT NULL DEFAULT 0,
    error_message     TEXT,
    source_path       TEXT
) STRICT`

	// migrateAddPushedAt adds the pushed_at column to the sessions table.
	// NULL means the session has never been pushed to the village.
	// An INTEGER value is a Unix millisecond timestamp of the last push.
	migrateAddPushedAt = `ALTER TABLE sessions ADD COLUMN pushed_at INTEGER`

	// createPushLog defines the push_log audit trail table.
	// AUTOINCREMENT PK ensures monotonic ordering for audit.
	// One row is written per peasant push run (at end of push pipeline).
	createPushLog = `CREATE TABLE IF NOT EXISTS push_log (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at         INTEGER NOT NULL,
    finished_at        INTEGER,
    village_url    TEXT NOT NULL,
    sessions_pushed    INTEGER DEFAULT 0,
    sessions_updated   INTEGER DEFAULT 0,
    sessions_skipped   INTEGER DEFAULT 0,
    sessions_failed    INTEGER DEFAULT 0,
    error_message      TEXT,
    user_id            TEXT,
    username           TEXT
) STRICT`

	// migrateAddIndexTracking adds index_version and indexed_at columns to sessions
	// and creates the index_log audit trail table for index operations.
	// index_version tracks which indexer version was used (0 = never indexed).
	// indexed_at stores Unix milliseconds of the last successful indexing.
	// index_log follows the ingest_log/push_log pattern but has NO FK to ingest_log —
	// index_log entries are written during INDEX stage before ingest_log row exists.
	// Correlate via started_at timestamp overlap.
	migrateAddIndexTracking = `ALTER TABLE sessions ADD COLUMN index_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN indexed_at INTEGER`

	// createIndexLog defines the index_log audit trail table.
	// One row is written per session indexing attempt during the INDEX stage.
	createIndexLog = `CREATE TABLE IF NOT EXISTS index_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id        TEXT NOT NULL,
    provider          TEXT NOT NULL,
    outcome           TEXT NOT NULL CHECK (outcome IN ('indexed','reindexed','fallback','skipped','error')),
    index_version     INTEGER NOT NULL,
    entries_count     INTEGER NOT NULL DEFAULT 0,
    source_path       TEXT,
    original_root     TEXT,
    reason            TEXT,
    started_at        INTEGER NOT NULL,
    finished_at       INTEGER,
    error_message     TEXT
) STRICT`

	// createIndexLogIndexes defines indexes for the index_log table.
	createIndexLogIndexes = `CREATE INDEX idx_index_log_session ON index_log(session_id);
CREATE INDEX idx_index_log_outcome ON index_log(outcome)`

	// migrateSessionEntriesV11 renames tool_use_id to tool_call_id and adds
	// tool_kind and stop_reason columns to session_entries for ACP alignment.
	migrateSessionEntriesV11 = `ALTER TABLE session_entries RENAME COLUMN tool_use_id TO tool_call_id;
ALTER TABLE session_entries ADD COLUMN tool_kind TEXT;
ALTER TABLE session_entries ADD COLUMN stop_reason TEXT`

	// createSessionCommits defines the session_commits fact table.
	// Records git commits detected as produced during a session (EXTRACT+WRITE stage).
	// Composite PK (session_id, commit_hash) — one row per unique commit per session.
	// FK to sessions(session_id) ensures referential integrity.
	// All attribution fields (author_name, author_email, message) are nullable:
	// not all providers expose full commit metadata.
	// commit_time = committer timestamp (Unix ms); author_time = author timestamp (Unix ms).
	// author_time differs from commit_time only for rebased/cherry-picked commits.
	createSessionCommits = `CREATE TABLE session_commits (
    session_id   TEXT NOT NULL REFERENCES sessions(session_id),
    commit_hash  TEXT NOT NULL,
    author_name  TEXT,
    author_email TEXT,
    message      TEXT,
    commit_time  INTEGER,
    author_time  INTEGER,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch('now') * 1000),
    PRIMARY KEY (session_id, commit_hash)
) STRICT`

	// createSessionCommitsIndexes defines indexes for the session_commits table.
	// idx_session_commits_author: enables efficient per-author email lookups.
	// idx_session_commits_time: enables efficient time-range scans across all commits.
	createSessionCommitsIndexes = `CREATE INDEX idx_session_commits_author ON session_commits(author_email) WHERE author_email IS NOT NULL;
CREATE INDEX idx_session_commits_time ON session_commits(commit_time) WHERE commit_time IS NOT NULL`

	// ─── Migration V13: Annotation System ───────────────────────────────────────
	//
	// Standards basis:
	//   ISO/IEC 15408-1:2022 (Class > Family > Component taxonomy)
	//   ISO/IEC 11179:2023   (Metadata Registry, lifecycle, value domains)
	//   ISO/IEC 5394:2024    (Concept system principles)
	//   EvalevalAI           (lower_is_better, provenance, evaluator identity)
	//
	// BCNF status: all 6 tables are BCNF ✅.
	// STRICT mode enforced (consistent with V1-V12).
	// No unixepoch() — timestamps use CAST(strftime('%s','now') AS INTEGER)*1000.

	// createAnnotationClasses defines the top-level taxonomy grouping (ISO 15408 Class).
	// INTEGER PK (R2 consistent), text class UNIQUE for readability.
	// ~3 rows at MVP (quality, metadata, behavior), ~10 at maturity.
	createAnnotationClasses = `CREATE TABLE annotation_classes (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    class        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    description  TEXT
) STRICT`

	// createAnnotationFamilies defines the mid-level taxonomy grouping (ISO 15408 Family).
	// INTEGER PK (R2 consistent), text family UNIQUE for readability.
	// Each family belongs to exactly one class via class_id FK.
	// ~5 rows at MVP, ~25 at maturity.
	createAnnotationFamilies = `CREATE TABLE annotation_families (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    family       TEXT NOT NULL UNIQUE,
    class_id     INTEGER NOT NULL REFERENCES annotation_classes(id),
    display_name TEXT NOT NULL,
    description  TEXT
) STRICT`

	// createAnnotationFamiliesIndex creates an index for efficient class lookups.
	createAnnotationFamiliesIndex = `CREATE INDEX idx_annfam_class_id ON annotation_families(class_id)`

	// createAnnotationTypes defines the annotation type registry (ISO 15408 Component level).
	// INTEGER PK (R2) with text type_id UNIQUE.
	// Family FK derives class via join (BCNF: no redundant class column per R9).
	// type_id must contain at least one dot (family.name format).
	createAnnotationTypes = `CREATE TABLE annotation_types (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    type_id           TEXT NOT NULL UNIQUE,
    version           INTEGER NOT NULL DEFAULT 1,
    display_name      TEXT NOT NULL,
    description       TEXT,
    family_id         INTEGER NOT NULL REFERENCES annotation_families(id),
    value_domain_type TEXT NOT NULL
                      CHECK (value_domain_type IN ('enumerated', 'described')),
    datatype          TEXT NOT NULL
                      CHECK (datatype IN ('text', 'integer', 'real', 'boolean')),
    value_constraint  TEXT NOT NULL,
    lower_is_better   INTEGER CHECK (lower_is_better IN (0, 1)),
    status            TEXT NOT NULL DEFAULT 'proposed'
                      CHECK (status IN ('proposed', 'active', 'deprecated', 'retired')),
    origin            TEXT NOT NULL
                      CHECK (origin IN ('system', 'user', 'group')),
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER,
    deprecated_at     INTEGER,
    superseded_by     INTEGER REFERENCES annotation_types(id),
    CHECK (type_id LIKE '%.%')
) STRICT`

	// createAnnotationTypesIndexes creates indexes for annotation_types.
	createAnnotationTypesIndexes = `CREATE INDEX idx_anntype_status ON annotation_types(status);
CREATE INDEX idx_anntype_family_id ON annotation_types(family_id)`

	// createAnnotationTypeDeps defines the type-to-type dependency DAG (R8).
	// Cycle detection performed via recursive CTE at write time.
	// Self-dependency blocked by CHECK (annotation_type_id != depends_on_id).
	createAnnotationTypeDeps = `CREATE TABLE annotation_type_deps (
    annotation_type_id INTEGER NOT NULL REFERENCES annotation_types(id),
    depends_on_id      INTEGER NOT NULL REFERENCES annotation_types(id),
    required           INTEGER NOT NULL DEFAULT 1 CHECK (required IN (0, 1)),
    rationale          TEXT,
    PRIMARY KEY (annotation_type_id, depends_on_id),
    CHECK (annotation_type_id != depends_on_id)
) STRICT`

	// createAnnotationTypeDepsIndex creates an index for reverse dependency lookups.
	createAnnotationTypeDepsIndex = `CREATE INDEX idx_anntype_deps_reverse ON annotation_type_deps(depends_on_id)`

	// createAnnotators defines the annotator registry.
	// Who/what produces annotations. FK to models for agent annotators (composite FK).
	// Human annotator identity sourced from peasant login (not git email).
	// Priority derived from kind: human(3) > agent(2) > rule(1).
	// Exclusive arc CHECK: agent annotators must link to a model; others must not.
	createAnnotators = `CREATE TABLE annotators (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL CHECK (kind IN ('human', 'agent', 'rule')),
    name          TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL,
    description   TEXT,
    model_id      TEXT,
    provider_key  TEXT,
    status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'inactive')),
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (model_id, provider_key) REFERENCES models(model_id, provider_key),
    CHECK (kind != 'agent' OR (model_id IS NOT NULL AND provider_key IS NOT NULL)),
    CHECK (kind = 'agent' OR (model_id IS NULL AND provider_key IS NULL))
) STRICT`

	// createAnnotatorsIndexes creates indexes for the annotators table.
	createAnnotatorsIndexes = `CREATE INDEX idx_annotator_kind ON annotators(kind);
CREATE INDEX idx_annotator_model ON annotators(model_id, provider_key) WHERE model_id IS NOT NULL`

	// createAnnotations defines the annotation instances table (R1, R2, R3, R7).
	// No kind column (R3 — derived via VIEW annotations_with_target).
	// 3-arm exclusive arc: session, entry, or annotation target (R3 expansion from R7).
	// Retention: keep forever (R11). Supersession via superseded_by FK.
	createAnnotations = `CREATE TABLE annotations (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    target_session_id         TEXT REFERENCES sessions(session_id),
    target_entry_session_id   TEXT,
    target_entry_index        INTEGER,
    target_annotation_id      INTEGER REFERENCES annotations(id),
    annotator_id              INTEGER NOT NULL REFERENCES annotators(id),
    annotation_type_id        INTEGER NOT NULL REFERENCES annotation_types(id),
    value                     TEXT NOT NULL,
    confidence                REAL,
    reason                    TEXT,
    provenance                TEXT,
    created_at                INTEGER NOT NULL,
    updated_at                INTEGER,
    superseded_by             INTEGER REFERENCES annotations(id),
    FOREIGN KEY (target_entry_session_id, target_entry_index)
        REFERENCES session_entries(session_id, entry_index),
    CHECK (
        (CASE WHEN target_session_id IS NOT NULL THEN 1 ELSE 0 END +
         CASE WHEN target_entry_session_id IS NOT NULL THEN 1 ELSE 0 END +
         CASE WHEN target_annotation_id IS NOT NULL THEN 1 ELSE 0 END) = 1
    ),
    CHECK (
        (target_entry_session_id IS NULL) = (target_entry_index IS NULL)
    )
) STRICT`

	// createAnnotationsIndexes creates 5 indexes for the annotations table.
	// idx_ann_effective: covers priority-resolution query pattern (effective annotation).
	createAnnotationsIndexes = `CREATE INDEX idx_ann_session ON annotations(target_session_id) WHERE target_session_id IS NOT NULL;
CREATE INDEX idx_ann_entry ON annotations(target_entry_session_id, target_entry_index) WHERE target_entry_session_id IS NOT NULL;
CREATE INDEX idx_ann_meta ON annotations(target_annotation_id) WHERE target_annotation_id IS NOT NULL;
CREATE INDEX idx_ann_type_id ON annotations(annotation_type_id);
CREATE INDEX idx_ann_effective ON annotations(target_session_id, annotation_type_id, annotator_id, created_at DESC) WHERE target_session_id IS NOT NULL AND superseded_by IS NULL`

	// createAnnotationsView defines the annotations_with_target VIEW (R3).
	// Derives target_kind, annotator_kind, and taxonomy class from joins.
	// No kind column on annotations table — BCNF pure.
	createAnnotationsView = `CREATE VIEW annotations_with_target AS
SELECT
    a.id,
    a.target_session_id,
    a.target_entry_session_id,
    a.target_entry_index,
    a.target_annotation_id,
    CASE
        WHEN a.target_session_id IS NOT NULL THEN 'session'
        WHEN a.target_entry_session_id IS NOT NULL THEN 'entry'
        WHEN a.target_annotation_id IS NOT NULL THEN 'annotation'
    END AS target_kind,
    a.annotator_id,
    ann.kind AS annotator_kind,
    ann.name AS annotator_name,
    ann.display_name AS annotator_display_name,
    a.annotation_type_id,
    t.type_id,
    t.display_name AS type_name,
    f.family,
    c.class,
    a.value,
    a.confidence,
    a.reason,
    a.provenance,
    a.created_at,
    a.updated_at,
    a.superseded_by
FROM annotations a
JOIN annotators ann ON ann.id = a.annotator_id
JOIN annotation_types t ON t.id = a.annotation_type_id
JOIN annotation_families f ON f.id = t.family_id
JOIN annotation_classes c ON c.id = f.class_id`

	// seedAnnotationClasses seeds 3 taxonomy classes at MVP.
	seedAnnotationClasses = `INSERT INTO annotation_classes (id, class, display_name, description) VALUES
    (1, 'quality',  'Quality',  'Assessments of session or turn quality and outcomes'),
    (2, 'metadata', 'Metadata', 'Descriptive classifications of session properties'),
    (3, 'behavior', 'Behavior', 'Session behavior pattern classifications (post-MVP)')`

	// seedAnnotationFamilies seeds 5 taxonomy families at MVP.
	// FK-valid: all class_id values reference seeded annotation_classes rows.
	seedAnnotationFamilies = `INSERT INTO annotation_families (id, family, class_id, display_name, description) VALUES
    (1, 'session_quality',  1, 'Session Quality',  'Session-level quality assessments'),
    (2, 'turn_quality',     1, 'Turn Quality',     'Turn-level quality assessments'),
    (3, 'session_metadata', 2, 'Session Metadata', 'Session-level descriptive properties'),
    (4, 'turn_metadata',    2, 'Turn Metadata',    'Turn-level descriptive properties'),
    (5, 'session_behavior', 3, 'Session Behavior', 'Session-level behavior patterns')`

	// seedAnnotationTypes seeds 4 system annotation types at MVP.
	// FK-valid: all family_id values reference seeded annotation_families rows.
	// Note: quality.session_outcome has 4 values (includes "abandoned") vs the
	// legacy session_metrics.outcome CHECK (3 values). This is intentional.
	seedAnnotationTypes = `INSERT INTO annotation_types
    (type_id, version, display_name, description, family_id,
     value_domain_type, datatype, value_constraint, lower_is_better,
     status, origin, created_at)
VALUES
    ('quality.session_approval', 1, 'Session Approval',
     'Binary approval or denial of overall session quality. '
     || 'Used for RLHF-style preference signals on AI agent sessions.',
     1,
     'enumerated', 'text', '["approve","deny"]', NULL,
     'active', 'system', CAST(strftime('%s', 'now') AS INTEGER) * 1000),

    ('quality.session_outcome', 1, 'Session Outcome',
     'Categorical classification of how the session concluded. '
     || 'Resolved = task completed; partial = progress but unfinished; '
     || 'failed = approach did not work; abandoned = user stopped.',
     1,
     'enumerated', 'text', '["resolved","partial","failed","abandoned"]', NULL,
     'active', 'system', CAST(strftime('%s', 'now') AS INTEGER) * 1000),

    ('quality.user_frustration', 1, 'User Frustration',
     'Detection of user frustration signals in session interaction patterns. '
     || 'Detected = frustration indicators present; not_detected = normal interaction.',
     1,
     'enumerated', 'text', '["detected","not_detected"]', NULL,
     'active', 'system', CAST(strftime('%s', 'now') AS INTEGER) * 1000),

    ('metadata.session_scope', 1, 'Session Scope',
     'Classification of the type of work performed in the session.',
     3,
     'enumerated', 'text', '["feature","bug","refactor","docs","config","unknown"]', NULL,
     'active', 'system', CAST(strftime('%s', 'now') AS INTEGER) * 1000)`

	// seedAnnotators seeds 3 system rule-based annotators at MVP.
	seedAnnotators = `INSERT INTO annotators (kind, name, display_name, description, status, created_at) VALUES
    ('rule', 'outcome-classifier', 'Outcome Classifier',
     'Rule-based classifier that determines session outcome from metrics and turn patterns.',
     'active', CAST(strftime('%s', 'now') AS INTEGER) * 1000),
    ('rule', 'frustration-classifier', 'Frustration Classifier',
     'Rule-based classifier that detects user frustration signals from interaction patterns.',
     'active', CAST(strftime('%s', 'now') AS INTEGER) * 1000),
    ('rule', 'scope-classifier', 'Scope Classifier',
     'Rule-based classifier that determines session scope from tool usage and content patterns.',
     'active', CAST(strftime('%s', 'now') AS INTEGER) * 1000)`

	// seedAnnotationTypeDeps seeds 1 dependency at MVP:
	// quality.session_outcome optionally depends on metadata.session_scope.
	seedAnnotationTypeDeps = `INSERT INTO annotation_type_deps (annotation_type_id, depends_on_id, required, rationale)
SELECT
    (SELECT id FROM annotation_types WHERE type_id = 'quality.session_outcome'),
    (SELECT id FROM annotation_types WHERE type_id = 'metadata.session_scope'),
    0,
    'Scope classification provides context for outcome assessment but is not required'`
)

const (
	// ─── Migration V14: Annotation system extensions (F1 + F4 + F5) ─────────────
	//
	// F1: Project-level annotations — adds target_project_hash (4th arm) and
	//     updates the exclusive arc CHECK from 3-arm to 4-arm.
	//     SQLite cannot modify CHECK constraints in place, so we use the
	//     table rebuild pattern: rename → create new → copy data → drop old.
	//
	// F4: Multi-annotation tag — adds is_primary INTEGER NOT NULL DEFAULT 0.
	//
	// F5: Per-type configurable priority — adds priority_override INTEGER (nullable)
	//     to annotation_types. Simple ALTER TABLE, no rebuild needed.
	//
	// After rebuilding the table, all 5 original indexes are recreated plus
	// the new idx_ann_project partial index.  The annotations_with_target view
	// is also dropped and recreated to expose target_project_hash, the 4th CASE
	// WHEN arm in target_kind, and is_primary.

	// migrateAnnotationsV14 rebuilds the annotations table to add the 4th arm
	// (target_project_hash) and the is_primary flag (F4).
	// Data from V13 is preserved: target_project_hash = NULL, is_primary = 0.
	//
	// IMPORTANT: DROP VIEW must happen BEFORE renaming the table.
	// SQLite does NOT cascade-drop views on table rename; the view definition
	// continues to reference "annotations" by name and becomes temporarily stale.
	// Dropping it first prevents any validation errors on the rename.
	migrateAnnotationsV14 = `DROP VIEW IF EXISTS annotations_with_target;

ALTER TABLE annotations RENAME TO _annotations_v13;

CREATE TABLE annotations (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    target_session_id         TEXT REFERENCES sessions(session_id),
    target_entry_session_id   TEXT,
    target_entry_index        INTEGER,
    target_annotation_id      INTEGER REFERENCES annotations(id),
    target_project_hash       TEXT REFERENCES projects(project_hash),
    annotator_id              INTEGER NOT NULL REFERENCES annotators(id),
    annotation_type_id        INTEGER NOT NULL REFERENCES annotation_types(id),
    value                     TEXT NOT NULL,
    confidence                REAL,
    reason                    TEXT,
    provenance                TEXT,
    is_primary                INTEGER NOT NULL DEFAULT 0,
    created_at                INTEGER NOT NULL,
    updated_at                INTEGER,
    superseded_by             INTEGER REFERENCES annotations(id),
    FOREIGN KEY (target_entry_session_id, target_entry_index)
        REFERENCES session_entries(session_id, entry_index),
    CHECK (
        (CASE WHEN target_session_id IS NOT NULL THEN 1 ELSE 0 END +
         CASE WHEN target_entry_session_id IS NOT NULL THEN 1 ELSE 0 END +
         CASE WHEN target_annotation_id IS NOT NULL THEN 1 ELSE 0 END +
         CASE WHEN target_project_hash IS NOT NULL THEN 1 ELSE 0 END) = 1
    ),
    CHECK ((target_entry_session_id IS NULL) = (target_entry_index IS NULL)),
    CHECK (is_primary IN (0, 1))
) STRICT;

INSERT INTO annotations
    (id, target_session_id, target_entry_session_id, target_entry_index,
     target_annotation_id, target_project_hash, annotator_id, annotation_type_id,
     value, confidence, reason, provenance, is_primary,
     created_at, updated_at, superseded_by)
SELECT
    id, target_session_id, target_entry_session_id, target_entry_index,
    target_annotation_id, NULL, annotator_id, annotation_type_id,
    value, confidence, reason, provenance, 0,
    created_at, updated_at, superseded_by
FROM _annotations_v13;

DROP TABLE _annotations_v13`

	// recreateAnnotationsV14Indexes recreates all 6 annotation indexes after the
	// table rebuild.  idx_ann_project is new; the other 5 match V13.
	recreateAnnotationsV14Indexes = `CREATE INDEX idx_ann_session ON annotations(target_session_id) WHERE target_session_id IS NOT NULL;
CREATE INDEX idx_ann_entry ON annotations(target_entry_session_id, target_entry_index) WHERE target_entry_session_id IS NOT NULL;
CREATE INDEX idx_ann_meta ON annotations(target_annotation_id) WHERE target_annotation_id IS NOT NULL;
CREATE INDEX idx_ann_project ON annotations(target_project_hash) WHERE target_project_hash IS NOT NULL;
CREATE INDEX idx_ann_type_id ON annotations(annotation_type_id);
CREATE INDEX idx_ann_effective ON annotations(target_session_id, annotation_type_id, annotator_id, created_at DESC) WHERE target_session_id IS NOT NULL AND superseded_by IS NULL`

	// recreateAnnotationsV14View drops the V13 view and recreates it with:
	//   - target_project_hash column
	//   - 4th CASE WHEN arm in target_kind ('project')
	//   - is_primary column
	recreateAnnotationsV14View = `DROP VIEW IF EXISTS annotations_with_target;
CREATE VIEW annotations_with_target AS
SELECT
    a.id,
    a.target_session_id,
    a.target_entry_session_id,
    a.target_entry_index,
    a.target_annotation_id,
    a.target_project_hash,
    CASE
        WHEN a.target_session_id IS NOT NULL THEN 'session'
        WHEN a.target_entry_session_id IS NOT NULL THEN 'entry'
        WHEN a.target_annotation_id IS NOT NULL THEN 'annotation'
        WHEN a.target_project_hash IS NOT NULL THEN 'project'
    END AS target_kind,
    a.annotator_id,
    ann.kind AS annotator_kind,
    ann.name AS annotator_name,
    ann.display_name AS annotator_display_name,
    a.annotation_type_id,
    t.type_id,
    t.display_name AS type_name,
    f.family,
    c.class,
    a.value,
    a.confidence,
    a.reason,
    a.provenance,
    a.is_primary,
    a.created_at,
    a.updated_at,
    a.superseded_by
FROM annotations a
JOIN annotators ann ON ann.id = a.annotator_id
JOIN annotation_types t ON t.id = a.annotation_type_id
JOIN annotation_families f ON f.id = t.family_id
JOIN annotation_classes c ON c.id = f.class_id`

	// migrateAnnotationTypesV14 adds priority_override to annotation_types.
	// Simple ALTER TABLE (no rebuild needed — adding a nullable column).
	// NULL means use the default kind-based priority (human=3, agent=2, rule=1).
	migrateAnnotationTypesV14 = `ALTER TABLE annotation_types ADD COLUMN priority_override INTEGER`
)

const (
	// ─── Migration V15: Seed human-web annotator ─────────────────────────────────
	//
	// The web dashboard POST /api/v1/annotations endpoint resolves annotator by
	// name. V13 only seeded rule-based annotators. This migration adds a human
	// annotator ("human-web") so the frontend can create human annotations via
	// the REST API without requiring a separate annotator creation step.

	seedHumanWebAnnotator = `INSERT INTO annotators (kind, name, display_name, description, status, created_at) VALUES
    ('human', 'human-web', 'Web Dashboard User',
     'Human annotations created through the Peasant web dashboard.',
     'active', CAST(strftime('%s', 'now') AS INTEGER) * 1000)`
)

const (
	// ─── Migration V17: ScaleKind lookup table ───────────────────────────────────
	//
	// Adds the scale_kinds lookup table (Stevens 1946 measurement levels) and a
	// nullable scale_kind_id FK column to annotation_types.
	//
	// scale_kinds rows: 1=nominal, 2=ordinal, 3=continuous.
	//
	// The 4 seed annotation types from V13 (preserved through V16 UUID refactor)
	// are assigned their semantic scale kinds:
	//   - quality.session_outcome   → ordinal  (ordered: abandoned<failed<partial<resolved)
	//   - quality.user_frustration  → nominal  (binary categories without order)
	//   - metadata.session_scope    → nominal  (unordered categories: feature/bug/etc.)
	//   - quality.session_approval  → nominal  (binary categories: approve/deny)
	//
	// scale_kind_id is nullable to allow user-defined annotation types to omit it
	// until explicitly classified.

	// createScaleKinds defines the scale_kinds lookup table.
	createScaleKinds = `CREATE TABLE scale_kinds (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
) STRICT`

	// seedScaleKinds inserts the 3 Stevens measurement level rows.
	seedScaleKinds = `INSERT INTO scale_kinds (id, name) VALUES
    (1, 'nominal'),
    (2, 'ordinal'),
    (3, 'continuous')`

	// addScaleKindIDToAnnotationTypes adds the nullable FK column.
	addScaleKindIDToAnnotationTypes = `ALTER TABLE annotation_types ADD COLUMN scale_kind_id INTEGER REFERENCES scale_kinds(id)`

	// updateSeedAnnotationTypeScaleKinds assigns scale kinds to the 4 seed types.
	// Uses subquery lookup so it is robust to UUID-based PKs from V16.
	updateSeedAnnotationTypeScaleKinds = `UPDATE annotation_types SET scale_kind_id = (SELECT id FROM scale_kinds WHERE name = 'ordinal')
WHERE type_id = 'quality.session_outcome';
UPDATE annotation_types SET scale_kind_id = (SELECT id FROM scale_kinds WHERE name = 'nominal')
WHERE type_id = 'quality.user_frustration';
UPDATE annotation_types SET scale_kind_id = (SELECT id FROM scale_kinds WHERE name = 'nominal')
WHERE type_id = 'metadata.session_scope';
UPDATE annotation_types SET scale_kind_id = (SELECT id FROM scale_kinds WHERE name = 'nominal')
WHERE type_id = 'quality.session_approval'`
)

// ─── Migration V18: Entry-level annotation types + classifiers ──────────────
//
// Seeds 2 new annotation types for entry-level classification:
//   - quality.frustration_signal   (enumerated: "detected"/"not_detected")
//   - quality.resolution_evidence  (enumerated: "present"/"absent")
//
// Seeds 2 new rule-based annotators:
//   - frustration-signal-classifier
//   - resolution-evidence-classifier
//
// Registers allowed target kinds: both types target entries (target_kind 2).
// Assigns scale_kind: both are nominal (unordered categories).
//
// These types use the turn_quality family (family_id 2) since they target
// individual turns/entries rather than whole sessions.
//
// Uses fmt.Sprintf in buildMigrationV18() to embed deterministic UUIDs.

// buildMigrationV18 constructs the V18 SQL with embedded deterministic UUIDs.
func buildMigrationV18() string {
	return fmt.Sprintf(v18SeedEntryTypes,
		uuidTypeFrustrationSignal, uuidFamTurnQuality,
		uuidTypeResolutionEvidence, uuidFamTurnQuality,
		uuidAnnotatorFrustrationSignal,
		uuidAnnotatorResolutionEvidence,
		uuidTypeFrustrationSignal,
		uuidTypeResolutionEvidence,
	)
}

// v18SeedEntryTypes: 8 format args:
//
//	%[1]s = frustration_signal type UUID
//	%[2]s = turn_quality family UUID (for frustration_signal)
//	%[3]s = resolution_evidence type UUID
//	%[4]s = turn_quality family UUID (for resolution_evidence)
//	%[5]s = frustration-signal-classifier annotator UUID
//	%[6]s = resolution-evidence-classifier annotator UUID
//	%[7]s = frustration_signal type UUID (for allowed_target_kinds)
//	%[8]s = resolution_evidence type UUID (for allowed_target_kinds)
const v18SeedEntryTypes = `-- V18: Seed entry-level annotation types
INSERT OR IGNORE INTO annotation_types
    (id, type_id, version, display_name, description, family_id,
     value_domain_kind_id, datatype, value_constraint, lower_is_better,
     status_id, origin_id, scale_kind_id, created_at)
VALUES
    ('%[1]s', 'quality.frustration_signal', 1, 'Frustration Signal',
     'Entry-level detection of user frustration signals (expletive patterns). '
     || 'Marks individual turns where frustration was detected.',
     '%[2]s',
     (SELECT id FROM value_domain_kinds WHERE name = 'enumerated'),
     'text', '["detected","not_detected"]', NULL,
     (SELECT id FROM annotation_statuses WHERE name = 'active'),
     (SELECT id FROM type_origins WHERE name = 'system'),
     (SELECT id FROM scale_kinds WHERE name = 'nominal'),
     CAST(strftime('%%s', 'now') AS INTEGER) * 1000),

    ('%[3]s', 'quality.resolution_evidence', 1, 'Resolution Evidence',
     'Entry-level detection of resolution evidence phrases in assistant responses. '
     || 'Marks individual turns where resolution evidence was found.',
     '%[4]s',
     (SELECT id FROM value_domain_kinds WHERE name = 'enumerated'),
     'text', '["present","absent"]', NULL,
     (SELECT id FROM annotation_statuses WHERE name = 'active'),
     (SELECT id FROM type_origins WHERE name = 'system'),
     (SELECT id FROM scale_kinds WHERE name = 'nominal'),
     CAST(strftime('%%s', 'now') AS INTEGER) * 1000);

-- V18: Seed entry-level classifier annotators
INSERT OR IGNORE INTO annotators
    (id, kind_id, name, display_name, description, status, created_at)
VALUES
    ('%[5]s',
     (SELECT id FROM annotator_kinds WHERE name = 'rule'),
     'frustration-signal-classifier', 'Frustration Signal Classifier',
     'Rule-based classifier that detects frustration signals at individual entry level.',
     'active', CAST(strftime('%%s', 'now') AS INTEGER) * 1000),

    ('%[6]s',
     (SELECT id FROM annotator_kinds WHERE name = 'rule'),
     'resolution-evidence-classifier', 'Resolution Evidence Classifier',
     'Rule-based classifier that detects resolution evidence at individual entry level.',
     'active', CAST(strftime('%%s', 'now') AS INTEGER) * 1000);

-- V18: Register allowed target kinds (entry-level = target_kind 2)
INSERT OR IGNORE INTO annotation_type_target_kinds (annotation_type_id, target_kind_id) VALUES
    ('%[7]s', 2),
    ('%[8]s', 2)`

// --- Migration V20: Seed quality.annotation_approval meta-annotation type ---
//
// quality.annotation_approval is a meta-annotation (annotates another annotation).
// Used by the web UI to record human approval of auto-detected annotations.
// Target kind: annotation (id=3). Family: quality session outcomes (family 1).
// Value domain: enumerated {"approved","rejected","perceived"}.
//
// Uses fmt.Sprintf in buildMigrationV20() to embed the deterministic UUID.

// buildMigrationV20 constructs the V20 SQL with embedded deterministic UUID.
func buildMigrationV20() string {
	return fmt.Sprintf(v20SeedAnnotationApproval, uuidTypeAnnotationApproval, uuidFamSessionQuality)
}

// v20SeedAnnotationApproval: 2 format args:
//
//	%[1]s = annotation_approval type UUID
//	%[2]s = session_quality family UUID
const v20SeedAnnotationApproval = `-- V20: Seed quality.annotation_approval meta-annotation type
INSERT OR IGNORE INTO annotation_types
    (id, type_id, version, display_name, description, family_id,
     value_domain_kind_id, datatype, value_constraint, lower_is_better,
     status_id, origin_id, scale_kind_id, created_at)
VALUES
    ('%[1]s', 'quality.annotation_approval', 1, 'Annotation Approval',
     'Human approval or rejection of an auto-detected annotation. '
     || 'A meta-annotation: annotates another annotation.',
     '%[2]s',
     (SELECT id FROM value_domain_kinds WHERE name = 'enumerated'),
     'text', '["approved","rejected","perceived"]', NULL,
     (SELECT id FROM annotation_statuses WHERE name = 'active'),
     (SELECT id FROM type_origins WHERE name = 'system'),
     (SELECT id FROM scale_kinds WHERE name = 'nominal'),
     CAST(strftime('%%s', 'now') AS INTEGER) * 1000);

-- V20: Allow annotation-level targeting (target_kind 3) for annotation_approval
INSERT OR IGNORE INTO annotation_type_target_kinds (annotation_type_id, target_kind_id) VALUES
    ('%[1]s', 3)`

// ─── Migration V22: Codebase Familiarity Tables ──────────────────────────────
//
// Adds two tables for the codebase familiarity feature:
//   - session_files:    session-file junction — which files each session touched
//   - file_familiarity: project-level familiarity aggregation
const (
	// createSessionFiles defines the session-file junction table.
	// Tracks which files each session touched and how deeply.
	createSessionFiles = `CREATE TABLE IF NOT EXISTS session_files (
    session_id    TEXT NOT NULL REFERENCES sessions(session_id),
    file_path     TEXT NOT NULL,
    interaction   TEXT NOT NULL DEFAULT 'mentioned' CHECK (interaction IN ('mentioned','read','discussed','questioned')),
    turn_count    INTEGER NOT NULL DEFAULT 0,
    human_turns   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, file_path)
) STRICT`

	// createSessionFilesIndex creates an index for file path lookups across sessions.
	createSessionFilesIndex = `CREATE INDEX IF NOT EXISTS idx_session_files_path ON session_files(file_path)`

	// createFileFamiliarity defines the project-level familiarity aggregation table.
	createFileFamiliarity = `CREATE TABLE IF NOT EXISTS file_familiarity (
    project_hash       TEXT NOT NULL REFERENCES projects(project_hash),
    file_path          TEXT NOT NULL,
    familiarity_depth  INTEGER NOT NULL DEFAULT 0,
    session_count      INTEGER NOT NULL DEFAULT 0,
    total_turns        INTEGER NOT NULL DEFAULT 0,
    total_human_turns  INTEGER NOT NULL DEFAULT 0,
    first_engaged_at   TEXT,
    last_engaged_at    TEXT,
    PRIMARY KEY (project_hash, file_path)
) STRICT`
)

// ─── Migration V24: Session Commands Table ────────────────────────────────────
//
// Adds session_commands to record slash-command invocations found in transcripts.
// Each row links back to the session_entries row where the command appeared.
const (
	// createSessionCommands defines the session_commands table.
	// Composite PK (session_id, entry_index) mirrors session_entries PK.
	// FK ON DELETE CASCADE ensures rows are removed when the entry is pruned.
	createSessionCommands = `CREATE TABLE session_commands (
    session_id   TEXT NOT NULL,
    entry_index  INTEGER NOT NULL,
    command_name TEXT NOT NULL,
    command_args TEXT,
    PRIMARY KEY (session_id, entry_index),
    FOREIGN KEY (session_id, entry_index) REFERENCES session_entries(session_id, entry_index) ON DELETE CASCADE
) STRICT`

	// createSessionCommandsIndex creates an index for lookups by command name.
	createSessionCommandsIndex = `CREATE INDEX idx_session_commands_name ON session_commands(command_name)`
)

// AllTableNames lists all tables created across all migrations, in creation order.
// Used by tests and diagnostic queries.
var AllTableNames = []string{
	"projects",
	"host_slugs",
	"sessions",
	"session_metrics",
	"daily_summary",
	"daily_summary_harness",
	"session_entries",
	"session_entries_ext",
	"ingest_log",
	"push_log",
	"models",
	"daily_summary_by_project",
	"index_log",
	"session_commits",
	// V16: annotation tables (rebuilt with UUID PKs + INT lookup FKs)
	"target_kinds",
	"annotator_kinds",
	"annotation_statuses",
	"value_domain_kinds",
	"type_origins",
	"annotation_classes",
	"annotation_families",
	"annotation_types",
	"annotation_type_deps",
	"annotators",
	"annotations",
	"annotation_target_sessions",
	"annotation_target_entries",
	"annotation_target_annotations",
	"annotation_target_projects",
	"annotation_type_target_kinds",
	// V17: scale_kinds lookup table (Stevens measurement levels)
	"scale_kinds",
	// V21: pending_annotations table
	"pending_annotations",
	// V22: codebase familiarity tables
	"session_files",
	"file_familiarity",
	// V24: session commands table
	"session_commands",
}

// --- Migration V19: Seed annotation_type_deps for entry-level evidence classifiers ---
//
// Links entry-level classifiers to their session-level counterparts:
//   - session_outcome depends on resolution_evidence (required)
//   - user_frustration depends on frustration_signal (required)
//
// These dependencies formalize the relationship: entry-level annotations are
// evidence supporting the session-level classification. The annotation_type_deps
// table already exists from V13/V16; V19 only inserts new rows.
const seedEntryClassifierDeps = `INSERT INTO annotation_type_deps (annotation_type_id, depends_on_id, required, rationale)
VALUES
    ((SELECT id FROM annotation_types WHERE type_id = 'quality.session_outcome'),
     (SELECT id FROM annotation_types WHERE type_id = 'quality.resolution_evidence'),
     1,
     'Entry-level resolution evidence annotations support the session-level outcome classification'),
    ((SELECT id FROM annotation_types WHERE type_id = 'quality.user_frustration'),
     (SELECT id FROM annotation_types WHERE type_id = 'quality.frustration_signal'),
     1,
     'Entry-level frustration signal annotations support the session-level frustration classification')`
