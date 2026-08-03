package store

// migrationV28 creates the lessons table for the agent memory system.
//
// Each lesson is extracted from an annotated friction episode and contains:
//   - topic: short domain tag (e.g. "database/migrations", "api/websocket")
//   - rule: actionable instruction ("When X, do Y")
//   - failure_mode: what goes wrong without the rule
//   - situation_embedding: float32 vector as BLOB for cosine similarity retrieval
//
// The episode_annotation_id FK links back to the source friction annotation.
// session_id is denormalized for efficient per-session queries.
const migrationV28 = `
CREATE TABLE lessons (
    id                     TEXT PRIMARY KEY,
    episode_annotation_id  TEXT NOT NULL,
    session_id             TEXT NOT NULL,
    topic                  TEXT NOT NULL,
    rule                   TEXT NOT NULL,
    failure_mode           TEXT NOT NULL,
    situation_embedding    BLOB,
    created_at             INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_lessons_session ON lessons(session_id);
CREATE INDEX idx_lessons_annotation ON lessons(episode_annotation_id);
`
