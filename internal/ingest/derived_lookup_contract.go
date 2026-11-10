package ingest

import (
	"context"
	"errors"
)

// RolloutFile is one enumerated Codex rollout, handed to the rebuild so it can
// write the rollout catalog on its own path. The catalog is a file catalog, not
// a projection of sessions: a rollout that was never stored still belongs in
// it, so the rebuild takes the enumerated set from its caller instead of
// deriving it from stored rows.
type RolloutFile struct {
	Path      ResolvedPath
	SessionID SessionID
}

// RolloutEnumerationStatus describes the coverage of a discovery walk.
type RolloutEnumerationStatus uint8

const (
	rolloutEnumerationUnset RolloutEnumerationStatus = iota
	// RolloutEnumerationNotWalked indicates that no source root was walked.
	RolloutEnumerationNotWalked
	// RolloutEnumerationPartial indicates that one or more roots were incomplete.
	RolloutEnumerationPartial
	// RolloutEnumerationComplete indicates that every configured root was walked.
	RolloutEnumerationComplete
)

// RolloutEnumeration carries observed files and roots walked to completion.
// Deletes contains only paths confirmed absent beneath a completed root.
// CompletedRoots is used by ingest to calculate Deletes, not by the store.
type RolloutEnumeration struct {
	Status         RolloutEnumerationStatus
	Files          []RolloutFile
	CompletedRoots []ResolvedPath
	Deletes        []ResolvedPath
}

// ErrIncompleteRolloutEnumeration indicates an enumeration with unknown status.
var ErrIncompleteRolloutEnumeration = errors.New("the Codex rollout enumeration is incomplete")

// RolloutEnumerator is implemented by an adapter that can report the rollout
// files its most recent full discovery enumerated and whether that enumeration
// covered every configured root. The adapter factory signature carries only the
// filesystem, the git resolver, and the salt, so a caller asks the adapter after
// the walk — the same attach shape the evidence cache uses.
type RolloutEnumerator interface {
	EnumeratedRollouts() RolloutEnumeration
}

// DerivedLookupRebuilder reconciles the catalog and rebuilds teammate spawns.
type DerivedLookupRebuilder interface {
	RebuildDerivedLookups(ctx context.Context, enumeration RolloutEnumeration) error
}
