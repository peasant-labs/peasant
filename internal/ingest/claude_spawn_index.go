package ingest

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// claudeSpawnLink names one candidate teammate relationship: the root that
// declared the identity (the child) and the root that recorded spawning it
// (the parent). It is only ever produced for an identity that is unambiguous
// in both directions.
type claudeSpawnLink struct {
	Child  ResolvedPath
	Parent ResolvedPath
}

// buildClaudeSpawnIndex derives the identity-to-spawner pairing that is safe
// to trust from every piece of root-transcript evidence the caller passes in.
// A pairing is included only when exactly one root declares the identity and
// exactly one (different) root recorded spawning it. An identity claimed by
// more than one root, or spawned by more than one root, is OMITTED entirely:
// a wrong parent would silently re-home a person's session under someone
// else's, so ambiguity is worse than no link.
//
// The caller decides what "every piece of evidence" means. Passing the
// current run's mined records alone reproduces same-batch linking; passing a
// merge of the persisted evidence cache with this run's mined records (mined
// values winning on overlap) extends linking so a child discovered in a
// later run can still find a spawner an earlier run discovered.
func buildClaudeSpawnIndex(evidence map[ResolvedPath]ClaudeTranscriptEvidence) map[ClaudeTeammateIdentity]claudeSpawnLink {
	declarers := make(map[ClaudeTeammateIdentity]map[ResolvedPath]struct{})
	spawners := make(map[ClaudeTeammateIdentity]map[ResolvedPath]struct{})

	for path, record := range evidence {
		if record.Scope != ClaudeEvidenceScopeRoot {
			continue // only a root transcript ever carries an identity or a spawn
		}
		if record.Identity != nil {
			identity := *record.Identity
			if declarers[identity] == nil {
				declarers[identity] = make(map[ResolvedPath]struct{})
			}
			declarers[identity][path] = struct{}{}
		}
		for _, spawn := range record.Spawns {
			if spawners[spawn] == nil {
				spawners[spawn] = make(map[ResolvedPath]struct{})
			}
			spawners[spawn][path] = struct{}{}
		}
	}

	index := make(map[ClaudeTeammateIdentity]claudeSpawnLink)
	for identity, children := range declarers {
		parents := spawners[identity]
		if len(children) != 1 || len(parents) != 1 {
			continue // claimed or spawned more than once: stays absent
		}
		var child, parent ResolvedPath
		for c := range children {
			child = c
		}
		for p := range parents {
			parent = p
		}
		if child == parent {
			continue // a root cannot spawn itself
		}
		index[identity] = claudeSpawnLink{Child: child, Parent: parent}
	}
	return index
}

// mergeClaudeEvidence returns the effective evidence view for one discovery:
// every persisted record, with this run's freshly mined records overlaid on
// top so a stale cached row for a path this run re-mined never wins over
// what this run just read.
func mergeClaudeEvidence(cached, mined map[ResolvedPath]ClaudeTranscriptEvidence) map[ResolvedPath]ClaudeTranscriptEvidence {
	merged := make(map[ResolvedPath]ClaudeTranscriptEvidence, len(cached)+len(mined))
	for path, record := range cached {
		merged[path] = record
	}
	for path, record := range mined {
		merged[path] = record
	}
	return merged
}

// claudeSessionIDFromRootPath recovers the session id a root transcript path
// encodes: {project-slug}/{uuid}.jsonl. It is the same shape Discover already
// validates when it first collects root entries, so a path that fails this
// parse cannot have produced a discovered session either, in this run or any
// other.
func claudeSessionIDFromRootPath(path ResolvedPath) (SessionID, bool) {
	base := filepath.Base(path.String())
	idStr := strings.TrimSuffix(base, defaults.ExtJSONL.String())
	id, err := NewSessionID(idStr)
	if err != nil {
		return SessionID(""), false
	}
	return id, true
}

// claudeStoredSessionLookup is satisfied by an evidence cache that can also
// answer whether a session id is already persisted. The production cache is
// the local store, which already exposes this exact lookup for an unrelated
// reason (pre-populating the diff-stage location cache); reusing it here lets
// cross-run linking confirm a persisted-only candidate parent is real before
// pointing a child at it, without adding a new store dependency to the
// adapter. A cache that offers no such lookup simply cannot confirm a
// cross-run parent, so cross-run linking never fires for it.
type claudeStoredSessionLookup interface {
	LookupSessionLocation(ctx context.Context, sessionID SessionID) (hostSlug string, parentID string, err error)
}

// claudeSessionAlreadyStored reports whether sessionID is already recorded in
// the store from a prior discovery. It fails closed: any error, or a cache
// that offers no such lookup, answers false, because a parent identifier must
// never point at a session that is neither in this write batch nor already
// stored — the store's own FK-orphan guard would otherwise silently drop the
// child at write time.
func (a *ClaudeAdapter) claudeSessionAlreadyStored(ctx context.Context, sessionID SessionID) bool {
	lookup, ok := a.evidence.(claudeStoredSessionLookup)
	if !ok {
		return false
	}
	hostSlug, _, err := lookup.LookupSessionLocation(ctx, sessionID)
	return err == nil && hostSlug != ""
}

// claudeStoredParent returns the parent id the store already recorded for
// sessionID, if any, so cycle detection can keep walking an ancestor chain
// past this discovery's own write batch. It fails closed: any error or an
// absent parent answers no parent found.
func (a *ClaudeAdapter) claudeStoredParent(ctx context.Context, sessionID SessionID) (SessionID, bool) {
	lookup, ok := a.evidence.(claudeStoredSessionLookup)
	if !ok {
		return SessionID(""), false
	}
	_, parentID, err := lookup.LookupSessionLocation(ctx, sessionID)
	if err != nil || parentID == "" {
		return SessionID(""), false
	}
	return SessionID(parentID), true
}

// claudeCyclicChildren returns which candidate assignments would place a
// session among its own ancestors, so linking can refuse exactly those and
// nothing else. Walking follows this run's own candidate edges first, then
// falls back to the store's already-persisted parent chain, because a cycle
// can just as easily complete through data this run never touched. The
// parent column is a self-referencing foreign key: a cycle is a corrupt
// store, not a cosmetic problem, so an ambiguous or cyclic outcome is refused
// rather than guessed.
func (a *ClaudeAdapter) claudeCyclicChildren(ctx context.Context, candidates map[SessionID]SessionID) map[SessionID]bool {
	cyclic := make(map[SessionID]bool)
	for child, parent := range candidates {
		seen := map[SessionID]bool{child: true}
		current := parent
		for {
			if current == child {
				cyclic[child] = true
				break
			}
			if seen[current] {
				break // a cycle exists upstream, but not one that loops back to child
			}
			seen[current] = true
			next, ok := candidates[current]
			if !ok {
				next, ok = a.claudeStoredParent(ctx, current)
			}
			if !ok {
				break
			}
			current = next
		}
	}
	return cyclic
}
