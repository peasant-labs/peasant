package store

// migrationV70 makes the parent lane and a store-wide revision durable columns.
//
// sessions.parent_state is the store-only projection of the ONE parent rule,
// not a second resolver: 'root' means the evidence names no logical parent,
// 'resolved' means it names one (including a parent the store does not hold),
// and 'unresolved' means the session is a known spawned child whose parent
// cannot be named. resolved_parent_id is the logical parent scalar, and only
// when the state is 'resolved', so the two states cannot overlap, the column
// CHECK refuses the pair disagreeing, and the unresolved state does not fall
// back to the availability cache.
//
// The rule itself is LogicalParentSQL/ParentUnresolvedSQL: when the session's
// generation scope carries started_by or context_from evidence, the evidence
// alone answers (a snapshot or cache value cannot override it); otherwise the
// managed metadata ParentUUID snapshot answers, then the parent_id cache.
//
// sessions.parent_id stays the nullable FK availability cache it always was.
// This migration adds no write site: the triggers below are the ONLY writers of
// parent_state, resolved_parent_id, and store_state.revision. The four
// parent_id statements keep writing the cache; the AFTER UPDATE trigger fires
// after their row write, and the evidence triggers cover the evidence-only
// writes (insertHarmonizedGenerationOnConn and its callers) that never touch
// the cache. The AFTER DELETE trigger covers a pruned session that leaves no
// evidence row behind.
//
// store_state holds exactly one row and a monotonic revision. The page, the
// transcript read, and the caches compare it; it is the integer rendered as a
// decimal string on the wire.
//
// The trigger bodies are generated migration text. testdata/parent_state_triggers.golden.sql
// pins their exact SQL, and the seeded drift test proves the stored column
// matches the rule. A parent-rule change updates the golden in the same reviewed
// diff and, once V70 has shipped, lands as a NEW migration that redefines the
// triggers rather than editing an applied one.
//
// Re-entry: SQLite does not fire a trigger recursively by default. The
// migrations leave recursive_triggers off and must not enable it. The two
// sessions triggers never fire each other because the AFTER UPDATE trigger is
// scoped to the page-relevant input columns and the projection UPDATE names
// only parent_state and resolved_parent_id. The evidence triggers recompute the
// projection and bump the revision themselves; their UPDATE names only the two
// projection columns, so it cannot re-enter. One write bumps the revision once.
//
// The backfill runs before store_state and the triggers exist, so it neither
// bumps the revision nor depends on the trigger text, and a new database
// starts at revision 0.

// parentStateLogicalParentSQL is the projection's LogicalParentSQL expression
// for the session row an UPDATE is writing, correlated on the unaliased
// sessions table (a trigger body cannot name an UPDATE target alias). It is
// built from the live rule, not a second copy: the migration text, the
// parent_state column, and the Home reader's row markers all answer through
// LogicalParentSQL.
var parentStateLogicalParentSQL = LogicalParentSQL("sessions")

// parentStateUnresolvedSQL is the projection's ParentUnresolvedSQL expression
// for the same row: a started_by relationship in the session's generation scope
// whose target is a known spawned child with no nameable parent.
var parentStateUnresolvedSQL = ParentUnresolvedSQL("sessions")

// parentStateProjectionSQL is the frozen CASE that maps the parent rule to the
// one parent lane. ParentUnresolvedSQL wins over the cache fallback, so a known
// unnamed child is never a root and never a resolved child of a stale cache
// value.
var parentStateProjectionSQL = `CASE
         WHEN ` + parentStateUnresolvedSQL + ` THEN 'unresolved'
         WHEN ` + parentStateLogicalParentSQL + ` IS NOT NULL THEN 'resolved'
         ELSE 'root'
       END`

// parentStateResolvedParentSQL is the frozen resolved_parent_id scalar: the
// logical parent only while the state is 'resolved', NULL for both other
// states.
var parentStateResolvedParentSQL = `CASE
         WHEN ` + parentStateUnresolvedSQL + ` THEN NULL
         WHEN ` + parentStateLogicalParentSQL + ` IS NOT NULL THEN ` + parentStateLogicalParentSQL + `
         ELSE NULL
       END`

// parentStateProjectionUpdateSQL recomputes both projection columns. It names
// no WHERE clause so the backfill can rewrite every row; a trigger appends its
// own correlation, because a trigger body cannot alias its UPDATE target and
// must name the unaliased table.
var parentStateProjectionUpdateSQL = `UPDATE sessions
   SET parent_state = ` + parentStateProjectionSQL + `,
       resolved_parent_id = ` + parentStateResolvedParentSQL

// parentStateRevisionBumpSQL advances the one-row store revision and fails
// closed at the integer ceiling rather than silently freezing every cache.
const parentStateRevisionBumpSQL = `UPDATE store_state SET revision = revision + 1 WHERE id = 1 AND revision < 9223372036854775807;
  SELECT RAISE(ABORT, 'store: store_state.revision cannot advance past its integer ceiling; export the store and start a new one')
   WHERE changes() = 0;`

