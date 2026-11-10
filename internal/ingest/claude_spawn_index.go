package ingest

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// claudeIdentityCountDelta is how far one build's current evidence view moves
// each identity count away from the stored count. The store's counts describe
// the rows the last discovery saved; a transcript this run re-mined replaces
// its stored row, and a new transcript adds one, so the ambiguity decision has
// to add the difference to the stored count.
type claudeIdentityCountDelta struct {
	declarers map[ClaudeTeammateIdentity]int
	spawners  map[ClaudeTeammateIdentity]int
}

// declarer returns the identity's declarer-count adjustment.
func (d claudeIdentityCountDelta) declarer(id ClaudeTeammateIdentity) int { return d.declarers[id] }

// spawner returns the identity's spawner-count adjustment.
func (d claudeIdentityCountDelta) spawner(id ClaudeTeammateIdentity) int { return d.spawners[id] }

// claudeIdentityDeltas computes the per-identity difference between the stored
// evidence rows and this build's current view.
//
// Only the paths this build holds current evidence for can differ from the
// stored table. For each of them, the stored row loses its contribution and the
// current row adds its own, so an unchanged row cancels out and a changed row is
// replaced rather than counted twice. A stored row this build did not touch — a
// transcript it did not walk, or one whose file is gone — keeps its stored
// contribution.
//
// The stored spawn side comes from storedSpawns, the store's claude_teammate
// projection read by path, not from the loaded evidence cache. That keeps the
// subtraction exact when a transcript's spawns_json cannot be parsed (the row
// would otherwise contribute zero and the link would be dropped) and when the
// cache did not hold the row at all.
func claudeIdentityDeltas(
	cached, current map[ResolvedPath]ClaudeTranscriptEvidence,
	storedSpawns map[ResolvedPath][]ClaudeTeammateIdentity,
) claudeIdentityCountDelta {
	delta := claudeIdentityCountDelta{
		declarers: make(map[ClaudeTeammateIdentity]int),
		spawners:  make(map[ClaudeTeammateIdentity]int),
	}
	for path, record := range current {
		if stored, ok := cached[path]; ok {
			if stored.Scope == ClaudeEvidenceScopeRoot && stored.Identity != nil {
				delta.declarers[*stored.Identity]--
			}
		}
		// The stored spawn side is the derived projection, independent of
		// whether the evidence row's spawns_json parsed.
		seenStored := make(map[ClaudeTeammateIdentity]struct{}, len(storedSpawns[path]))
		for _, spawn := range storedSpawns[path] {
			if _, duplicate := seenStored[spawn]; duplicate {
				continue
			}
			seenStored[spawn] = struct{}{}
			delta.spawners[spawn]--
		}
		if record.Scope != ClaudeEvidenceScopeRoot {
			continue
		}
		if record.Identity != nil {
			delta.declarers[*record.Identity]++
		}
		seen := make(map[ClaudeTeammateIdentity]struct{}, len(record.Spawns))
		for _, spawn := range record.Spawns {
			if _, duplicate := seen[spawn]; duplicate {
				continue
			}
			seen[spawn] = struct{}{}
			delta.spawners[spawn]++
		}
	}
	return delta
}

// claudeEvidenceIdentityReader is the DerivedLookupReader of a run that keeps
// no store — a logs-only harvest, a dry run, or an adapter test. The derived
// tables are a projection of the evidence table, so the evidence view this run
// already holds IS that projection; reading it directly answers the same two
// questions the store's indexes answer. The ambiguity rule itself lives in one
// place (linkClaudeTeammates), so this is a data source, never a second rule.
//
// The counts and paths are computed once at construction, so the per-identity
// questions are lookups: a build that asks about every declarer must not walk
// the whole view once per identity.
type claudeEvidenceIdentityReader struct {
	declarers map[ClaudeTeammateIdentity]int
	spawners  map[ClaudeTeammateIdentity]int
	paths     map[ClaudeTeammateIdentity][]string
}

var _ DerivedLookupReader = claudeEvidenceIdentityReader{}

// newClaudeEvidenceIdentityReader projects the evidence view once.
func newClaudeEvidenceIdentityReader(evidence map[ResolvedPath]ClaudeTranscriptEvidence) claudeEvidenceIdentityReader {
	reader := claudeEvidenceIdentityReader{
		declarers: make(map[ClaudeTeammateIdentity]int),
		spawners:  make(map[ClaudeTeammateIdentity]int),
		paths:     make(map[ClaudeTeammateIdentity][]string),
	}
	for path, record := range evidence {
		if record.Scope != ClaudeEvidenceScopeRoot {
			continue
		}
		if record.Identity != nil {
			reader.declarers[*record.Identity]++
		}
		// One transcript's repeated spawn of the same identity is still one
		// spawner path, exactly as the derived table's primary key makes it.
		seen := make(map[ClaudeTeammateIdentity]struct{}, len(record.Spawns))
		for _, spawn := range record.Spawns {
			if _, duplicate := seen[spawn]; duplicate {
				continue
			}
			seen[spawn] = struct{}{}
			reader.spawners[spawn]++
			if len(reader.paths[spawn]) < 2 {
				reader.paths[spawn] = append(reader.paths[spawn], path.String())
			}
		}
	}
	return reader
}

