package main

import (
	"context"
	"os"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/store"
)

// loadKnownSessions reads what the local store already holds so a re-scan can
// skip resolving those sessions again. The database is reused ONLY when it
// exists and its schema version already matches this build: an older file would
// have to be migrated first, and a newer one was written by a build whose
// columns this one does not know. Either way, and on any read problem, it
// returns nil so discovery resolves everything the full way. A slower scan is
// always correct, so a database this build cannot reuse must never fail
// onboarding.
func loadKnownSessions(ctx context.Context, dbPath string) discovery.StoreKnownSessions {
	db := openReusableStore(dbPath)
	if db == nil {
		return nil
	}
	defer db.Close()
	return loadKnownSessionsFrom(ctx, db)
}

// openReusableStore opens the local database ONLY when this build can reuse it
// as it stands. A missing file, an older file, or a file written by a newer
// build all return nil, and a scan then does the full work. The caller owns the
// returned store and must close it.
func openReusableStore(dbPath string) *store.Store {
	if dbPath == "" {
		return nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	version, err := store.SchemaVersionAt(dbPath)
	if err != nil || version != store.CurrentSchemaVersion() {
		return nil
	}
	db, err := store.Open(dbPath, store.WithPoolSize(1))
	if err != nil {
		return nil
	}
	return db
}

// loadKnownSessionsFrom reads the recorded sessions from an already open store.
// Any read problem returns nil, so a scan resolves everything the full way. The
// index is discovery's own, the one the web setup reads, so the setup tree, the
// web import and a harvest reuse a recorded session by one rule.
func loadKnownSessionsFrom(ctx context.Context, db *store.Store) discovery.StoreKnownSessions {
	if db == nil {
		return nil
	}
	rows, err := db.AllIngestedSessions(ctx)
	if err != nil {
		return nil
	}
	return discovery.NewStoreKnownSessions(rows)
}
