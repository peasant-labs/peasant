package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

// DefaultPoolSize is the SQLite connection-pool size used when neither
// WithPoolSize nor the EnvPoolSize override is set. The web server
// (internal/api) needs concurrent connections; one-shot CLI commands open this
// many too, but the cost is amortized over the process lifetime.
const DefaultPoolSize = 10

// EnvPoolSize overrides the pool size. Each Open creates PoolSize connections
// up front, and every connection re-parses the schema + runs the PRAGMAs — so
// the test suite (which needs a single connection per Open across hundreds of
// Opens) sets this to "1" to avoid opening DefaultPoolSize connections each
// time. Mirrors the village backend's POOL_MAX_CONNS env knob. An explicit
// WithPoolSize option takes precedence over this.
const EnvPoolSize = "PEASANT_DB_POOL_SIZE"

// resolvePoolSize picks the pool size: an explicit WithPoolSize option wins,
// then the EnvPoolSize override (if a positive integer), else DefaultPoolSize.
func resolvePoolSize(optPoolSize int) int {
	if optPoolSize > 0 {
		return optPoolSize
	}
	if v := os.Getenv(EnvPoolSize); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultPoolSize
}

// AllTableNamesFromDB retrieves the list of tables from the database.
func AllTableNamesFromDB(s *Store) ([]string, error) {
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)

	var tables []string
	stmt, err := conn.Prepare("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name;")
	if err != nil {
		return nil, err
	}
	defer stmt.Finalize()

	for {
		if hasRow, err := stmt.Step(); err != nil {
			return nil, err
		} else if !hasRow {
			break
		}
		name := stmt.GetText("name")
		tables = append(tables, name)
	}
	return tables, nil
}

// TableRowCount returns the number of rows in a table.
func TableRowCount(s *Store, tableName string) (int64, error) {
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return 0, err
	}
	defer s.pool.Put(conn)

	stmt, err := conn.Prepare("SELECT COUNT(*) FROM " + tableName)
	if err != nil {
		return 0, err
	}
	defer stmt.Finalize()

	if hasRow, err := stmt.Step(); err != nil {
		return 0, err
	} else if !hasRow {
		return 0, nil
	}
	return stmt.GetInt64("COUNT(*)"), nil
}

// TableSelectLimit returns sample rows from a table as formatted strings.
// It limits to the first N rows and shows key columns only.
func TableSelectLimit(s *Store, tableName string, limit int) ([]string, error) {
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)

	// Get column info for the table
	colStmt, err := conn.Prepare("PRAGMA table_info(" + tableName + ")")
	if err != nil {
		return nil, err
	}
	defer colStmt.Finalize()

	var colNames []string
	for {
		if hasRow, err := colStmt.Step(); err != nil {
			return nil, err
		} else if !hasRow {
			break
		}
		colNames = append(colNames, colStmt.GetText("name"))
	}

	// Limit to first 5 columns for readability
	if len(colNames) > 5 {
		colNames = colNames[:5]
	}

	// Build query for sample data
	cols := strings.Join(colNames, ", ")
	query := "SELECT " + cols + " FROM " + tableName + " LIMIT " + fmt.Sprintf("%d", limit)

	stmt, err := conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Finalize()

	var results []string
	for {
		if hasRow, err := stmt.Step(); err != nil {
			return nil, err
		} else if !hasRow {
			break
		}

		var parts []string
		for _, col := range colNames {
			val := stmt.GetText(col)
			if val == "" {
				val = "(empty)"
			} else if len(val) > 30 {
				val = val[:27] + "..."
			}
			parts = append(parts, val)
		}
		results = append(results, strings.Join(parts, " | "))
	}

	return results, nil
}

// TaxonomyChainBadCount returns how many annotation_types lack a valid family→class FK chain.
// A return value of 0 means the taxonomy chain is intact.
func (s *Store) TaxonomyChainBadCount(ctx context.Context) (int64, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, err
	}
	defer s.pool.Put(conn)

	var bad int64
	err = sqlitex.ExecuteTransient(conn,
		`SELECT COUNT(*) FROM annotation_types t
LEFT JOIN annotation_families f ON f.id = t.family_id
LEFT JOIN annotation_classes c ON c.id = f.class_id
WHERE f.id IS NULL OR c.id IS NULL`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				bad = stmt.ColumnInt64(0)
				return nil
			},
		})
	return bad, err
}

