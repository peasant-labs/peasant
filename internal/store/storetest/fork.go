package storetest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
)

// sqliteSidecars are the files a write-ahead-log database keeps beside the
// main database file. A copy that drops them can lose the last transactions
// that have not been checkpointed yet, which would make a fork silently
// differ from the store it was copied from.
var sqliteSidecars = []string{"-wal", "-shm"}

// ForkSeeded copies one seeded database to every destination and returns the
// destination paths. The source must be closed: an open connection may still
// be writing, and a copy of a page in flight is not a copy of the store.
//
// The main database and each sidecar that exists at the source are copied. A
// sidecar that does not exist is not created at the destination, so a store
// without a write-ahead log does not grow an empty one. Parent directories of
// each destination are created; an existing destination file is replaced.
//
// The source is never modified: this is the "one seeded store, two independent
// runs" step of a parity fixture, and both forks must start from identical
// bytes.
func ForkSeeded(t *testing.T, source string, destinations ...string) []string {
	t.Helper()
	if err := forkSeeded(source, destinations...); err != nil {
		t.Fatal(err)
	}
	return destinations
}

// forkSeeded is ForkSeeded's error-returning body, so the refusals are
// testable without provoking a Fatal on the caller's test.
func forkSeeded(source string, destinations ...string) error {
	if len(destinations) == 0 {
		return fmt.Errorf("storetest: ForkSeeded(%q) was given no destination; a fork needs at least one copy, because a run that mutates the only copy destroys the comparison baseline; fix: pass the database paths to copy to", source)
	}
	seen := make(map[string]struct{}, len(destinations))
	for _, destination := range destinations {
		if destination == "" {
			return fmt.Errorf("storetest: ForkSeeded(%q) was given an empty destination; an empty path would resolve relative to the test process's working directory and overwrite an unrelated file; fix: name each destination explicitly", source)
		}
		if filepath.Clean(destination) == filepath.Clean(source) {
			return fmt.Errorf("storetest: ForkSeeded(%q) names the source as a destination; copying a database onto itself would truncate it before the copy reads it; fix: name a distinct destination", source)
		}
		if _, duplicate := seen[filepath.Clean(destination)]; duplicate {
			return fmt.Errorf("storetest: ForkSeeded(%q) names the destination %q twice; each fork must be an independent copy; fix: drop the repeated destination", source, destination)
		}
		seen[filepath.Clean(destination)] = struct{}{}
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("storetest: ForkSeeded: the source database %q was not readable: %v; the seed run must close its store before the fork, and the path must be the file the run actually wrote", source, err)
	}
	for _, destination := range destinations {
		if err := copyDatabaseFiles(source, destination); err != nil {
			return fmt.Errorf("storetest: ForkSeeded: copy %q to %q: %w", source, destination, err)
		}
	}
	return nil
}

// ForkStore closes db, then copies the database at source to every
// destination with ForkSeeded and returns the destination paths. It is the
// convenience form for a caller that still holds the store it seeded.
//
// A Close error is returned rather than fatal: the caller owns the store's
// cleanup, and a store that already closed itself is a caller mistake worth
// reporting instead of hiding. Calling ForkStore twice on the same store is
// safe, because store.Close is idempotent.
func ForkStore(t *testing.T, db *store.Store, source string, destinations ...string) ([]string, error) {
	t.Helper()
	if db == nil {
		return nil, errors.New("storetest: ForkStore was given a nil store; there is no seeded database to close and fork; fix: pass the store the seed run returned")
	}
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("storetest: ForkStore could not close the seeded store at %q before copying it: %w; a live connection can leave a page in flight, so the copy would not be the store; fix: stop the writer and retry", source, err)
	}
	return ForkSeeded(t, source, destinations...), nil
}

// copyDatabaseFiles copies the main database and every existing sidecar. It is
// unexported because ForkSeeded is the supported entry point: a caller that
// copies only the main file is exactly the mistake the sidecar list prevents.
func copyDatabaseFiles(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create the destination directory for %q: %w", destination, err)
	}
	if err := copyFile(source, destination); err != nil {
		return err
	}
	for _, suffix := range sqliteSidecars {
		if err := copyFileIfPresent(source+suffix, destination+suffix); err != nil {
			return err
		}
	}
	return nil
}

// copyFileIfPresent copies a sidecar when it exists and does nothing when it
// does not. Any other read error is returned: permission failures must not
// look like an absent file.
func copyFileIfPresent(source, destination string) error {
	_, err := os.Stat(source)
	switch {
	case err == nil:
		return copyFile(source, destination)
	case errors.Is(err, fs.ErrNotExist):
		// Remove a stale sidecar at the destination: a pre-existing -wal from a
		// different database would be replayed against this one, which SQLite
		// refuses or (worse) applies with the wrong salt.
		if removeErr := os.Remove(destination); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return fmt.Errorf("remove the stale destination sidecar %q: %w", destination, removeErr)
		}
		return nil
	default:
		return fmt.Errorf("stat the source sidecar %q: %w", source, err)
	}
}

// copyFile copies one file, replacing the destination. It writes through a
// temporary file in the destination directory and renames it into place, so a
// failed copy never leaves a half-written database that looks valid.
func copyFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read %q: %w", source, err)
	}
	dir := filepath.Dir(destination)
	temp, err := os.CreateTemp(dir, ".fork-*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %q: %w", dir, err)
	}
	tempName := temp.Name()
	cleanup := func() { _ = os.Remove(tempName) }
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("write %q: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %q: %w", tempName, err)
	}
	if err := os.Chmod(tempName, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("set the permissions of %q: %w", tempName, err)
	}
	if err := os.Rename(tempName, destination); err != nil {
		cleanup()
		return fmt.Errorf("install %q: %w", destination, err)
	}
	return nil
}
