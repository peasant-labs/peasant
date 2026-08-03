package store

// migrationV40 creates the append-only producer association ledger. Every row
// owns an opaque durable ID for one session and the commit hash observed during
// that session. The ledger deliberately does not reference session_commits:
// a later re-ingest may replace the current commit relation, while the original
// observation must remain available to the rewrite timeline as historical fact.
//
// The migration adds a table and index, then clears pushed_at only for pushed
// sessions whose current commit rows were backfilled. It does not rebuild
// sessions or pulled_transcripts, so the independent license CHECK constraints
// introduced by V37/V38 remain byte-for-byte intact.
const migrationV40 = `
CREATE TABLE session_commit_associations (
    association_id       TEXT PRIMARY KEY,
    session_id           TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
    observed_commit_hash TEXT NOT NULL,
    subject              TEXT,
    author_time          INTEGER,
    created_at           INTEGER NOT NULL DEFAULT (unixepoch('now') * 1000),
    UNIQUE (session_id, observed_commit_hash)
) STRICT;

INSERT INTO session_commit_associations (
    association_id, session_id, observed_commit_hash, subject, author_time
)
SELECT
    'assoc-' || lower(hex(randomblob(16))),
    session_id,
    commit_hash,
    message,
    author_time
FROM session_commits;

-- A session that was already pushed before this ledger existed did not send
-- the association rows backfilled above. Replay only those affected sessions
-- through the ordinary push candidate path. Sessions without a backfilled
-- association retain their prior pushed state.
UPDATE sessions
SET pushed_at = NULL
WHERE pushed_at IS NOT NULL
  AND EXISTS (
      SELECT 1
      FROM session_commits sc
      WHERE sc.session_id = sessions.session_id
  );

CREATE INDEX idx_session_commit_associations_session
    ON session_commit_associations(session_id, observed_commit_hash);
`
