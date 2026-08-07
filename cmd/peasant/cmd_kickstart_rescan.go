package main

import (
	"context"
	"os"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// knownSession is what the local store already recorded for one ingested
// session: the values a re-scan reuses instead of walking git for them again,
// plus the two fields the diff rule classifies the discovered source against.
type knownSession struct {
	GitRemote     string
	Branch        string
	Title         string
	IngestedMs    int64
	SchemaVersion int
}

// knownSessionIndex maps a session id to what the store recorded for it. A nil
// index means no reusable database was available, and every discovered session
// is then resolved the full way.
type knownSessionIndex map[string]knownSession

// reusable reports whether a discovered session is already recorded AND its
// source has not changed since that ingest, so the recorded values stand in for
// a fresh git resolution. Classification is the pipeline's own diff rule
// (ingest.ClassifyAgainstStore), never a second copy of it: a session whose
// source moved on is resolved again, because its project or branch may have
// moved with it.
func (k knownSessionIndex) reusable(session ingest.DiscoveredSession, staleness time.Duration) (knownSession, bool) {
	record, ok := k[string(session.SessionID)]
	if !ok {
		return knownSession{}, false
	}
	ingestedMs := record.IngestedMs
	location := ingest.SessionLocation{IngestedMs: &ingestedMs, SchemaVersion: record.SchemaVersion}
	switch ingest.ClassifyAgainstStore(session, location, staleness) {
	case ingest.DiffUnchanged, ingest.DiffActive:
		// Unchanged, or still being written and so not re-ingested this run.
		return record, true
	}
	return knownSession{}, false
}

// loadKnownSessions reads what the local store already holds so a re-scan can
// skip resolving those sessions again. The database is reused ONLY when it
// exists and its schema version already matches this build: an older file would
// have to be migrated first, and a newer one was written by a build whose
// columns this one does not know. Either way, and on any read problem, it
// returns nil so discovery resolves everything the full way. A slower scan is
// always correct, so a database this build cannot reuse must never fail
// onboarding.
func loadKnownSessions(ctx context.Context, dbPath string) knownSessionIndex {
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
	// The version check above is the guarantee WithSkipMigrations requires.
	db, err := store.Open(dbPath, store.WithPoolSize(1), store.WithSkipMigrations())
	if err != nil {
		return nil
	}
	defer db.Close()

	rows, err := db.AllIngestedSessions(ctx)
	if err != nil {
		return nil
	}
	known := make(knownSessionIndex, len(rows))
	for _, row := range rows {
		known[row.SessionID] = knownSession{
			GitRemote:     row.GitRemote,
			Branch:        row.Branch,
			Title:         row.Title,
			IngestedMs:    row.IngestedMs,
			SchemaVersion: row.SchemaVersion,
		}
	}
	return known
}

// configStaleness returns the window inside which a source file counts as still
// being written, from config with the packaged default as the fallback. Ingest
// and the re-scan read it the same way so they agree on which sessions are
// active.
func configStaleness(cfg *config.Config) time.Duration {
	staleness := time.Duration(cfg.Output.StalenessThresholdSec) * time.Second
	if staleness == 0 {
		staleness = time.Duration(defaults.ConfigStalenessThresholdSec) * time.Second
	}
	return staleness
}
