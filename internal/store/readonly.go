package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/peasant-labs/peasant/internal/salt"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// OpenReadOnly inspects a checkpointed existing database without opening its
// source files through SQLite: even mode=ro can create or update WAL/SHM files.
// A private in-memory copy uses the existing Store readers. Active sidecars,
// changing input, pending migrations and missing installation salt are refused.
// The caller must close the Store. No source directory or database is created.
func OpenReadOnly(path string) (*Store, error) {
	data, err := readCheckpointedDatabase(path)
	if err != nil {
		return nil, fmt.Errorf("store: dry-run inspection of %s: %w; no files were changed; finish active database work and close Peasant before retrying, or run a normal harvest to initialize or migrate missing state", path, err)
	}
	// SQLite Deserialize cannot open a WAL-mode image. With no journal/WAL and
	// a stable copied image, change only the PRIVATE copy's read/write versions
	// to rollback mode. The original file and its header are never modified.
	data[18], data[19] = 1, 1
	pool, err := sqlitex.NewPool("file:peasant-readonly?mode=memory&cache=private", sqlitex.PoolOptions{
		Flags:    sqlite.OpenReadWrite | sqlite.OpenCreate | sqlite.OpenURI | sqlite.OpenMemory | sqlite.OpenPrivateCache,
		PoolSize: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("store: prepare private dry-run database: %w", err)
	}
	conn, err := pool.Take(context.Background())
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	fail := func(err error) (*Store, error) {
		pool.Put(conn)
		_ = pool.Close()
		return nil, fmt.Errorf("store: inspect %s for dry-run: %w; source files were unchanged; run a normal harvest to initialize or migrate this state, or use a compatible Peasant version", path, err)
	}
	if err := conn.Deserialize("main", data); err != nil {
		return fail(err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA query_only=ON", nil); err != nil {
		return fail(err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA temp_store=MEMORY", nil); err != nil {
		return fail(err)
	}
	var version int
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		version = stmt.ColumnInt(0)
		return nil
	}}); err != nil {
		return fail(err)
	}
	if version != CurrentSchemaVersion() {
		return fail(fmt.Errorf("database schema is %d; this build needs schema %d to plan current work, so migration or a compatible build is required", version, CurrentSchemaVersion()))
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
		return fail(err)
	}
	if !found {
		return fail(fmt.Errorf("installation salt is missing; dry-run cannot create it"))
	}
	pool.Put(conn)
	formats, _ := newIndexFormats(nil)
	conversions, _ := newIndexFormatConversions(nil, formats)
	return &Store{pool: pool, salt: installationSalt, indexFormats: formats, indexConversions: conversions}, nil
}

func readCheckpointedDatabase(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database path must name a regular file, not a symbolic link; use the database's actual path so its sidecars can be checked")
	}
	if err := requireCheckpointedDatabase(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("database is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	if len(data) < 100 || !bytes.Equal(data[:16], []byte("SQLite format 3\x00")) ||
		(data[18] != 1 && data[18] != 2) || data[18] != data[19] {
		return nil, fmt.Errorf("database has no supported SQLite format header")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	expected := sha256.Sum256(data)
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(expected[:], digest.Sum(nil)) {
		return nil, fmt.Errorf("database changed during capture; a stable checkpointed database is required")
	}
	if err := requireCheckpointedDatabase(path); err != nil {
		return nil, err
	}
	return data, nil
}

func requireCheckpointedDatabase(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("database sidecar %s exists; dry-run requires a closed, checkpointed database and will not ignore or modify its journal", path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
