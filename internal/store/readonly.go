package store

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
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
	return OpenReadOnlyWithOptions(path)
}

// OpenReadOnlyWithOptions is OpenReadOnly with the read-only-safe open options a
// dry-run needs to resolve the same capabilities a writable open would. The
// format declaration and the managed-generation reader are honored so a dry run
// plans against the harness targets a real run would use; every configured
// artifact store is wrapped in a write-refusing view and the session locker in a
// no-file view, so no planning path can turn into a file write. The store still
// opens SQLITE_OPEN_READONLY with query_only, runs no migration, and creates no
// file.
func OpenReadOnlyWithOptions(path string, opts ...OpenOption) (*Store, error) {
	o := openOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	declared := o.indexFormats
	if len(declared) == 0 {
		// Preserve the original read-only default: inspect a store that already
		// holds format-2 sessions without configuring an artifact file store.
		declared = []IndexFormat{generationIndexFormat{}}
	}
	formats, err := newIndexFormats(declared)
	if err != nil {
		return nil, err
	}
	conversions, err := newIndexFormatConversions(o.indexConversions, formats)
	if err != nil {
		return nil, err
	}
	artifacts := o.generationArtifacts
	if artifacts != nil {
		artifacts = readOnlyGenerationArtifacts{inner: artifacts}
	}
	locker := o.sessionLocker
	if locker != nil {
		locker = lockFreeSessionLocker{}
	}

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

	// The managed-generation reader is registered so a dry run can inspect a
	// store that already holds format-2 sessions. A caller-wired artifact store
	// crosses as the write-refusing view, so a dry run can resolve the native
	// targets and read staged candidates without changing a file.
	return &Store{
		pool:                pool,
		salt:                installationSalt,
		indexFormats:        formats,
		indexConversions:    conversions,
		generationArtifacts: artifacts,
		sessionLocker:       locker,
	}, nil
}

// lockFreeSessionLocker is the no-file view OpenReadOnlyWithOptions wraps a
// configured locker in. A dry run must change no file, and a lock file is a
// file: shared reads proceed without serializing against a concurrent writer
// (the snapshot read stays transactional and every artifact read verifies its
// digest), while exclusive acquisition refuses because a read-only store never
// activates or removes a generation.
type lockFreeSessionLocker struct{}

var _ SessionLocker = lockFreeSessionLocker{}

// errReadOnlySessionLock is the refusal exclusive acquisition returns.
var errReadOnlySessionLock = errors.New("store: a read-only store cannot take an exclusive session lock; no lock file was created; run without --dry-run to activate or remove a generation")

func (lockFreeSessionLocker) LockShared(context.Context, schema.SessionID) (func() error, error) {
	return func() error { return nil }, nil
}

func (lockFreeSessionLocker) LockExclusive(context.Context, schema.SessionID) (func() error, error) {
	return nil, errReadOnlySessionLock
}

// readOnlyGenerationArtifacts is the write-refusing view OpenReadOnlyWithOptions
// wraps every configured artifact store in. Reads pass through; every write
// refuses with the dry-run guarantee, so no future planning path can stage,
// activate, or repair managed-generation files through a read-only store.
type readOnlyGenerationArtifacts struct{ inner GenerationArtifactStore }

var _ GenerationArtifactStore = readOnlyGenerationArtifacts{}

// errReadOnlyGenerationWrite is the one refusal every write entry point returns.
var errReadOnlyGenerationWrite = errors.New("store: a read-only store cannot write managed-generation files; no file was changed; run without --dry-run to stage or activate a generation")

func (a readOnlyGenerationArtifacts) Stage(context.Context, indexformat.Generation, map[schema.SourceEntryRef][]byte) (indexformat.Generation, error) {
	return indexformat.Generation{}, errReadOnlyGenerationWrite
}

func (a readOnlyGenerationArtifacts) WriteIntent(context.Context, GenerationIntent) error {
	return errReadOnlyGenerationWrite
}

func (a readOnlyGenerationArtifacts) ReadIntent(ctx context.Context, id schema.SessionID) (*GenerationIntent, error) {
	return a.inner.ReadIntent(ctx, id)
}

func (a readOnlyGenerationArtifacts) ClearIntent(context.Context, schema.SessionID) error {
	return errReadOnlyGenerationWrite
}

func (a readOnlyGenerationArtifacts) RepairMetadata(context.Context, schema.SessionID, []byte) error {
	return errReadOnlyGenerationWrite
}

func (a readOnlyGenerationArtifacts) RemoveGeneration(context.Context, schema.SessionID, string) error {
	return errReadOnlyGenerationWrite
}

func (a readOnlyGenerationArtifacts) ReadManifest(ctx context.Context, id schema.SessionID, generationID string) (indexformat.Generation, error) {
	return a.inner.ReadManifest(ctx, id, generationID)
}

func (a readOnlyGenerationArtifacts) ReadBlob(ctx context.Context, id schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	return a.inner.ReadBlob(ctx, id, generationID, record)
}

func (a readOnlyGenerationArtifacts) WritePriorEvidence(context.Context, schema.SessionID, string, []byte) error {
	return errReadOnlyGenerationWrite
}

func (a readOnlyGenerationArtifacts) ReadPriorEvidence(ctx context.Context, id schema.SessionID, generationID string) ([]byte, error) {
	return a.inner.ReadPriorEvidence(ctx, id, generationID)
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
