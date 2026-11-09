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

// RolloutEnumerationStatus says what one Codex discovery can claim about the
// rollout files it enumerated.
//
// The ZERO VALUE IS INVALID and a rebuild refuses it: a caller that never asked
// cannot claim "nothing to enumerate", because replacing a catalog from silence
// clears rows no source produced. Build one of the declared values.
type RolloutEnumerationStatus uint8

const (
	rolloutEnumerationUnset RolloutEnumerationStatus = iota
	// RolloutEnumerationNotWalked says this run configured no Codex source, so
	// nothing was enumerated. A rebuild has nothing to derive the catalog from
	// and leaves it exactly as it is.
	RolloutEnumerationNotWalked
	// RolloutEnumerationPartial says a Codex walk ran and swallowed at least
	// one directory-read or file-stat FAILURE that could hide rollouts, so a
	// directory or file the walk could not read may hold rollouts that are
	// still on disk. A rebuild refuses rather than clearing rows it could not
	// re-derive. A configured root that is not installed is NOT this case: it
	// holds nothing, so it reports no path and stays complete.
	RolloutEnumerationPartial
	// RolloutEnumerationComplete says every configured root was walked and every
	// rollout under it was enumerated, so the set is the whole truth for those
	// roots. A configured root whose path does not exist is complete: it
	// contributes no path, and a rollout an earlier run recorded under it is
	// pruned for the same reason the walk's maintenance prunes it — its file is
	// gone.
	RolloutEnumerationComplete
)

// RolloutEnumeration is the rollout-file set one Codex discovery enumerated,
// together with what the walk can claim about its own coverage.
//
// It is the ONE input the derived rollout catalog is both maintained and
// rebuilt from: the full walk writes it, `harvest index --all` replaces the
// table with it, and a caller cannot substitute the discovered sessions for it,
// because a session list is neither complete for a partial walk nor complete
// for rollout files that never became sessions.
type RolloutEnumeration struct {
	Status RolloutEnumerationStatus
	Files  []RolloutFile
}

// ErrIncompleteRolloutEnumeration is the refusal a rebuild returns when the
// enumeration it was given may be short. The caller renders it; the store
// writes nothing.
var ErrIncompleteRolloutEnumeration = errors.New("the Codex rollout enumeration is incomplete")

// RolloutEnumerator is implemented by an adapter that can report the rollout
// files its most recent full discovery enumerated and whether that enumeration
// covered every configured root. The adapter factory signature carries only the
// filesystem, the git resolver, and the salt, so a caller asks the adapter after
// the walk — the same attach shape the evidence cache uses.
type RolloutEnumerator interface {
	EnumeratedRollouts() RolloutEnumeration
}

// DerivedLookupRebuilder repairs the derived lookup tables from their
// sources. It writes through its own statements, never the discovery
// maintenance write, so a dropped maintenance pass cannot be masked by the
// rebuild: the two paths are able to disagree, which is what makes the
// rebuild a check on derivation rather than a second copy of the same code.
//
// The enumeration is the COMPLETE set of rollout paths under every configured
// root this run walked, including every rollout that was refused or never
// stored. The rebuild replaces the catalog rather than merging into it, so an
// incomplete set would delete rows for the paths it omits and reopen the
// duplicate-detection gap those rows close; the implementation therefore
// refuses a partial or unset enumeration and writes nothing. A run with no
// Codex source at all reports RolloutEnumerationNotWalked, which leaves the
// catalog untouched while the teammate spawns are still re-derived.
type DerivedLookupRebuilder interface {
	RebuildDerivedLookups(ctx context.Context, enumeration RolloutEnumeration) error
}
