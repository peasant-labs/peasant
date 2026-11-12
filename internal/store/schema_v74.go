package store

// migrationV74 adds the conversation turn count to the session's captured
// stats row. The count is the producer's folded conversation turns (projected
// turns whose rendered role is not system), computed once during indexing and
// carried to every count surface. The retired turn_count column stays for
// stores that predate this migration; the upgrade backfill copies its value
// only for a session whose entries are gone.
//
// NOT NULL DEFAULT 0 means a fresh capture row starts at zero and the capture
// lane writes the real count in the same transaction. A plain ALTER TABLE ADD
// COLUMN, no table rebuild.
const migrationV74 = `
ALTER TABLE session_captured_stats ADD COLUMN conversation_turn_count INTEGER NOT NULL DEFAULT 0
 CHECK(conversation_turn_count >= 0);
`

// conversationTurnCountBackfillSchemaVersion is the schema version whose
// upgrade step carries the one-time conversation count fold. The fold is
// that step's data fill and commits in its transaction, so it runs exactly
// when a store crosses this version and never again.
const conversationTurnCountBackfillSchemaVersion = 74
