package store

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed baseline_schema.sql
var baselineSchemaSQL string

//go:generate go run ../../scripts/store-baseline-gen baseline_schema.sql

// GenerateSchemaBaselineSQL builds a scratch database by replaying the shipped
// migration chain (including the Go-driven V23 phase), then serializes it under
// the canonical contract. The result is the committed baseline_schema.sql. The
// builder is chain-only and salt-free: runtime objects such as _install_salt
// stay owned by Open.
func GenerateSchemaBaselineSQL() ([]byte, error) {
	dir, err := os.MkdirTemp("", "peasant-baseline-gen-*")
	if err != nil {
		return nil, fmt.Errorf("store: create scratch dir for the baseline generator: %w", err)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "chain.db")
	if err := buildChainDatabase(dbPath); err != nil {
		return nil, err
	}
	conn, err := sqlite.OpenConn(dbPath, 0)
	if err != nil {
		return nil, fmt.Errorf("store: reopen the chain-built scratch database: %w", err)
	}
	defer conn.Close()
	return serializeBaselineSQL(conn)
}

// buildChainDatabase materializes the shipped migration chain, including the
// Go-driven V23 data migration, at dbPath.
func buildChainDatabase(dbPath string) error {
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
	if err != nil {
		return fmt.Errorf("store: open scratch pool for the migration chain: %w", err)
	}
	defer pool.Close()

	conn, err := pool.Take(context.Background())
	if err != nil {
		return fmt.Errorf("store: take scratch connection for the migration chain: %w", err)
	}
	if err := refuseUnmappableCaptureFormats(conn); err != nil {
		pool.Put(conn)
		return fmt.Errorf("store: check capture formats on the scratch database: %w", err)
	}
	if err := sqlitemigration.Migrate(context.Background(), conn, dbSchema); err != nil {
		pool.Put(conn)
		return fmt.Errorf("store: run the migration chain on the scratch database: %w", err)
	}
	pool.Put(conn)
	if err := applyV23DataMigration(pool); err != nil {
		return fmt.Errorf("store: apply the V23 data migration on the scratch database: %w", err)
	}
	return nil
}
