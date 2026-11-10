package store

// migrationV74 admits image entries and records their content-addressed
// metadata. SQLite cannot alter a CHECK, so session_entries is rebuilt with
// image in the entry_type set. The new table is created under a temporary
// name, the old table is dropped, and the new table is renamed back to
// session_entries. Renaming the old table first would retarget child foreign
// keys onto the dropped name. Foreign keys are disabled for the drop and
// recreate. The external-content search index is recreated afterwards.
//
// session_attachments is keyed by (session_id, source_entry_ref). partition_id
// is recorded and is not part of the key: a source ref is unique within a
// session, including across partitions. sha256 is empty when no bytes were
// stored. One file per digest lives outside the database.
const migrationV74 = `
DROP TRIGGER IF EXISTS session_entries_ai;
DROP TRIGGER IF EXISTS session_entries_ad;
DROP TRIGGER IF EXISTS session_entries_au;
DROP TRIGGER IF EXISTS session_entry_bodies_fts_ai;
DROP TRIGGER IF EXISTS session_entry_bodies_fts_bd;
DROP TABLE IF EXISTS session_entries_fts;
DROP TABLE IF EXISTS session_search_fts;
DROP VIEW IF EXISTS session_search_source;

CREATE TABLE session_entries_v74 (
    session_id      TEXT NOT NULL REFERENCES sessions(session_id),
    entry_index     INTEGER NOT NULL,
    provider        TEXT NOT NULL,
    entry_type      TEXT NOT NULL CHECK (entry_type IN ('text','tool_use','tool_result','thinking','system','error','result','image')),
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
    tool_call_id    TEXT,
    entry_id        TEXT,
    parent_entry_id TEXT,
    extra           TEXT,
    depth           INTEGER NOT NULL DEFAULT 0,
    parent_index    INTEGER,
    tool_input      TEXT,
    tool_output     TEXT,
    tool_kind       TEXT,
    stop_reason     TEXT,
    part_type       TEXT,
    PRIMARY KEY (session_id, entry_index)
) STRICT;

INSERT INTO session_entries_v74 (
    session_id, entry_index, provider, entry_type, role, timestamp_ms, content_preview,
    tokens_in, tokens_out, has_tool_use, tool_names_csv, has_thinking, is_error,
    raw_byte_length, tool_call_id, entry_id, parent_entry_id, extra, depth, parent_index,
    tool_input, tool_output, tool_kind, stop_reason, part_type
)
SELECT
    session_id, entry_index, provider, entry_type, role, timestamp_ms, content_preview,
    tokens_in, tokens_out, has_tool_use, tool_names_csv, has_thinking, is_error,
    raw_byte_length, tool_call_id, entry_id, parent_entry_id, extra, depth, parent_index,
    tool_input, tool_output, tool_kind, stop_reason, part_type
FROM session_entries;

DROP TABLE session_entries;
ALTER TABLE session_entries_v74 RENAME TO session_entries;

CREATE INDEX idx_session_entries_role ON session_entries(session_id, role);
CREATE INDEX idx_session_entries_type ON session_entries(session_id, entry_type);
CREATE INDEX idx_session_entries_error ON session_entries(session_id, is_error) WHERE is_error = 1;
CREATE INDEX idx_session_entries_depth ON session_entries(session_id, depth);
CREATE INDEX idx_session_entries_parent ON session_entries(session_id, parent_index) WHERE parent_index IS NOT NULL;

CREATE VIRTUAL TABLE session_entries_fts
USING fts5(
    content_preview,
    tool_input,
    tool_output,
    session_id UNINDEXED,
    entry_index UNINDEXED,
    content='session_entries',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER session_entries_ai AFTER INSERT ON session_entries BEGIN
    INSERT INTO session_entries_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES (new.rowid, new.content_preview, new.tool_input, new.tool_output, new.session_id, new.entry_index);
END;

CREATE TRIGGER session_entries_ad AFTER DELETE ON session_entries BEGIN
    INSERT INTO session_entries_fts(session_entries_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES ('delete', old.rowid, old.content_preview, old.tool_input, old.tool_output, old.session_id, old.entry_index);
END;

CREATE TRIGGER session_entries_au AFTER UPDATE ON session_entries BEGIN
    INSERT INTO session_entries_fts(session_entries_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES ('delete', old.rowid, old.content_preview, old.tool_input, old.tool_output, old.session_id, old.entry_index);
    INSERT INTO session_entries_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES (new.rowid, new.content_preview, new.tool_input, new.tool_output, new.session_id, new.entry_index);
END;

INSERT INTO session_entries_fts(session_entries_fts) VALUES ('rebuild');

CREATE VIEW session_search_source(rid, session_id, entry_index, content_preview, tool_input, tool_output) AS
  SELECT rowid, session_id, entry_index, content_preview, tool_input, tool_output
    FROM session_entries
  UNION ALL
  SELECT body_id, session_id, entry_index, content_preview, tool_input, tool_output
    FROM session_entry_bodies;

CREATE VIRTUAL TABLE session_search_fts USING fts5(
  content_preview, tool_input, tool_output, session_id UNINDEXED, entry_index UNINDEXED,
  content='session_search_source', content_rowid='rid', tokenize='unicode61 remove_diacritics 2');

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

INSERT INTO session_search_fts(session_search_fts) VALUES ('rebuild');

CREATE TABLE session_attachments (
    session_id       TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    source_entry_ref TEXT NOT NULL,
    partition_id     TEXT NOT NULL DEFAULT '',
    sha256           TEXT,
    media_type       TEXT,
    byte_size        INTEGER NOT NULL DEFAULT 0 CHECK (byte_size >= 0),
    width            INTEGER NOT NULL DEFAULT 0 CHECK (width >= 0),
    height           INTEGER NOT NULL DEFAULT 0 CHECK (height >= 0),
    omitted_reason   TEXT CHECK (omitted_reason IS NULL OR omitted_reason IN ('too_large','unsupported','missing')),
    PRIMARY KEY (session_id, source_entry_ref)
) STRICT;

CREATE INDEX idx_session_attachments_sha ON session_attachments(sha256) WHERE sha256 IS NOT NULL;
`
