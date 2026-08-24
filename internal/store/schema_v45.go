package store

// migrationV45 creates the OpenCode change cursor. OpenCode rewrites a session
// in place: an edit can replace earlier turns without moving any time column, so
// the session clock alone cannot see the change. Every mutation still advances
// the session's newest event sequence. This table records the last ingested
// sequence per session, so a rescan re-ingests a session whose current sequence
// has moved past the stored value even when no time column changed.
const migrationV45 = `
CREATE TABLE opencode_session_seq_cursor (
  session_id TEXT PRIMARY KEY,
  last_seq INTEGER NOT NULL CHECK(last_seq >= 0)
) STRICT;
`
