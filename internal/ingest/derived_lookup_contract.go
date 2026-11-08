package ingest

import "context"

// RolloutFile is one enumerated Codex rollout, handed to the rebuild so it can
// write the rollout catalog on its own path. The catalog is a file catalog, not
// a projection of sessions: a rollout that was never stored still belongs in
// it, so the rebuild takes the enumerated set from its caller instead of
// deriving it from stored rows.
type RolloutFile struct {
	Path      ResolvedPath
	SessionID SessionID
}

// DerivedLookupRebuilder repairs the derived lookup tables from their
// sources. It writes through its own statements, never the discovery
// maintenance write, so a dropped maintenance pass cannot be masked by the
// rebuild: the two paths are able to disagree, which is what makes the
// rebuild a check on derivation rather than a second copy of the same code.
//
// The catalog argument is the COMPLETE enumeration of the source roots being
// rebuilt, including every rollout that was refused or never stored. The
// rebuild replaces the catalog rather than merging into it, so a filtered or
// partial walk deletes the rows for the paths it omits and reopens the
// duplicate-detection gap those rows close. An empty walk is valid and means
// the walk enumerated no rollout.
type DerivedLookupRebuilder interface {
	RebuildDerivedLookups(ctx context.Context, catalog []RolloutFile) error
}
