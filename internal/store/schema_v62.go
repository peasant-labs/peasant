package store

// BodyRowIDBase is the FTS rowid base for harmonized entry bodies:
// bodyRowIDBase = 1 << 50 = 1125899906842624. Every session_entries rowid
// stays below it and every session_entry_bodies body_id at or above it, so
// the two rowid spaces feeding session_search_source never meet. SQLite DDL
// cannot reference a Go constant, so migrationV62 spells the literal in the
// CHECK and in the allocation subquery; writers allocate explicitly with
// (SELECT coalesce(max(body_id), BodyRowIDBase-1) + 1 ...) and every use
// reads this constant. Harvest verify refuses when
// max(session_entries.rowid) reaches the base.
const BodyRowIDBase = 1 << 50

// migrationV62 adds the harmonized session content model: the typed entry
// entity, the digest-addressed content store, the immutable generation
// catalog with its ordered metadata children, the mutable per-session stats
// row, and the unified search store. It backfills the stats row and the
// sweep flag from the file-backed catalog, rebuilds the reshaped
// generation-keyed tables and the annotation targets, and drops the
// duplicate partition index. No entry content moves inside this migration:
// conversion runs later in peasant migrate.
//
// The migration is SQL-only and runs at open. Table rebuilds drop and rename
// (the v53 pattern), so this slot disables foreign keys in MigrationOptions.
// Data preservation rules, per rebuilt table:
//   - session_relationship_evidence loses its anchor JSON to three structured
//     columns (json_extract over the anchor object; NULL anchor stays NULL).
//   - session_context_segments loses captured_refs_json to ordered
//     session_context_segment_refs rows (json_each over the ref array; the
//     array index is the ordinal).
//   - session_projection_sections loses native_metadata to ordered
//     session_section_native_metadata rows (json_each over the record array;
//     scalar fields decode with json_extract, which the Go serializer
//     re-encodes byte-identically; the opaque data payload keeps its exact
//     bytes through the -> operator, so both raw embedding and re-marshal
//     round-trip).
//   - annotation_target_entries is rebuilt without its session_entries
//     foreign key; the insert-time existence check replaces the key.
// The v61 relationship-evidence target index is recreated after its table
// rebuild, and the annotations_with_target view is recreated in its v41 form.
//
// Backfills:
//   - session_captured_stats gains one row per session whose active
//     generation lives in session_projection_generations, read from that
//     generation's metadata_json (source 'harness'; seed_json is the $.stats
//     document; updated_at_ms is activated_at_ms falling back to
//     installed_at_ms; absent stats read NULL, which means unknown).
//     Stats documents are Go-marshaled SessionStats, so only the known keys
//     can occur; overflow stays NULL for future stat kinds to promote.
//   - sessions.content_sweep_pending is set for every session with a
//     non-active session_projection_generations row (NULL-safe: a NULL active
//     pointer counts every generation as non-active). Pending intents and
//     staged directories are files, so the flag does not claim them; peasant
//     migrate Phase 1 drains them.
// The new search index starts empty on purpose: bodies index through their
// triggers going forward, and search consolidation rebuilds over the union
// view once every session is converted.
const migrationV62 = `
CREATE TABLE session_entry_bodies (
  body_id          INTEGER PRIMARY KEY CHECK(body_id >= 1125899906842624),
  session_id       TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  body_digest      TEXT NOT NULL CHECK(length(body_digest) = 64),
  entry_index      INTEGER NOT NULL CHECK(entry_index >= 0),
  harness          TEXT NOT NULL,
  entry_type       TEXT NOT NULL,
  role             TEXT NOT NULL,
  timestamp_ms     INTEGER,
  content_preview  TEXT,
  tokens_in        INTEGER,
  tokens_out       INTEGER,
  has_tool_use     INTEGER NOT NULL CHECK(has_tool_use IN (0,1)),
  tool_kind        TEXT,
  tool_names_csv   TEXT,
  has_thinking     INTEGER NOT NULL CHECK(has_thinking IN (0,1)),
  is_error         INTEGER NOT NULL CHECK(is_error IN (0,1)),
  stop_reason      TEXT,
  raw_byte_length  INTEGER,
  tool_call_id     TEXT,
  entry_id         TEXT,
  parent_entry_id  TEXT,
  depth            INTEGER NOT NULL,
  parent_index     INTEGER,
  tool_input       TEXT,
  tool_output      TEXT,
  model_id         TEXT,
  tokens_reasoning INTEGER,
  cache_read       INTEGER,
  cache_write      INTEGER,
  extra            TEXT,
  extra_verbatim   TEXT,
  part_type        TEXT,
  source_entry_ref TEXT,
  prov_origin      TEXT,
  prov_actor       TEXT,
  prov_delivery    TEXT,
  prov_ownership   TEXT,
  prov_evidence    TEXT,
  prov_input_modality TEXT,
  prov_submission_ref TEXT,
  UNIQUE (session_id, body_digest),
  CHECK (extra IS NULL OR extra_verbatim IS NULL)
) STRICT;

CREATE TRIGGER session_entry_bodies_immutable BEFORE UPDATE ON session_entry_bodies
BEGIN
  SELECT RAISE(ABORT, 'session_entry_bodies rows are immutable; insert a new entry instead');
END;

CREATE TABLE session_content (
  session_id  TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  digest      TEXT NOT NULL CHECK(length(digest) = 64),
  byte_length INTEGER NOT NULL CHECK(byte_length >= 0),
  chunk_count INTEGER GENERATED ALWAYS AS ((byte_length + 65535) / 65536) VIRTUAL,
  PRIMARY KEY (session_id, digest)
) STRICT, WITHOUT ROWID;

CREATE TABLE session_content_chunks (
  session_id  TEXT NOT NULL,
  digest      TEXT NOT NULL,
  chunk_index INTEGER NOT NULL CHECK(chunk_index >= 0),
  data        BLOB NOT NULL CHECK(length(data) BETWEEN 1 AND 65536),
  UNIQUE (session_id, digest, chunk_index),
  FOREIGN KEY (session_id, digest) REFERENCES session_content(session_id, digest) ON DELETE CASCADE
) STRICT;

CREATE TABLE session_generations (
  session_id             TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id          TEXT NOT NULL,
  schema_version         INTEGER NOT NULL,
  harness                TEXT NOT NULL,
  model                  TEXT NOT NULL,
  version                TEXT NOT NULL,
  ts_start               INTEGER NOT NULL,
  ts_end                 INTEGER NOT NULL,
  ts_ingested            INTEGER,
  source_file_path       TEXT,
  source_format          TEXT NOT NULL,
  git_branch             TEXT,
  git_remote             TEXT,
  git_worktree           TEXT,
  git_tracking           TEXT,
  project_hash           TEXT NOT NULL,
  project_file_path      TEXT,
  project_name           TEXT NOT NULL,
  host_slug              TEXT NOT NULL,
  root_session_id        TEXT,
  purpose                TEXT,
  cwd                    TEXT,
  derived_at             INTEGER,
  content_hash           TEXT NOT NULL,
  metadata_hash          TEXT NOT NULL CHECK(length(metadata_hash) = 64),
  redaction_applied      INTEGER NOT NULL CHECK(redaction_applied IN (0,1)),
  redaction_level        TEXT,
  redaction_rule_set_version TEXT,
  redaction_at_ms        INTEGER,
  redaction_content_hash_at_redact TEXT,
  adapter_version        INTEGER,
  diagnostics_partial    INTEGER CHECK(diagnostics_partial IS NULL OR diagnostics_partial IN (0,1)),
  completeness           TEXT NOT NULL CHECK(completeness IN ('complete','incomplete_new')),
  source_evidence_digest TEXT NOT NULL CHECK(length(source_evidence_digest) = 64),
  index_format_version   INTEGER NOT NULL CHECK(index_format_version = 2),
  candidate_digest       TEXT NOT NULL CHECK(length(candidate_digest) = 64),
  prior_evidence         BLOB,
  installed_at_ms        INTEGER NOT NULL CHECK(installed_at_ms >= 0),
  activated_at_ms        INTEGER CHECK(activated_at_ms IS NULL OR activated_at_ms >= 0),
  PRIMARY KEY (session_id, generation_id)
) STRICT;

CREATE TRIGGER session_generations_immutable BEFORE UPDATE ON session_generations
BEGIN
  SELECT RAISE(ABORT, 'generation rows are immutable; a new generation gets a new row');
END;

CREATE TABLE session_generation_subagents (
  session_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  subagent_session_id TEXT NOT NULL,
  parent_uuid TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, ordinal),
  FOREIGN KEY (session_id, generation_id) REFERENCES session_generations(session_id, generation_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE session_generation_commits (
  session_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  hash TEXT NOT NULL,
  message TEXT NOT NULL,
  author_name TEXT NOT NULL,
  author_email TEXT NOT NULL,
  commit_time INTEGER NOT NULL,
  author_time INTEGER NOT NULL,
  PRIMARY KEY (session_id, generation_id, ordinal),
  FOREIGN KEY (session_id, generation_id) REFERENCES session_generations(session_id, generation_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE session_generation_associations (
  session_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  association_id TEXT NOT NULL,
  observed_commit_hash TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, ordinal),
  FOREIGN KEY (session_id, generation_id) REFERENCES session_generations(session_id, generation_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE session_generation_diagnostics (
  session_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  error_type TEXT NOT NULL,
  location TEXT NOT NULL,
  message TEXT NOT NULL,
  remediation TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, ordinal),
  FOREIGN KEY (session_id, generation_id) REFERENCES session_generations(session_id, generation_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE session_generation_title_refs (
  session_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  source_entry_ref TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, ordinal),
  FOREIGN KEY (session_id, generation_id) REFERENCES session_generations(session_id, generation_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE session_generation_entries (
  session_id       TEXT NOT NULL,
  generation_id    TEXT NOT NULL,
  partition_id     INTEGER NOT NULL CHECK(partition_id >= 0),
  entry_index      INTEGER NOT NULL CHECK(entry_index >= 0),
  source_entry_ref TEXT,
  body_digest      TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, partition_id, entry_index),
  FOREIGN KEY (session_id, generation_id) REFERENCES session_generations(session_id, generation_id) ON DELETE CASCADE,
  FOREIGN KEY (session_id, body_digest)   REFERENCES session_entry_bodies(session_id, body_digest)
) STRICT;

CREATE INDEX idx_generation_entries_body ON session_generation_entries(session_id, body_digest);

CREATE TABLE session_generation_content (
  session_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  source_entry_ref TEXT NOT NULL,
  digest TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, source_entry_ref),
  FOREIGN KEY (session_id, generation_id) REFERENCES session_generations(session_id, generation_id) ON DELETE CASCADE,
  FOREIGN KEY (session_id, digest) REFERENCES session_content(session_id, digest)
) STRICT, WITHOUT ROWID;

CREATE INDEX idx_generation_content_digest ON session_generation_content(session_id, digest);

CREATE TABLE session_relationship_evidence_v62 (
  session_id      TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id   TEXT NOT NULL,
  kind            TEXT NOT NULL,
  target_state    TEXT NOT NULL CHECK(target_state IN
    ('target_known','target_known_retained','explicit_none','unknown',
     'conflicting_current_native_evidence')),
  target_local_id TEXT,
  evidence        TEXT,
  anchor_kind TEXT,
  anchor_source_entry_ref TEXT,
  anchor_source_revision_ref TEXT,
  PRIMARY KEY (session_id, generation_id, kind)
) STRICT;

INSERT INTO session_relationship_evidence_v62
  (session_id, generation_id, kind, target_state, target_local_id, evidence,
   anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
SELECT session_id, generation_id, kind, target_state, target_local_id, evidence,
  json_extract(anchor, '$.kind'),
  json_extract(anchor, '$.sourceEntryRef'),
  json_extract(anchor, '$.sourceRevisionRef')
FROM session_relationship_evidence;

DROP TABLE session_relationship_evidence;

ALTER TABLE session_relationship_evidence_v62 RENAME TO session_relationship_evidence;

CREATE INDEX idx_relationship_evidence_started_by_target
  ON session_relationship_evidence(kind, target_local_id, session_id, target_state);

CREATE TABLE session_context_segment_refs (
  session_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  segment_ordinal INTEGER NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  source_entry_ref TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, segment_ordinal, ordinal),
  FOREIGN KEY (session_id, generation_id, segment_ordinal)
    REFERENCES session_context_segments(session_id, generation_id, segment_ordinal) ON DELETE CASCADE
) STRICT;

INSERT INTO session_context_segment_refs
  (session_id, generation_id, segment_ordinal, ordinal, source_entry_ref)
SELECT s.session_id, s.generation_id, s.segment_ordinal, je.key, je.value
FROM session_context_segments s, json_each(s.captured_refs_json) AS je;

CREATE TABLE session_context_segments_v62 (
  session_id                TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id             TEXT NOT NULL,
  segment_ordinal           INTEGER NOT NULL CHECK(segment_ordinal >= 0),
  logical_session_id        TEXT,
  physical_source_id        TEXT NOT NULL,
  coordinate_kind           TEXT NOT NULL,
  start_coordinate          INTEGER,
  end_exclusive             INTEGER,
  decoded_byte_start        INTEGER,
  decoded_byte_end_exclusive INTEGER,
  inclusion                 TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, segment_ordinal)
) STRICT;

INSERT INTO session_context_segments_v62
  (session_id, generation_id, segment_ordinal, logical_session_id, physical_source_id,
   coordinate_kind, start_coordinate, end_exclusive, decoded_byte_start,
   decoded_byte_end_exclusive, inclusion)
SELECT session_id, generation_id, segment_ordinal, logical_session_id, physical_source_id,
  coordinate_kind, start_coordinate, end_exclusive, decoded_byte_start,
  decoded_byte_end_exclusive, inclusion
FROM session_context_segments;

DROP TABLE session_context_segments;

ALTER TABLE session_context_segments_v62 RENAME TO session_context_segments;

CREATE TABLE session_section_native_metadata (
  session_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  partition_id INTEGER NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
  native_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  source_entry_ref TEXT NOT NULL,
  source_type TEXT NOT NULL,
  source_message_role TEXT,
  attachment_turn_index INTEGER,
  attachment_tool_call_id TEXT,
  custom_type TEXT,
  data TEXT,
  PRIMARY KEY (session_id, generation_id, partition_id, ordinal),
  FOREIGN KEY (session_id, generation_id, partition_id)
    REFERENCES session_projection_sections(session_id, generation_id, partition_id) ON DELETE CASCADE
) STRICT;

INSERT INTO session_section_native_metadata
  (session_id, generation_id, partition_id, ordinal, native_id, kind,
   source_entry_ref, source_type, source_message_role, attachment_turn_index,
   attachment_tool_call_id, custom_type, data)
SELECT s.session_id, s.generation_id, s.partition_id, je.key,
  json_extract(je.value, '$.id'),
  json_extract(je.value, '$.kind'),
  json_extract(je.value, '$.source.entryRef'),
  json_extract(je.value, '$.source.sourceType'),
  json_extract(je.value, '$.source.messageRole'),
  json_extract(je.value, '$.attachment.turnIndex'),
  json_extract(je.value, '$.attachment.toolCallId'),
  json_extract(je.value, '$.customType'),
  (je.value -> '$.data')
FROM session_projection_sections s, json_each(s.native_metadata) AS je;

CREATE TABLE session_projection_sections_v62 (
  session_id     TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id  TEXT NOT NULL,
  partition_id   INTEGER NOT NULL CHECK(partition_id >= 0),
  earlier_state  TEXT,
  PRIMARY KEY (session_id, generation_id, partition_id)
) STRICT;

INSERT INTO session_projection_sections_v62
  (session_id, generation_id, partition_id, earlier_state)
SELECT session_id, generation_id, partition_id, earlier_state
FROM session_projection_sections;

DROP TABLE session_projection_sections;

ALTER TABLE session_projection_sections_v62 RENAME TO session_projection_sections;

CREATE TABLE session_captured_stats (
  session_id             TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
  turn_count             INTEGER,
  input_submission_count INTEGER CHECK(input_submission_count IS NULL OR (input_submission_count BETWEEN 0 AND 9007199254740991)),
  tool_call_count        INTEGER,
  subagent_count         INTEGER,
  duration_ms            INTEGER,
  tokens_in              INTEGER,
  tokens_out             INTEGER,
  thought_tokens         INTEGER,
  cached_read_tokens     INTEGER,
  cached_write_tokens    INTEGER,
  seed_json              TEXT CHECK(seed_json IS NULL OR json_valid(seed_json)),
  source                 TEXT NOT NULL CHECK(source IN ('harness','derived')),
  updated_at_ms          INTEGER NOT NULL CHECK(updated_at_ms >= 0),
  overflow               TEXT
) STRICT;

ALTER TABLE sessions ADD COLUMN content_sweep_pending INTEGER NOT NULL DEFAULT 0 CHECK(content_sweep_pending IN (0,1));

CREATE INDEX idx_sessions_sweep_pending ON sessions(session_id) WHERE content_sweep_pending = 1;

UPDATE sessions SET content_sweep_pending = 1
WHERE EXISTS (
  SELECT 1 FROM session_projection_generations g
  WHERE g.session_id = sessions.session_id
    AND g.generation_id IS NOT sessions.active_generation_id
);

INSERT INTO session_captured_stats (
  session_id, turn_count, input_submission_count, tool_call_count, subagent_count,
  duration_ms, tokens_in, tokens_out, thought_tokens, cached_read_tokens,
  cached_write_tokens, seed_json, source, updated_at_ms, overflow
)
SELECT s.session_id,
  json_extract(g.metadata_json, '$.stats.turnCount'),
  json_extract(g.metadata_json, '$.stats.inputSubmissionCount'),
  json_extract(g.metadata_json, '$.stats.toolCallCount'),
  json_extract(g.metadata_json, '$.stats.subagentCount'),
  json_extract(g.metadata_json, '$.stats.durationMs'),
  json_extract(g.metadata_json, '$.stats.tokensIn'),
  json_extract(g.metadata_json, '$.stats.tokensOut'),
  json_extract(g.metadata_json, '$.stats.thoughtTokens'),
  json_extract(g.metadata_json, '$.stats.cachedReadTokens'),
  json_extract(g.metadata_json, '$.stats.cachedWriteTokens'),
  json_extract(g.metadata_json, '$.stats'),
  'harness',
  coalesce(g.activated_at_ms, g.installed_at_ms),
  NULL
FROM sessions s
JOIN session_projection_generations g
  ON g.session_id = s.session_id AND g.generation_id = s.active_generation_id
WHERE s.active_generation_id IS NOT NULL;

DROP VIEW annotations_with_target;

CREATE TABLE annotation_target_entries_v62 (
    annotation_id TEXT PRIMARY KEY REFERENCES annotations(id) ON DELETE CASCADE,
    session_id    TEXT NOT NULL,
    entry_index   INTEGER NOT NULL,
    end_index     INTEGER NOT NULL,
    CHECK (end_index > entry_index)
) STRICT;

INSERT INTO annotation_target_entries_v62 (annotation_id, session_id, entry_index, end_index)
SELECT annotation_id, session_id, entry_index, end_index FROM annotation_target_entries;

DROP TABLE annotation_target_entries;

ALTER TABLE annotation_target_entries_v62 RENAME TO annotation_target_entries;

CREATE INDEX idx_ann_target_entry ON annotation_target_entries(session_id, entry_index);

CREATE VIEW annotations_with_target AS
SELECT
    a.id, a.target_kind_id, tk.name AS target_kind,
    ts.session_id AS target_session_id,
    te.session_id AS target_entry_session_id,
    te.entry_index AS target_entry_index,
    te.end_index AS target_entry_end_index,
    ta.target_annotation_id,
    tp.project_hash AS target_project_hash,
    tsc.association_id AS target_association_id,
    a.annotator_id, ak.name AS annotator_kind,
    ann.name AS annotator_name, ann.display_name AS annotator_display_name,
    a.annotation_type_id, t.type_id, t.display_name AS type_name,
    f.family, c.class,
    a.value, a.confidence, a.reason, a.provenance,
    a.is_primary, a.content_hash, a.created_at, a.updated_at, a.superseded_by
FROM annotations a
    JOIN target_kinds tk ON a.target_kind_id = tk.id
    JOIN annotators ann ON a.annotator_id = ann.id
    JOIN annotator_kinds ak ON ann.kind_id = ak.id
    JOIN annotation_types t ON a.annotation_type_id = t.id
    JOIN annotation_families f ON t.family_id = f.id
    JOIN annotation_classes c ON f.class_id = c.id
    LEFT JOIN annotation_target_sessions ts ON a.id = ts.annotation_id
    LEFT JOIN annotation_target_entries te ON a.id = te.annotation_id
    LEFT JOIN annotation_target_annotations ta ON a.id = ta.annotation_id
    LEFT JOIN annotation_target_projects tp ON a.id = tp.annotation_id
    LEFT JOIN annotation_target_associations tsc ON a.id = tsc.annotation_id;

DROP INDEX idx_session_projection_entries_partition;

CREATE VIEW session_search_source(rid, session_id, entry_index, content_preview, tool_input, tool_output) AS
  SELECT rowid, session_id, entry_index, content_preview, tool_input, tool_output
    FROM session_entries
  UNION ALL
  SELECT body_id, session_id, entry_index, content_preview, tool_input, tool_output
    FROM session_entry_bodies;

CREATE VIRTUAL TABLE session_search_fts USING fts5(
  content_preview, tool_input, tool_output, session_id UNINDEXED, entry_index UNINDEXED,
  content='session_search_source', content_rowid='rid', tokenize='unicode61 remove_diacritics 2');

CREATE TABLE session_search_state (
  id            INTEGER PRIMARY KEY CHECK(id = 1),
  needs_rebuild INTEGER NOT NULL CHECK(needs_rebuild IN (0,1))
) STRICT;

INSERT INTO session_search_state(id, needs_rebuild) VALUES (1, 0);

CREATE TRIGGER session_entry_bodies_fts_ai AFTER INSERT ON session_entry_bodies BEGIN
  INSERT INTO session_search_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    SELECT rid, content_preview, tool_input, tool_output, session_id, entry_index
      FROM session_search_source WHERE rid = new.body_id;
END;

CREATE TRIGGER session_entry_bodies_fts_bd BEFORE DELETE ON session_entry_bodies BEGIN
  INSERT INTO session_search_fts(session_search_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    SELECT 'delete', rid, content_preview, tool_input, tool_output, session_id, entry_index
      FROM session_search_source WHERE rid = old.body_id;
END;
`
