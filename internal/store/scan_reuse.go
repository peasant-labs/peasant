package store

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// ReadSurface names a production read whose compatibility floor is tested
// against the migration chain. The zero value is not a surface.
type ReadSurface string

const (
	ReadSurfaceKnownSessions  ReadSurface = "known_sessions"
	ReadSurfaceClaudeEvidence ReadSurface = "claude_evidence"
)

// AllReadSurfaces lists the supported scan reuse surfaces.
var AllReadSurfaces = []ReadSurface{ReadSurfaceKnownSessions, ReadSurfaceClaudeEvidence}

func (s ReadSurface) String() string { return string(s) }

// NewReadSurface validates a surface at a string boundary.
func NewReadSurface(value string) (ReadSurface, error) {
	surface := ReadSurface(value)
	if _, ok := surface.floor(); !ok {
		return "", fmt.Errorf("store: unknown scan reuse surface %q; request a supported read surface", value)
	}
	return surface, nil
}

func (s ReadSurface) floor() (int, bool) {
	switch s {
	case ReadSurfaceKnownSessions:
		return knownSessionsReuseFloor, true
	case ReadSurfaceClaudeEvidence:
		return claudeEvidenceReuseFloor, true
	default:
		return 0, false
	}
}

// ScanReuse holds one read-only SQLite snapshot. Only the surfaces requested
// at open may be read. It exposes no store or writer, and owns its connection.
type ScanReuse struct {
	pool     *sqlitex.Pool
	version  int
	current  bool
	surfaces map[ReadSurface]bool
	mu       sync.Mutex
	closed   bool
}

// OpenForScanReuse opens an existing database with SQLITE_OPEN_READONLY and
// query_only. It never migrates or writes database or WAL content. As with any
// ordinary SQLite WAL reader, SQLite may create or update the transient -shm
// coordination file. The version check and all subsequent reads share one
// snapshot, even if another process upgrades the database during discovery.
// The caller must Close the handle. An unknown surface, newer schema, or schema
// below any requested floor is refused.
func OpenForScanReuse(path string, surfaces ...ReadSurface) (*ScanReuse, error) {
	if len(surfaces) == 0 {
		return nil, fmt.Errorf("store: scan reuse requires at least one read surface")
	}
	requested := make(map[ReadSurface]bool, len(surfaces))
	for _, surface := range surfaces {
		if _, ok := surface.floor(); !ok {
			return nil, fmt.Errorf("store: unknown scan reuse surface %q; request a supported read surface", surface)
		}
		requested[surface] = true
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("store: inspect scan reuse database %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("store: scan reuse database %s is not a regular file", path)
	}
	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{
		PoolSize: 1, Flags: sqlite.OpenReadOnly, PrepareConn: prepareReadOnlyConn,
	})
	if err != nil {
		return nil, fmt.Errorf("store: open %s for read-only scan reuse: %w", path, err)
	}
	conn, err := pool.Take(context.Background())
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("store: take scan reuse connection: %w", err)
	}
	fail := func(cause error) (*ScanReuse, error) {
		if !conn.AutocommitEnabled() {
			_ = sqlitex.ExecuteTransient(conn, "ROLLBACK", nil)
		}
		pool.Put(conn)
		_ = pool.Close()
		return nil, cause
	}
	if err := sqlitex.ExecuteTransient(conn, "BEGIN", nil); err != nil {
		return fail(fmt.Errorf("store: begin scan reuse snapshot: %w", err))
	}
	// A schema read establishes the snapshot before the version check. The
	// transaction stays open for every query made through the handle.
	if err := sqlitex.ExecuteTransient(conn, "SELECT name FROM sqlite_master LIMIT 1", nil); err != nil {
		return fail(fmt.Errorf("store: establish scan reuse snapshot: %w", err))
	}
	version, err := readPragmaInt(conn, "PRAGMA user_version")
	if err != nil {
		return fail(fmt.Errorf("store: read scan reuse schema version: %w", err))
	}
	if int(version) > CurrentSchemaVersion() {
		return fail(newerSchemaError(path, int(version)))
	}
	for surface := range requested {
		floor, _ := surface.floor()
		if int(version) < floor {
			return fail(fmt.Errorf("store: scan reuse surface %s needs schema %d or later; %s is at schema %d", surface, floor, path, version))
		}
	}
	current := false
	if int(version) == CurrentSchemaVersion() {
		// A head-version store can still owe a recount. An unreadable marker
		// cannot authorize the separate current-store writer either.
		owed, recountErr := owedRecountsOnConn(conn)
		current = recountErr == nil && len(owed) == 0
	}
	pool.Put(conn)
	return &ScanReuse{pool: pool, version: int(version), current: current, surfaces: requested}, nil
}

