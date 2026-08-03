package store

// migrationV32 creates the lesson_sources provenance table to track which
// (annotation, session) pairs contributed to each lesson. This supports
// provenance tracking when the same lesson is imported from multiple friction
// episodes — the lessons table deduplicates by (topic, rule, failure_mode),
// but lesson_sources records every contributing episode.
const migrationV32 = `
CREATE TABLE lesson_sources (
    id                    TEXT PRIMARY KEY,
    lesson_id             TEXT NOT NULL REFERENCES lessons(id) ON DELETE CASCADE,
    episode_annotation_id TEXT NOT NULL,
    session_id            TEXT NOT NULL,
    created_at            INTEGER NOT NULL,
    UNIQUE(lesson_id, episode_annotation_id, session_id)
) STRICT;

CREATE INDEX idx_lesson_sources_lesson ON lesson_sources(lesson_id);
CREATE INDEX idx_lesson_sources_session ON lesson_sources(session_id);
`
