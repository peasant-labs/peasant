package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// errDataDirNotWritable reports a data directory (or the database in it) a
// writing command cannot write to, by path, instead of SQLite's "attempt to
// write a readonly database".
func errDataDirNotWritable(path string) error {
	return fmt.Errorf("data dir is not writable: %s; restore write permission (for example `chmod -R u+w %s`), or use a read-only command such as `peasant sessions list` or `peasant prune --dry-run`", path, path)
}

// dataDirWritable reports whether the data directory and the database file in
// it (when they exist) accept writes. A missing directory or file counts as
// writable: openDB creates it. Directory writability is probed by creating
// and removing a temporary file, which works the same on every platform.
func dataDirWritable(dataDir, dbPath string) (bool, string) {
	if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
		probe, err := os.CreateTemp(dataDir, ".peasant-write-probe-*")
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return false, dataDir
			}
		} else {
			name := probe.Name()
			probe.Close()
			os.Remove(name)
		}
	}
	if f, err := os.OpenFile(dbPath, os.O_WRONLY, 0); err == nil {
		f.Close()
	} else if errors.Is(err, fs.ErrPermission) {
		return false, dbPath
	}
	return true, ""
}

// openDBForRead opens the analytics store for a command that only reads it.
// On a writable data dir it is openDB, unchanged. On a read-only one it opens
// the existing database read-only (no migration, no schema creation) so
// listing and dry runs keep working.
func openDBForRead(cmd *cobra.Command) (*store.Store, func(), error) {
	db, err := openReadOnlyIfUnwritable(cmd)
	if err != nil || db == nil {
		if err != nil {
			return nil, func() {}, err
		}
		return openDB(cmd)
	}
	return db, func() { db.Close() }, nil
}

// openReadOnlyIfUnwritable returns a read-only store when the data dir or its
// database cannot be written, and (nil, nil) when they can, so the caller
// keeps its usual writable open.
func openReadOnlyIfUnwritable(cmd *cobra.Command) (*store.Store, error) {
	override := dataDirOverride(cmd)
	dataDir := string(defaults.ResolveDataDirPathWith(override))
	dbPath := string(defaults.ResolveDBFilePathWith(override))
	if ok, _ := dataDirWritable(dataDir, dbPath); ok {
		return nil, nil
	}
	db, err := store.OpenReadOnlyWithOptions(dbPath, append(contentIndexFormats(), store.WithImmutableReadOnly())...)
	if err != nil {
		return nil, fmt.Errorf("open analytics store read-only (the data dir %s is not writable): %w", dataDir, err)
	}
	return db, nil
}
