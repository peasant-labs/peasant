package store

const (
	sourceUnavailabilitySchemaVersion  = 63
	sourceDiagnosticLanesSchemaVersion = 81
)

// migrationV81 separates diagnostic lanes without changing the old causes.
// Existing rows predate lane ownership and are retained as stored-content
// evidence, the original owner of the ledger. Other lanes may report once after
// the upgrade; their observations do not erase the migrated cause.
const migrationV81 = `
CREATE TABLE session_source_unavailability_v80 AS SELECT session_id, reason FROM session_source_unavailability;
DROP TABLE session_source_unavailability;

CREATE TABLE session_source_unavailability (
 session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
 lane TEXT NOT NULL CHECK (lane IN ('stored_content','pair_repair','publication_capture','index')),
 reason TEXT NOT NULL CHECK (reason IN ('original-source-unavailable-no-usable-saved-copy','original-source-present-record-unusable')),
 PRIMARY KEY (session_id, lane)
) STRICT;

INSERT INTO session_source_unavailability (session_id, lane, reason)
 SELECT session_id, 'stored_content', reason FROM session_source_unavailability_v80;
DROP TABLE session_source_unavailability_v80;
`
