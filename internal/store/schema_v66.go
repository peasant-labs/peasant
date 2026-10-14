package store

// migrationV66 adds the session thinking-level columns.
//
// thinking_level is the canonical level the producer resolved for the session
// (the earliest valid root-assistant observation). NULL means unknown, which is
// distinct from the canonical 'off'. Its CHECK mirrors schema.AllThinkingLevels
// exactly; SQLite cannot ALTER a CHECK, so any future change to that set needs a
// table-rebuild migration, and migration_v66_check_test.go fails until one lands.
//
// thinking_level_raw is the exact native spelling kept as source evidence. It
// carries no CHECK: its byte bound is enforced at ingest and by the wire
// contract, not by the store, so an over-long stored value reads back as stored.
//
// thinking_level_history is the JSON array of later in-session changes, in
// source order with consecutive duplicates collapsed. NULL means no change was
// observed (absent, never an empty array), which keeps a session with no
// history distinct from one whose history was recorded as empty.
const migrationV66 = `
ALTER TABLE sessions
  ADD COLUMN thinking_level TEXT
  CHECK (thinking_level IS NULL OR thinking_level IN
    ('off','minimal','low','medium','high','xhigh','max','ultra','ultracode'));
ALTER TABLE sessions
  ADD COLUMN thinking_level_raw TEXT;
ALTER TABLE sessions
  ADD COLUMN thinking_level_history TEXT;
`
