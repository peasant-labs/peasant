package store

// migrationV35 adds an external-content FTS5 virtual table over session_entries
// for full-text search of transcript text at per-ENTRY grain. External-content
// mode (content='session_entries', content_rowid='rowid') stores only the
// inverted index — the text lives in session_entries, not a second copy. AFTER
// triggers keep the index in sync with the DELETE+INSERT-per-session write path
// in IndexSessionEntries and with PruneSessions' deletes, so no app code writes
// to the FTS table directly. The 'rebuild' command backfills existing rows.
//
// FTS5 is compiled into modernc.org/sqlite (-DSQLITE_ENABLE_FTS5); no build tag.
// session_entries is STRICT but NOT WITHOUT ROWID, so it has a usable implicit
// rowid for content_rowid. The external-content 'delete' command (rowid + the
// OLD text) removes stale terms — a plain DELETE on the FTS table would corrupt
// the index, so the triggers must always use the command form.
const migrationV35 = `
CREATE VIRTUAL TABLE session_entries_fts USING fts5(
    content_preview,
    tool_input,
    tool_output,
    session_id UNINDEXED,
    entry_index UNINDEXED,
    content='session_entries',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER session_entries_ai AFTER INSERT ON session_entries BEGIN
    INSERT INTO session_entries_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES (new.rowid, new.content_preview, new.tool_input, new.tool_output, new.session_id, new.entry_index);
END;

CREATE TRIGGER session_entries_ad AFTER DELETE ON session_entries BEGIN
    INSERT INTO session_entries_fts(session_entries_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES ('delete', old.rowid, old.content_preview, old.tool_input, old.tool_output, old.session_id, old.entry_index);
END;

CREATE TRIGGER session_entries_au AFTER UPDATE ON session_entries BEGIN
    INSERT INTO session_entries_fts(session_entries_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES ('delete', old.rowid, old.content_preview, old.tool_input, old.tool_output, old.session_id, old.entry_index);
    INSERT INTO session_entries_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES (new.rowid, new.content_preview, new.tool_input, new.tool_output, new.session_id, new.entry_index);
END;

INSERT INTO session_entries_fts(session_entries_fts) VALUES ('rebuild');
`
