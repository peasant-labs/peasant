package store

// migrationV29 drops the FK constraint on lessons.episode_annotation_id.
// The annotation_id is informational (links back to the source episode) but
// lessons should be importable without requiring the annotation to exist in
// the local DB — e.g., when lessons are extracted by an LLM and the
// annotation_id is empty or references a different DB.
const migrationV29 = `
CREATE TABLE lessons_v2 (
    id                     TEXT PRIMARY KEY,
    episode_annotation_id  TEXT NOT NULL,
    session_id             TEXT NOT NULL,
    topic                  TEXT NOT NULL,
    rule                   TEXT NOT NULL,
    failure_mode           TEXT NOT NULL,
    situation_embedding    BLOB,
    created_at             INTEGER NOT NULL
) STRICT;

INSERT INTO lessons_v2 SELECT * FROM lessons;
DROP TABLE lessons;
ALTER TABLE lessons_v2 RENAME TO lessons;

CREATE INDEX idx_lessons_session ON lessons(session_id);
CREATE INDEX idx_lessons_annotation ON lessons(episode_annotation_id);
`
