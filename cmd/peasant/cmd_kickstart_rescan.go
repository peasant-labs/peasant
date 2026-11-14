package main

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// loadKnownSessions reads what the local store already holds so a re-scan can
// skip resolving those sessions again. An incompatible or unreadable database
// returns nil so discovery resolves everything the full way.
func loadKnownSessions(ctx context.Context, dbPath string) discovery.StoreKnownSessions {
	db := openReusableStore(dbPath)
	if db == nil {
		return nil
	}
	defer db.Close()
	return loadKnownSessionsFrom(ctx, db)
}

// openReusableStore opens only the two compatible read surfaces. Any refusal
// falls back to a cold scan. The caller owns and closes the pinned snapshot.
func openReusableStore(dbPath string) *store.ScanReuse {
	if dbPath == "" {
		return nil
	}
	db, err := store.OpenForScanReuse(dbPath, store.ReadSurfaceKnownSessions, store.ReadSurfaceClaudeEvidence)
	if err != nil {
		return nil
	}
	return db
}

// loadKnownSessionsFrom reads the recorded sessions from an already open store.
// Any read problem returns nil, so a scan resolves everything the full way. The
// index is discovery's own, the one the web setup reads, so the setup tree, the
// web import and a harvest reuse a recorded session by one rule.
func loadKnownSessionsFrom(ctx context.Context, db interface {
	AllIngestedSessions(context.Context) ([]store.IngestedSessionRow, error)
}) discovery.StoreKnownSessions {
	if db == nil {
		return nil
	}
	rows, err := db.AllIngestedSessions(ctx)
	if err != nil {
		return nil
	}
	return discovery.NewStoreKnownSessions(rows)
}

// scanEvidenceCache combines a query-only snapshot with an optional separate
// current-store writer. A behind store has no writer, not a pretend success.
type scanEvidenceCache struct {
	reader    *store.ScanReuse
	writePath string
}

var _ ingest.ClaudeEvidenceCache = (*scanEvidenceCache)(nil)
var _ ingest.ClaudeEvidenceCompleteness = (*scanEvidenceCache)(nil)
var _ ingest.ClaudeIdentityChainReader = (*scanEvidenceCache)(nil)
var _ ingest.ClaudeEvidenceReadOnly = (*scanEvidenceCache)(nil)

func (c *scanEvidenceCache) ClaudeEvidenceReadOnly() bool { return c.writePath == "" }

func (c *scanEvidenceCache) LoadClaudeEvidence(ctx context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	records, _, err := c.LoadClaudeEvidenceCompletely(ctx)
	return records, err
}

func (c *scanEvidenceCache) LoadClaudeEvidenceCompletely(ctx context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	return c.reader.LoadClaudeEvidenceCompletely(ctx)
}

func (c *scanEvidenceCache) LoadClaudeEvidenceFor(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	records, _, err := c.LoadClaudeEvidenceForCompletely(ctx, paths)
	return records, err
}

func (c *scanEvidenceCache) LoadClaudeEvidenceForCompletely(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	return c.reader.LoadClaudeEvidenceForCompletely(ctx, paths)
}

func (c *scanEvidenceCache) LoadClaudeIdentityChains(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath][]ingest.ClaudeIdentityChainLink, []ingest.ResolvedPath, error) {
	return c.reader.LoadClaudeIdentityChains(ctx, paths)
}

func (c *scanEvidenceCache) SaveClaudeEvidence(ctx context.Context, upserts []ingest.ClaudeTranscriptEvidence, deletes []ingest.ResolvedPath) error {
	if c.writePath == "" {
		return fmt.Errorf("store: a behind-store scan cannot write Claude evidence")
	}
	return store.SaveClaudeEvidenceIfCurrent(ctx, c.writePath, upserts, deletes)
}