// SchemaVersion is the schema version of the pinned snapshot.
func (s *ScanReuse) SchemaVersion() int { return s.version }

// IsCurrent reports whether the snapshot was current with no owed recount.
// A later writer must still recheck inside its own transaction.
func (s *ScanReuse) IsCurrent() bool { return s.current }

// require runs while mu is held, so Close cannot roll back the snapshot
// between this check and the following read. An interrupted SQLite operation
// may roll back its transaction; a later read must fail rather than silently
// use a new snapshot with a different schema.
func (s *ScanReuse) require(ctx context.Context, surface ReadSurface) error {
	if s == nil || !s.surfaces[surface] {
		return fmt.Errorf("store: scan reuse surface %s was not requested when opening the snapshot", surface)
	}
	if s.closed {
		return fmt.Errorf("store: the scan reuse snapshot is closed")
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return err
	}
	active := !conn.AutocommitEnabled()
	s.pool.Put(conn)
	if !active {
		return fmt.Errorf("store: the scan reuse snapshot ended before the read; retry discovery")
	}
	return nil
}

// AllIngestedSessions reads the known-session surface from the snapshot.
func (s *ScanReuse) AllIngestedSessions(ctx context.Context) ([]IngestedSessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(ctx, ReadSurfaceKnownSessions); err != nil {
		return nil, err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: all ingested sessions take connection: %w", err)
	}
	defer s.pool.Put(conn)
	return allIngestedSessionsOnConn(conn)
}

// LoadClaudeEvidenceCompletely reads the complete evidence surface.
func (s *ScanReuse) LoadClaudeEvidenceCompletely(ctx context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(ctx, ReadSurfaceClaudeEvidence); err != nil {
		return nil, false, err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("store.LoadClaudeEvidence: take connection: %w", err)
	}
	defer s.pool.Put(conn)
	return loadClaudeEvidenceOnConn(conn)
}

// LoadClaudeEvidenceForCompletely reads the requested evidence paths.
func (s *ScanReuse) LoadClaudeEvidenceForCompletely(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(ctx, ReadSurfaceClaudeEvidence); err != nil {
		return nil, false, err
	}
	if !hasEvidencePath(paths) {
		return make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence), true, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("store.LoadClaudeEvidenceFor: take connection: %w; what: failed to obtain a database connection for the by-path evidence read; why: the pool is exhausted or the context was cancelled; where: store.LoadClaudeEvidenceFor in internal/store/claude_evidence.go; when: before the first by-path batch ran; impact: no cached transcript evidence was returned, so the caller cannot reuse the cache; fix: check the pool size and context deadline, then retry the scoped read", err)
	}
	defer s.pool.Put(conn)
	return loadClaudeEvidenceForOnConn(conn, paths)
}

// LoadClaudeIdentityChains reads the coordinate lists needed for lineage.
func (s *ScanReuse) LoadClaudeIdentityChains(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath][]ingest.ClaudeIdentityChainLink, []ingest.ResolvedPath, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(ctx, ReadSurfaceClaudeEvidence); err != nil {
		return nil, nil, err
	}
	if !hasEvidencePath(paths) {
		return make(map[ingest.ResolvedPath][]ingest.ClaudeIdentityChainLink), nil, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("store.LoadClaudeIdentityChains: take connection: %w; what: failed to obtain a database connection for the identity-chain read; why: the pool is exhausted or the context was cancelled; where: store.LoadClaudeIdentityChains in internal/store/claude_evidence.go; when: before the first chain batch ran; impact: no coordinate list was returned, so lineage linking cannot reuse the cache; fix: check the pool size and context deadline, then retry", err)
	}
	defer s.pool.Put(conn)
	return loadClaudeIdentityChainsOnConn(conn, paths)
}

// Close releases the pinned snapshot and its connection. It is idempotent.
func (s *ScanReuse) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return err
	}
	if !conn.AutocommitEnabled() {
		err = sqlitex.ExecuteTransient(conn, "ROLLBACK", nil)
	}
	s.pool.Put(conn)
	if closeErr := s.pool.Close(); err == nil {
		err = closeErr
	}
	return err
}
