package discovery

import (
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// StoreKnownSessions maps a session id to what the local store recorded for it.
// It is the KnownSessionIndex a scan uses to skip walking git for a session the
// store already holds and whose source has not changed since.
type StoreKnownSessions map[string]KnownSession

var _ KnownSessionIndex = StoreKnownSessions(nil)

// NewStoreKnownSessions builds the index from the rows the store recorded. No
// rows give a nil index, and a scan then resolves every session the full way.
func NewStoreKnownSessions(rows []store.IngestedSessionRow) StoreKnownSessions {
	if len(rows) == 0 {
		return nil
	}
	known := make(StoreKnownSessions, len(rows))
	for _, row := range rows {
		known[row.SessionID] = KnownSession{
			GitRemote:     row.GitRemote,
			Branch:        row.Branch,
			GitWorktree:   row.GitWorktree,
			CanonicalCwd:  row.CanonicalCwd,
			Title:         row.Title,
			IngestedMs:    row.IngestedMs,
			SchemaVersion: row.SchemaVersion,
		}
	}
	return known
}

// Reusable reports whether a discovered session is already recorded and its
// source has not changed since that ingest, so the recorded values stand in for
// a fresh git resolution. The classification is the ingest pipeline's own diff
// rule: a session whose source moved on is resolved again, because its project
// or branch may have moved with it.
func (k StoreKnownSessions) Reusable(session ingest.DiscoveredSession, staleness time.Duration) (KnownSession, bool) {
	record, ok := k[string(session.SessionID)]
	if !ok {
		return KnownSession{}, false
	}
	ingestedMs := record.IngestedMs
	location := ingest.SessionLocation{IngestedMs: &ingestedMs, SchemaVersion: record.SchemaVersion}
	switch ingest.ClassifyAgainstStore(session, location, staleness) {
	case ingest.DiffUnchanged, ingest.DiffActive:
		return record, true
	}
	return KnownSession{}, false
}
