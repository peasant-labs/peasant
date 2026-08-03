package store

const migrationV26 = `ALTER TABLE session_entries ADD COLUMN part_type TEXT;`