// AnnotationsByTargetKind returns annotation counts from annotations_with_target VIEW
// grouped by target_kind. Map key is the target_kind; value is the count.
// Returns an empty map if no annotations exist (normal for a fresh database).
func (s *Store) AnnotationsByTargetKind(ctx context.Context) (map[string]int64, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)

	result := make(map[string]int64)
	err = sqlitex.ExecuteTransient(conn,
		`SELECT target_kind, COUNT(*) FROM annotations_with_target GROUP BY target_kind`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				result[stmt.ColumnText(0)] = stmt.ColumnInt64(1)
				return nil
			},
		})
	return result, err
}

// AnnotationTypeDetail holds one annotation_types row for verbose verify display.
type AnnotationTypeDetail struct {
	TypeID string
	Status string
	Family string
	Class  string
}

// AnnotationTypeDetails returns all annotation types with their family and class,
// ordered by type_id. Used by runVerify --verbose.
func (s *Store) AnnotationTypeDetails(ctx context.Context) ([]AnnotationTypeDetail, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)

	var rows []AnnotationTypeDetail
	err = sqlitex.ExecuteTransient(conn,
		`SELECT t.type_id, s.name, f.family, c.class
FROM annotation_types t
JOIN annotation_statuses s ON s.id = t.status_id
JOIN annotation_families f ON f.id = t.family_id
JOIN annotation_classes c ON c.id = f.class_id
ORDER BY t.type_id`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows = append(rows, AnnotationTypeDetail{
					TypeID: stmt.ColumnText(0),
					Status: stmt.ColumnText(1),
					Family: stmt.ColumnText(2),
					Class:  stmt.ColumnText(3),
				})
				return nil
			},
		})
	return rows, err
}

// Compile-time interface guards.
var (
	_ ingest.SessionStore                   = (*Store)(nil)
	_ ingest.MetricsStore                   = (*Store)(nil)
	_ ingest.AnnotationRunStateStore        = (*Store)(nil)
	_ ingest.ClassifierAnnotationBatchStore = (*Store)(nil)
	_ ingest.PruneStore                     = (*Store)(nil)
)

// Store manages persistent storage of sessions and metrics via SQLite.
// It holds a connection pool, the installation salt (for opaque ID computation),
// and manages the database lifecycle.
type Store struct {
	pool              *sqlitex.Pool
	salt              salt.Salt
	annotationWriteMu sync.Mutex
	closed            atomic.Bool
}

// InstallationSalt returns the salt used by ingestion to derive canonical,
// installation-local project identities.
func (s *Store) InstallationSalt() salt.Salt {
	return s.salt
}

// pragmas are per-connection SQLite settings applied via PrepareConn.
var pragmas = []string{
	"PRAGMA journal_mode = WAL",
	"PRAGMA synchronous = NORMAL",
	"PRAGMA temp_store = MEMORY",
	"PRAGMA mmap_size = 268435456",
	"PRAGMA cache_size = -64000",
	"PRAGMA foreign_keys = ON",
	"PRAGMA busy_timeout = 5000",
}

// OpenOption configures Open behavior.
type OpenOption func(*openOptions)

type openOptions struct {
	migrationConsent MigrationConsent
	poolSize         int
	skipMigrations   bool
}

// WithSkipMigrations skips the migration-state check on Open. The caller MUST
// guarantee the database is already at the current schema (e.g. a copy of a
// freshly-migrated golden DB in tests). Using it on an unmigrated or stale DB
// will surface as missing-table / missing-column errors at query time. Intended
// for test helpers that open many copies of an already-migrated template.
func WithSkipMigrations() OpenOption {
	return func(o *openOptions) { o.skipMigrations = true }
}

