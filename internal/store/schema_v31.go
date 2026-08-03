package store

// migrationV31 deduplicates the lessons table and adds a UNIQUE index on
// (topic, rule, failure_mode) so that `memory build` is idempotent.
//
// Without this, running `memory build --from-file` multiple times inserts
// duplicate rows (different UUIDs, same content). The retrieval system then
// returns N copies of the same lesson instead of N distinct relevant ones.
//
// The migration:
//  1. Creates lessons_dedup with the UNIQUE constraint
//  2. Copies only the newest row per (topic, rule, failure_mode) group
//  3. Swaps the tables
//  4. Recreates indexes including the new unique index
const migrationV31 = `
CREATE TABLE lessons_dedup (
    id                     TEXT PRIMARY KEY,
    episode_annotation_id  TEXT NOT NULL,
    session_id             TEXT NOT NULL,
    topic                  TEXT NOT NULL,
    rule                   TEXT NOT NULL,
    failure_mode           TEXT NOT NULL,
    situation_embedding    BLOB,
    created_at             INTEGER NOT NULL
) STRICT;

-- Keep only the newest row per (topic, rule, failure_mode) group.
INSERT INTO lessons_dedup
SELECT id, episode_annotation_id, session_id, topic, rule, failure_mode, situation_embedding, created_at
FROM lessons
WHERE rowid IN (
    SELECT rowid FROM (
        SELECT rowid, ROW_NUMBER() OVER (
            PARTITION BY topic, rule, failure_mode
            ORDER BY created_at DESC, rowid DESC
        ) AS rn
        FROM lessons
    ) WHERE rn = 1
);

DROP TABLE lessons;
ALTER TABLE lessons_dedup RENAME TO lessons;

CREATE INDEX idx_lessons_session ON lessons(session_id);
CREATE INDEX idx_lessons_annotation ON lessons(episode_annotation_id);
CREATE UNIQUE INDEX idx_lessons_dedup ON lessons(topic, rule, failure_mode);
`