// CountClaudeIdentityDeclarers counts the distinct root paths that declare id.
func (r claudeEvidenceIdentityReader) CountClaudeIdentityDeclarers(_ context.Context, id ClaudeTeammateIdentity) (int, error) {
	return r.declarers[id], nil
}

// CountClaudeIdentitySpawners counts the distinct root paths that record
// spawning id.
func (r claudeEvidenceIdentityReader) CountClaudeIdentitySpawners(_ context.Context, id ClaudeTeammateIdentity) (int, error) {
	return r.spawners[id], nil
}

// ClaudeIdentitySpawnerPaths returns the root paths that record spawning
// (team, name). It is bounded like the store's read: the caller only
// distinguishes zero, one, and many.
func (r claudeEvidenceIdentityReader) ClaudeIdentitySpawnerPaths(_ context.Context, team, name string) ([]string, error) {
	return r.paths[ClaudeTeammateIdentity{Team: team, Name: name}], nil
}

// CountCodexRolloutsBySession is unused by the Claude link step. A store-free
// run answers no Codex question through the evidence view.
func (claudeEvidenceIdentityReader) CountCodexRolloutsBySession(context.Context, SessionID) (int, error) {
	return 0, nil
}

// CodexRolloutPaths is unused by the Claude link step.
func (claudeEvidenceIdentityReader) CodexRolloutPaths(context.Context, SessionID) ([]ResolvedPath, error) {
	return nil, nil
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

// ClaudeSessionLocationLookupCapable is implemented by an adapter that can use
// a store's session-location lookup to confirm a cross-run linking candidate
// before trusting it. The adapter factory signature carries only the
// filesystem, the git resolver, and the salt, so a caller attaches this
// capability after it builds the adapter — the same optional, type-asserted
// attach shape ClaudeEvidenceCaching already uses for the evidence cache.
type ClaudeSessionLocationLookupCapable interface {
	SetSessionLocationLookup(lookup SessionLocationLookup)
}

// AttachSessionLocationLookup gives adapter the session-location lookup when
// the adapter can use one. Adapters that need no such lookup ignore it.
func AttachSessionLocationLookup(adapter SourceAdapter, lookup SessionLocationLookup) {
	if adapter == nil || lookup == nil {
		return
	}
	if capable, ok := adapter.(ClaudeSessionLocationLookupCapable); ok {
		capable.SetSessionLocationLookup(lookup)
	}
}

// ClaudeLogicalParentLookupCapable is implemented by an adapter that can use a
// store's evidence-first logical-parent accessor to keep walking an ancestor
// chain past this discovery's own write batch. The adapter factory signature
// carries only the filesystem, the git resolver, and the salt, so a caller
// attaches this capability after it builds the adapter — the same optional,
// type-asserted attach shape ClaudeSessionLocationLookupCapable uses.
type ClaudeLogicalParentLookupCapable interface {
	SetLogicalParentLookup(lookup LogicalParentReader)
}

// AttachLogicalParentLookup gives adapter the evidence-first logical-parent
// accessor when the adapter can use one. Adapters that need no such lookup
// ignore it.
func AttachLogicalParentLookup(adapter SourceAdapter, lookup LogicalParentReader) {
	if adapter == nil || lookup == nil {
		return
	}
	if capable, ok := adapter.(ClaudeLogicalParentLookupCapable); ok {
		capable.SetLogicalParentLookup(lookup)
	}
}

// claudeSessionAlreadyStored reports whether sessionID is already recorded in
// the store from a prior discovery. It fails closed: no lookup attached, or
// any error, answers false, because a parent identifier must never point at a
// session that is neither in this write batch nor already stored — the
// store's own FK-orphan guard would otherwise silently drop the child at
// write time.
func (a *ClaudeAdapter) claudeSessionAlreadyStored(ctx context.Context, sessionID SessionID) bool {
	if a.locationLookup == nil {
		return false
	}
	hostSlug, _, err := a.locationLookup.LookupSessionLocation(ctx, sessionID)
	return err == nil && hostSlug != ""
}

// claudeStoredParent returns the logical parent the store already recorded for
// sessionID, if any, so cycle detection can keep walking an ancestor chain
// past this discovery's own write batch. It answers through the evidence-first
// accessor, so an ancestor whose relationship evidence names a parent is walked
// even while its availability cache is NULL. It fails closed: no lookup
// attached, any error, or an absent parent answers no parent found.
func (a *ClaudeAdapter) claudeStoredParent(ctx context.Context, sessionID SessionID) (SessionID, bool) {
	if a.logicalParentLookup == nil {
		return SessionID(""), false
	}
	parent, err := a.logicalParentLookup.LogicalParentForSession(ctx, sessionID)
	if err != nil || parent == nil || *parent == "" {
		return SessionID(""), false
	}
	return SessionID(*parent), true
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
