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

// openReadOnlyNoSidecars opens the database file at path read-only without
// creating sidecars: when no -wal file exists there is no uncommitted state
// to miss, so it opens with immutable=1 and SQLite creates neither -wal nor
// -shm; when a -wal exists a normal read-only open reads the committed plus
// WAL state. It never writes the database file or the -wal. A read-only
// reader of a live WAL store still records its read mark in the existing
// -shm, which is shared memory every reader updates, so the -shm is not
// byte-stable. The caller wraps the error with what it was reading for.
func openReadOnlyNoSidecars(path string) (*sqlite.Conn, error) {
	if _, err := os.Stat(path + "-wal"); err != nil && os.IsNotExist(err) {
		uriPath := filepath.ToSlash(path)
		if !strings.HasPrefix(uriPath, "/") {
			uriPath = "/" + uriPath
		}
		uri := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "immutable=1"}).String()
		return sqlite.OpenConn(uri, sqlite.OpenReadOnly, sqlite.OpenURI)
	}
	return sqlite.OpenConn(path, sqlite.OpenReadOnly)
}

// SchemaVersionAt reports the PRAGMA user_version of the database file at
// dbPath WITHOUT opening it for write or applying any migration. It opens
// through openReadOnlyNoSidecars, so it creates no sidecars and never writes
// the database or its -wal. A caller that
// wants to REUSE an existing database only when it already matches the schema
// this build writes compares the result against CurrentSchemaVersion; Open
// would instead migrate the file as a side effect, which is the opposite of a
// compatibility check.
func SchemaVersionAt(dbPath string) (int, error) {
	conn, err := openReadOnlyNoSidecars(dbPath)
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
