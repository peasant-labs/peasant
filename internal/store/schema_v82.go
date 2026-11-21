package store

// mappingSetSchemaVersion is the schema that lets a generation read the
// parsed rows of another generation of the same session.
const mappingSetSchemaVersion = 82

// migrationV82 adds the one nullable column that names the generation whose
// parsed rows a generation reads. NULL means the generation owns its rows,
// which is the state of every existing row, so the migration writes no row
// and needs no backfill. A non-NULL value is never empty and never names the
// row itself; the CHECK refers to generation_id, which SQLite accepts in an
// ADD COLUMN on this STRICT table.
//
// No index, foreign key or trigger is added. The reader resolves a
// generation's rows by its own primary key, so no query looks the column up
// in reverse. SQLite cannot add a composite foreign key through ADD COLUMN,
// and a delete guard would interfere with the cascades from sessions and
// with a representation delete. Reachability is kept in Go instead: a writer
// of a reference must check, inside its transaction, that the named
// generation exists in the same session and owns its rows.
//
// session_generations rows stay immutable: the column is written once, at
// insert, and the existing UPDATE trigger refuses any later change to it.
const migrationV82 = `
ALTER TABLE session_generations ADD COLUMN mapping_set_id TEXT CHECK (mapping_set_id IS NULL OR (length(mapping_set_id) > 0 AND mapping_set_id <> generation_id));
`
