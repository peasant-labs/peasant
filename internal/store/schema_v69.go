package store

// migrationV69 widens session_source_unavailability.reason so a source that is
// present but unreadable as records is a different cause from a missing source.
// SQLite cannot alter a CHECK, so the table is rebuilt. Existing rows keep the
// missing-source value and stay valid. Foreign keys are disabled for the
// drop and recreate, the same way other table rebuilds are.
const migrationV69 = `
CREATE TABLE session_source_unavailability_v68 AS SELECT * FROM session_source_unavailability;
DROP TABLE session_source_unavailability;

CREATE TABLE session_source_unavailability (
 session_id TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
 reason TEXT NOT NULL CHECK (reason IN ('original-source-unavailable-no-usable-saved-copy','original-source-present-record-unusable'))
) STRICT;

INSERT INTO session_source_unavailability (session_id, reason)
 SELECT session_id, reason FROM session_source_unavailability_v68;
DROP TABLE session_source_unavailability_v68;
`
