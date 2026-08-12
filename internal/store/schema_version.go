package store

import (
	"fmt"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// CurrentSchemaVersion is the PRAGMA user_version an up-to-date database
// carries. The migration framework applies one migration per version and stamps
// the count, so the length of the migration list IS the current version.
func CurrentSchemaVersion() int {
	return len(dbSchema.Migrations)
}

// SchemaVersionAt reports the PRAGMA user_version of the database file at
// dbPath WITHOUT opening it for write or applying any migration. A caller that
// wants to REUSE an existing database only when it already matches the schema
// this build writes compares the result against CurrentSchemaVersion; Open
// would instead migrate the file as a side effect, which is the opposite of a
// compatibility check.
func SchemaVersionAt(dbPath string) (int, error) {
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadOnly)
	if err != nil {
		return 0, fmt.Errorf("store: open %s to read schema version: %w", dbPath, err)
	}
	defer conn.Close()

	var version int
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			version = int(stmt.ColumnInt64(0))
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: read user_version from %s: %w", dbPath, err)
	}
	return version, nil
}