// WithPoolSize sets the SQLite connection-pool size for this Open. Use 1 for
// single-threaded callers (e.g. tests, one-shot CLI commands) to avoid opening
// DefaultPoolSize connections — each of which re-parses the schema and runs the
// PRAGMAs. A value <= 0 falls back to the EnvPoolSize override / DefaultPoolSize.
func WithPoolSize(n int) OpenOption {
	return func(o *openOptions) { o.poolSize = n }
}

// WithMigrationConsent supplies a callback that gates running the V33 harness
// rename migration on a database that already contains session data. The
// callback may prompt the user. If nil or omitted, the migration runs without
// confirmation (legacy behavior; appropriate for fresh DBs and tests).
func WithMigrationConsent(consent MigrationConsent) OpenOption {
	return func(o *openOptions) { o.migrationConsent = consent }
}

// Open creates or opens the SQLite database at dbPath, applies migrations,
// and configures PRAGMAs. The caller must call Close when done.
func Open(dbPath string, opts ...OpenOption) (*Store, error) {
	o := openOptions{}
	for _, opt := range opts {
		opt(&o)
	}

	// V33 is a breaking migration (renames harness identifiers in sessions).
	// If a consent callback was supplied AND the DB already has data,
	// invoke it and create a backup before opening for write.
	if err := maybeRunV33Consent(dbPath, o.migrationConsent); err != nil {
		return nil, err
	}

	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{
		PoolSize:    resolvePoolSize(o.poolSize),
		PrepareConn: preparePragmas,
	})
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}

	// Apply migrations on a single connection — unless the caller guarantees the
	// DB is already at the current schema (WithSkipMigrations). Skipping avoids
	// the per-Open migration-state re-check (~16% of store.Open CPU), which is
	// pure waste for a copy of a freshly-migrated golden DB in tests.
	if !o.skipMigrations {
		conn, err := pool.Take(context.Background())
		if err != nil {
			_ = pool.Close()
			return nil, fmt.Errorf("store: take connection for migration: %w", err)
		}
		if err := sqlitemigration.Migrate(context.Background(), conn, dbSchema); err != nil {
			pool.Put(conn)
			_ = pool.Close()
			return nil, fmt.Errorf("store: run migrations: %w", err)
		}
		pool.Put(conn)

		// V23 Go-driven phase: populate v2 tables with HMAC-SHA256 opaque IDs,
		// drop old tables, and rename v2 → final names.
		// This is idempotent: applyV23DataMigration checks migration state before running.
		if err := applyV23DataMigration(pool); err != nil {
			_ = pool.Close()
			return nil, fmt.Errorf("store: apply V23 data migration: %w", err)
		}
	}

	// Load the installation salt. Required by InsertSessions to compute opaque_host_id.
	// salt.Load is idempotent and creates the salt on first call.
	s, _, err := salt.Load(pool)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("store: load installation salt: %w", err)
	}

	return &Store{pool: pool, salt: s}, nil
}

// readUserVersion returns the PRAGMA user_version value from the pool.
func readUserVersion(pool *sqlitex.Pool) (int, error) {
	conn, err := pool.Take(context.Background())
	if err != nil {
		return 0, err
	}
	defer pool.Put(conn)
	var version int
	err = sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			version = int(stmt.ColumnInt64(0))
			return nil
		},
	})
	return version, err
}

// preparePragmas sets per-connection PRAGMAs for optimal performance.
func preparePragmas(conn *sqlite.Conn) error {
	for _, pragma := range pragmas {
		if err := sqlitex.ExecuteTransient(conn, pragma+";", nil); err != nil {
			return fmt.Errorf("store: set pragma: %w", err)
		}
	}
	return nil
}

// Close releases the connection pool. Safe to call multiple times.
// Pool returns the underlying SQLite connection pool.
// Used by packages that need direct pool access (e.g. salt.Load).
func (s *Store) Pool() *sqlitex.Pool {
	return s.pool
}

// Close releases the connection pool. It is idempotent, and it deliberately
// leaves the pool pointer in place: a background reader that loses the
// shutdown race then receives a "pool closed" error from Pool.Take instead of
// dereferencing nil. Writing the field here would also be an unsynchronized
// write racing those readers.
func (s *Store) Close() error {
	if s.pool == nil || !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	return s.pool.Close()
}
