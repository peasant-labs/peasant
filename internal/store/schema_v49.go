package store

// migrationV49 stores durable entry-target anchors and their repair state.
// A row can outlive annotation_target_entries when re-index repair cannot prove
// a safe target. That lets push and export fail closed instead of publishing an
// annotation on the wrong transcript entry.
const migrationV49 = `
CREATE TABLE annotation_target_anchors (
    annotation_id TEXT PRIMARY KEY REFERENCES annotations(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    entry_index INTEGER,
    end_index INTEGER,
    state TEXT NOT NULL CHECK (state IN ('resolved', 'unresolved', 'superseded')),
    entry_id TEXT,
    tool_call_id TEXT,
    entry_type TEXT,
    role TEXT,
    part_type TEXT,
    content_fingerprint TEXT,
    updated_at INTEGER NOT NULL,
    CHECK ((state = 'resolved' AND entry_index IS NOT NULL AND end_index IS NOT NULL AND end_index > entry_index) OR state IN ('unresolved', 'superseded'))
) STRICT;

CREATE INDEX idx_annotation_target_anchors_session_state
    ON annotation_target_anchors(session_id, state);

INSERT INTO annotation_target_anchors (
    annotation_id, session_id, entry_index, end_index, state,
    entry_id, tool_call_id, entry_type, role, part_type, content_fingerprint, updated_at
)
SELECT
    te.annotation_id,
    te.session_id,
    te.entry_index,
    te.end_index,
    'resolved',
    se.entry_id,
    se.tool_call_id,
    se.entry_type,
    se.role,
    se.part_type,
    se.content_preview,
    CAST(strftime('%s','now') AS INTEGER) * 1000
FROM annotation_target_entries te
JOIN session_entries se
  ON se.session_id = te.session_id
 AND se.entry_index = te.entry_index;
`
