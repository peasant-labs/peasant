package store

import (
	"context"
	"fmt"
	"os"

	"github.com/peasant-labs/peasant/internal/salt"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// OpenReadOnly opens an existing analytics database for dry-run inspection.
//
// It opens the database file itself with SQLITE_OPEN_READONLY and enables
// PRAGMA query_only on every connection, so no statement can write, no schema
// is created, and no migration runs. A database in WAL mode participates in its
// -shm shared-memory file; SQLite may create or update that transient
// coordination file when the directory is writable, but the stored database
// bytes, schema, and user data are never modified. Reading the database through
// SQLite keeps memory bounded and is how the rest of the codebase inspects a
// database read-only (see SchemaVersionAt and the migration-consent check).
//
// An older or newer schema is refused: dry-run plans against the schema this
// build understands and never migrates. The caller must Close the returned Store.
func OpenReadOnly(path string) (*Store, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("store: dry-run inspection of %s: %w; no files were changed; run a normal harvest to create the analytics database", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("store: dry-run inspection of %s: the path must be a regular file, not a directory or a symbolic link; point at the analytics database file", path)
	}

	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{
		PoolSize:    1,
		Flags:       sqlite.OpenReadOnly,
		PrepareConn: prepareReadOnlyConn,
	})
	if err != nil {
		return nil, fmt.Errorf("store: open %s read-only for dry-run: %w; no files were changed; check the path and read permissions, and close any Peasant process holding the database", path, err)
	}
	conn, err := pool.Take(context.Background())
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("store: take read-only connection for %s: %w; no files were changed", path, err)
	}
	fail := func(err error) (*Store, error) {
		pool.Put(conn)
		_ = pool.Close()
		return nil, err
	}

	var version int
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		version = stmt.ColumnInt(0)
		return nil
	}}); err != nil {
		return fail(fmt.Errorf("store: read schema version from %s for dry-run: %w; no files were changed", path, err))
	}
	if version != CurrentSchemaVersion() {
		return fail(fmt.Errorf("store: dry-run inspection of %s: database schema is %d, this build needs schema %d; no files were changed; run a normal harvest to migrate this database, or use the matching Peasant version", path, version, CurrentSchemaVersion()))
	}

	var installationSalt salt.Salt
	found := false
	if err := sqlitex.ExecuteTransient(conn, "SELECT salt FROM _install_salt WHERE id = 1", &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		if stmt.ColumnType(0) != sqlite.TypeBlob || stmt.ColumnLen(0) != len(installationSalt) {
			return fmt.Errorf("installation salt is invalid; dry-run cannot replace it")
		}
		stmt.ColumnBytes(0, installationSalt[:])
		found = true
		return nil
	}}); err != nil {
		return fail(fmt.Errorf("store: read installation salt from %s for dry-run: %w; no files were changed", path, err))
	}
	if !found {
		return fail(fmt.Errorf("store: dry-run inspection of %s: installation salt is missing; dry-run cannot create it; run a normal harvest first", path))
	}
	pool.Put(conn)

	formats, _ := newIndexFormats(nil)
	conversions, _ := newIndexFormatConversions(nil, formats)
	return &Store{pool: pool, salt: installationSalt, indexFormats: formats, indexConversions: conversions}, nil
}

// prepareReadOnlyConn pins the read-only guarantees on every pooled connection.
// It deliberately omits the write-side PRAGMAs (journal_mode, synchronous,
// mmap_size) that preparePragmas sets for the read-write store.
func prepareReadOnlyConn(conn *sqlite.Conn) error {
	for _, pragma := range []string{
		"PRAGMA query_only = ON;",
		"PRAGMA temp_store = MEMORY;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
	} {
		if err := sqlitex.ExecuteTransient(conn, pragma, nil); err != nil {
			return fmt.Errorf("store: prepare read-only dry-run connection with %q: %w", pragma, err)
		}
	}
	return nil
}