// parentStateSessionsTriggerFiresOn lists the sessions columns whose write can
// change a page. It is every writable column except the projection outputs
// (parent_state, resolved_parent_id) and the two publication-provenance columns
// the shipped publication trigger rewrites (indexed_publication_capture_revision,
// cwd_provenance_kind): including either set would re-enter this trigger from
// its own or from that trigger's UPDATE. A new page-relevant column must be
// added here by a migration, and TestMigrationV70SessionsUpdateTriggerCoversSchema
// fails until it is.
const parentStateSessionsTriggerFiresOn = `parent_id, model_harness, model_id, opaque_host_id,
  project_hash, start_ms, end_ms, ingested_ms, source_path, source_format,
  schema_version, git_branch, git_worktree, git_tracking, tool_version,
  pushed_at, tags, index_version, indexed_at, license_id, session_origin,
  origin_version, session_entries_hash, source_fingerprint, session_cwd,
  publication_capture_revision, index_format_version, adapter_version,
  artifact_hash, metric_seed_json, indexed_input_hash, active_generation_id,
  root_session_id, session_purpose, input_submission_count,
  content_sweep_pending, thinking_level, thinking_level_raw,
  thinking_level_history, latest_attempt_generation_id,
  latest_attempt_artifact_hash, latest_attempt_indexer_version,
  latest_attempt_failure_code, latest_attempt_failure_message,
  latest_attempt_captured_at_ms, latest_attempt_stats_json`

// migrationV70 adds the parent lane and the store revision, backfills the
// projection for every stored row, and installs the only writers of both.
var migrationV70 = `
ALTER TABLE sessions ADD COLUMN parent_state TEXT NOT NULL DEFAULT 'root'
  CHECK (parent_state IN ('root','resolved','unresolved'));
ALTER TABLE sessions ADD COLUMN resolved_parent_id TEXT
  CHECK ((parent_state = 'resolved') = (resolved_parent_id IS NOT NULL));

` + parentStateProjectionUpdateSQL + `;

CREATE TABLE store_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0)
) STRICT;
INSERT INTO store_state (id, revision) VALUES (1, 0);

CREATE INDEX idx_sessions_root_page
  ON sessions(start_ms DESC, session_id ASC)
  WHERE parent_state = 'root';
CREATE INDEX idx_sessions_child_page
  ON sessions(resolved_parent_id, start_ms DESC, session_id ASC)
  WHERE parent_state = 'resolved';

CREATE TRIGGER sessions_parent_state_ai AFTER INSERT ON sessions
BEGIN
  ` + parentStateProjectionUpdateSQL + `
 WHERE sessions.session_id = NEW.session_id;
  ` + parentStateRevisionBumpSQL + `
END;

CREATE TRIGGER sessions_parent_state_au
AFTER UPDATE OF ` + parentStateSessionsTriggerFiresOn + ` ON sessions
BEGIN
  ` + parentStateProjectionUpdateSQL + `
 WHERE sessions.session_id = NEW.session_id;
  ` + parentStateRevisionBumpSQL + `
END;

CREATE TRIGGER sessions_parent_state_ad AFTER DELETE ON sessions
BEGIN
  ` + parentStateRevisionBumpSQL + `
END;

CREATE TRIGGER session_relationship_evidence_parent_state_ai
AFTER INSERT ON session_relationship_evidence
BEGIN
  ` + parentStateProjectionUpdateSQL + `
 WHERE sessions.session_id = NEW.session_id;
  ` + parentStateRevisionBumpSQL + `
END;

CREATE TRIGGER session_relationship_evidence_parent_state_au
AFTER UPDATE ON session_relationship_evidence
BEGIN
  ` + parentStateProjectionUpdateSQL + `
 WHERE sessions.session_id = NEW.session_id;
  ` + parentStateRevisionBumpSQL + `
END;

CREATE TRIGGER session_relationship_evidence_parent_state_ad
AFTER DELETE ON session_relationship_evidence
BEGIN
  ` + parentStateProjectionUpdateSQL + `
 WHERE sessions.session_id = OLD.session_id;
  ` + parentStateRevisionBumpSQL + `
END;

CREATE TRIGGER session_publication_metadata_parent_state_ai
AFTER INSERT ON session_publication_metadata
BEGIN
  ` + parentStateProjectionUpdateSQL + `
 WHERE sessions.session_id = NEW.session_id;
  ` + parentStateRevisionBumpSQL + `
END;

CREATE TRIGGER session_publication_metadata_parent_state_au
AFTER UPDATE OF metadata_json ON session_publication_metadata
BEGIN
  ` + parentStateProjectionUpdateSQL + `
 WHERE sessions.session_id = NEW.session_id;
  ` + parentStateRevisionBumpSQL + `
END;

CREATE TRIGGER session_publication_metadata_parent_state_ad
AFTER DELETE ON session_publication_metadata
BEGIN
  ` + parentStateProjectionUpdateSQL + `
 WHERE sessions.session_id = OLD.session_id;
  ` + parentStateRevisionBumpSQL + `
END;
`
