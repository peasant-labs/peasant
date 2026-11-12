package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// SchemaVersion reports the version through the already-open store connection.
func (s *Store) SchemaVersion() (int, error) {
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return 0, err
	}
	defer s.pool.Put(conn)
	var version int
	err = sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { version = int(stmt.ColumnInt64(0)); return nil },
	})
	if err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return version, nil
}

// CurrentSchemaVersion is the PRAGMA user_version an up-to-date database
// carries. The migration framework applies one migration per version and stamps
// the count, so the length of the migration list IS the current version.
func CurrentSchemaVersion() int {
	return len(dbSchema.Migrations)
}

// SchemaVersionAt reports the PRAGMA user_version of the database file at
// dbPath WITHOUT opening it for write or applying any migration. It opens
// read-only without leaving -wal/-shm sidecars behind: immutable=1 when no
// -wal file exists, otherwise a normal read-only open. A caller that
// wants to REUSE an existing database only when it already matches the schema
// this build writes compares the result against CurrentSchemaVersion; Open
// would instead migrate the file as a side effect, which is the opposite of a
// compatibility check.
func SchemaVersionAt(dbPath string) (int, error) {
	openPath, flags := dbPath, sqlite.OpenReadOnly
	if _, err := os.Stat(dbPath + "-wal"); err != nil && os.IsNotExist(err) {
		uriPath := filepath.ToSlash(dbPath)
		if !strings.HasPrefix(uriPath, "/") {
			uriPath = "/" + uriPath
		}
		openPath = (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "immutable=1"}).String()
		flags |= sqlite.OpenURI
	}
	conn, err := sqlite.OpenConn(openPath, flags)
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
