package store

// migrationV60 adds the managed-generation catalog for the immutable V2
// projection. Immutable generation rows are addressed by
// (session_id, generation_id); the session carries ONE active pointer, and the
// logical relationship table deliberately has NO foreign key to a target
// session so an admitted child survives an absent, unselected or cyclic parent.
//
// The three new sessions columns stay nullable: a session with no active
// generation keeps active_generation_id NULL, and root_session_id/session_purpose
// are durable graph identity that may be legitimately unknown. The V2 generation
// row carries the full durable metadata JSON, so Metadata.Stats is the single
// count authority and no parallel submission count column exists.
//
// session_projection_entries holds the canonical V2 partition entries keyed by
// partition_id (0 = main, 1..N earlier sections in order). The main partition is
// mirrored into the existing session_entries table by the common writer in the
// SAME activation transaction, so search and count paths keep working.
const migrationV60 = `
ALTER TABLE sessions ADD COLUMN active_generation_id TEXT;
ALTER TABLE sessions ADD COLUMN root_session_id TEXT;
ALTER TABLE sessions ADD COLUMN session_purpose TEXT;

CREATE TABLE session_projection_generations (
  session_id             TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id          TEXT NOT NULL,
  metadata_json          TEXT NOT NULL,
  title_refs_json        TEXT NOT NULL DEFAULT '[]',
  input_submission_count INTEGER CHECK(input_submission_count IS NULL OR (input_submission_count BETWEEN 0 AND 9007199254740991)),
  source_evidence_digest TEXT NOT NULL CHECK(length(source_evidence_digest) = 64),
  completeness           TEXT NOT NULL CHECK(completeness IN ('complete','incomplete_new')),
  index_format_version   INTEGER NOT NULL CHECK(index_format_version = 2),
  installed_at_ms        INTEGER NOT NULL CHECK(installed_at_ms >= 0),
  activated_at_ms        INTEGER CHECK(activated_at_ms IS NULL OR activated_at_ms >= 0),
  PRIMARY KEY (session_id, generation_id)
) STRICT;

CREATE TABLE session_relationship_evidence (
  session_id      TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id   TEXT NOT NULL,
  kind            TEXT NOT NULL,
  target_state    TEXT NOT NULL CHECK(target_state IN
    ('target_known','target_known_retained','explicit_none','unknown',
     'conflicting_current_native_evidence')),
  target_local_id TEXT,
  evidence        TEXT,
  anchor          TEXT,
  PRIMARY KEY (session_id, generation_id, kind)
) STRICT;

CREATE TABLE session_projection_sections (
  session_id     TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id  TEXT NOT NULL,
  partition_id   INTEGER NOT NULL CHECK(partition_id >= 0),
  earlier_state  TEXT,
  native_metadata TEXT,
  PRIMARY KEY (session_id, generation_id, partition_id)
) STRICT;

CREATE TABLE session_projection_entries (
  session_id       TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id    TEXT NOT NULL,
  partition_id     INTEGER NOT NULL CHECK(partition_id >= 0),
  entry_index      INTEGER NOT NULL CHECK(entry_index >= 0),
  source_entry_ref TEXT,
  entry_json       TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, partition_id, entry_index)
) STRICT;

CREATE TABLE session_context_segments (
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
  captured_refs_json        TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, segment_ordinal)
) STRICT;

CREATE TABLE session_projection_content (
  session_id       TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id    TEXT NOT NULL,
  source_entry_ref TEXT NOT NULL,
  relative_blob    TEXT NOT NULL,
  byte_length      INTEGER NOT NULL CHECK(byte_length >= 0),
  integrity_digest TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, source_entry_ref)
) STRICT;

CREATE TABLE session_projection_aliases (
  session_id       TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  generation_id    TEXT NOT NULL,
  native_key       TEXT NOT NULL,
  source_entry_ref TEXT NOT NULL,
  PRIMARY KEY (session_id, generation_id, native_key)
) STRICT;

CREATE INDEX idx_session_projection_entries_partition
  ON session_projection_entries(session_id, generation_id, partition_id, entry_index);
`
