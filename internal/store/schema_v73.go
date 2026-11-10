package store

// migrationV73 adds the conversation turn count to the session's captured
// stats row. The count is the producer's folded conversation turns (projected
// turns whose rendered role is not system), computed once during indexing and
// carried to every count surface. The retired turn_count column stays for
// stores that predate this migration; the upgrade backfill copies its value
// only for a session whose entries are gone.
//
// NOT NULL DEFAULT 0 means a fresh capture row starts at zero and the capture
// lane writes the real count in the same transaction. A plain ALTER TABLE ADD
// COLUMN, no table rebuild.
const migrationV73 = `
ALTER TABLE session_captured_stats ADD COLUMN conversation_turn_count INTEGER NOT NULL DEFAULT 0
 CHECK(conversation_turn_count >= 0);
`

// conversationTurnCountBackfillSchemaVersion is the last schema version before
// V73. Only a store upgraded from an earlier version runs the one-time fold; a
// later head version must not re-run it on every future migration.
const conversationTurnCountBackfillSchemaVersion int64 = 73

// shouldBackfillConversationTurnCounts reports whether a pre-migration
// user_version needs the one-time conversation count fold.
func shouldBackfillConversationTurnCounts(version int64) bool {
	return version > 0 && version < conversationTurnCountBackfillSchemaVersion
}
